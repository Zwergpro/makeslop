# Architecture Fixes: Cancellation, Build Filesync, Registry Locking, Proxy Verification

## Overview

Four targeted fixes for architectural defects found during review. Each is independent and surgical; none changes the public CLI surface.

1. **Context cancellation + preflight timeouts** — `main()` uses `cmd.Execute()`, so `cmd.Context()` is `context.Background()`. Every `select { case <-ctx.Done() }` in `run`/`build`/`sidecar` is dead, and a black-hole `DOCKER_HOST` hangs forever (no deadline on `Ping`/`ImageInspect`). Wire `signal.NotifyContext` + `ExecuteContext`, and bound the preflight daemon calls with a timeout.
2. **Build filesync isolation** — `build()` syncs the Dockerfile's parent dir (`~/.makeslop`, which holds `.claude.json` credentials and the whole `workspaces/` cache tree) to the daemon. Stage just the Dockerfile in a temp dir and sync that.
3. **Registry lost-update race** — `settings.json` read-modify-write (`workspace.Init`, `init` re-stamp, `config set`, `migrate`) has no lock; concurrent invocations silently drop each other's writes. Add a POSIX advisory lock around the RMW sequences.
4. **Proxy `unix://` verification** — `HTTP_PROXY=unix:///sockets/proxy.sock` is non-standard; verify a real client in the target image honors it. Investigation + gated integration test + a documented decision. Any redesign is out of scope (deferred to Post-Completion).

Problem solved: the tool becomes interruptible and timeout-bounded, stops leaking credentials to the build daemon, stops losing workspace registrations under concurrency, and gains evidence that proxy mode actually functions.

## Context (from discovery)

- **Files/components involved:**
  - `cmd/makeslop/main.go` — `main()`, `runWithExitCode`, `runRun`, `init` RunE, `config set` RunE.
  - `internal/docker/preflight.go` — `CheckDaemon`, `ImageExists`.
  - `internal/docker/build.go` — `build()` filesync staging.
  - `internal/config/config.go` — `Load`/`Save`; add lock primitive.
  - `internal/workspace/workspace.go` — `Init` RMW.
  - `internal/docker/sidecar.go` / `spec.go` — proxy env var (`unix://`) under verification.
- **Related patterns found:**
  - `apiClient` seam + `SetClientForTest` + `FakeRunClient`/`FakeBuildClient` for daemon fakes.
  - Atomic temp-file+rename in `config.Save` / `WriteDockerfile` (durability already handled; only cross-process locking is missing).
  - Gated integration tests: `//go:build integration` + `MAKESLOP_DOCKER_IT=1` env gate (`build_integration_test.go`).
  - POSIX-only invariant (CLAUDE.md) — `syscall.Flock` is acceptable; no Windows path needed.
- **Dependencies identified:**
  - `golang.org/x/sys` v0.44.0 already present (indirect) — promotes to direct for `unix.Flock`, or use `syscall.Flock` directly (no new dep). Go 1.26.

## Development Approach

- **testing approach:** Regular (implement, then add/update tests in the same task).
- complete each task fully before moving to the next; small, focused changes.
- **CRITICAL: every task MUST include new/updated tests** (success + error/edge cases) as separate checklist items.
- **CRITICAL: all tests must pass before starting the next task.**
- run `go test ./...` after each change; maintain backward compatibility (no CLI/flag changes, no `settings.json` schema change → no `CurrentVersion`/`MigrationVersion` bump).

## Testing Strategy

- **unit tests:** required per task. Use the `apiClient` fake seam. For timeout behavior, add a fake whose `Ping`/`ImageInspect` blocks until `ctx` is cancelled, asserting the call returns `context.DeadlineExceeded` promptly.
- **integration tests:** Task 4 only — gated (`//go:build integration`, `MAKESLOP_DOCKER_IT=1`), exercises a real client through proxy mode.
- **no e2e/UI tests** in this project.

## Progress Tracking

- mark completed items `[x]` immediately; add ➕ for new tasks, ⚠️ for blockers.
- keep this file in sync if scope shifts (e.g. Task 4 forces a redesign decision).

## Solution Overview

- **Cancellation:** call `signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)` **inside `runWithExitCode`** and pass the resulting ctx to `cmd.ExecuteContext(ctx)`. This keeps `runWithExitCode`'s signature unchanged, so the ~21 existing `runWithExitCode(...)` call sites in `main_test.go` need no edits and every test exercises a live, cancellable context. Preflight daemon calls (`CheckDaemon`, the `ImageExists` calls in `runRun`/`status`) wrapped in `context.WithTimeout`. The interactive `Run` session and long `Build` keep the parent (signal-cancellable) context with no artificial deadline.
- **Filesync:** new unexported `stageDockerfile(path) (dir string, cleanup func(), err error)` in `build.go` that copies the Dockerfile into a fresh temp dir; `build()` syncs that dir under the `dockerfile` filesync key. Context dir stays the existing empty temp dir.
- **Locking:** new `config.WithLock(baseDir string, fn func() error) error` taking an exclusive `flock` on `<baseDir>/.settings.lock`. Callers that do Load→mutate→Save wrap the whole sequence in `WithLock`. POSIX-only, consistent with project invariant.
- **Proxy:** gated integration test that stands up a throwaway HTTP proxy (or asserts reachability through socat), runs the real image in proxy mode, and checks egress. Result documented in `docs/`; design change (if needed) deferred.

## Technical Details

- `WithLock` opens the lock file `O_CREATE|O_RDWR`, `0o600`, calls `syscall.Flock(fd, LOCK_EX)`, defers `LOCK_UN`+close, runs `fn`. The lock file is created under `baseDir` (already `MkdirAll`'d by callers/`Bootstrap`); creating it is harmless and never deleted.
- **flock semantics / no-nesting invariant:** flock locks bind to the *open file description*, not the process. Cross-goroutine concurrency is handled correctly — a second goroutine's `open()`+`LOCK_EX` on a fresh fd **blocks** until the first unlocks (this is what serializes concurrent `init`). But a **re-entrant** call on the *same goroutine* (holding fd1's lock, opening fd2 and `LOCK_EX`-ing it) blocks forever — a self-deadlock. Therefore `WithLock` must **never be nested**. The design relies on this: each Load→mutate→Save site takes its own short-lived lock *sequentially*, and no caller wraps another `WithLock`-protected call. Independent per-RMW locking is sufficient to prevent lost updates because every `fn` re-`Load`s fresh under the lock — there is no need (and it would be unsafe) to span multiple RMW sites with one lock.
- **`init` is two sequential RMW sites, not nested:** on a fresh seed, `ws.Init` performs one locked Load→Save (registers the workspace) and *returns* (releasing its lock); the `init` RunE then performs a second, separate locked Load→Save (stamps `MigratedVersion`). These run back-to-back, never nested, so no deadlock. The re-stamp is idempotent (re-loads current state, sets the same version), so an interleaving concurrent `init` between the two cannot cause a lost update.
- Preflight timeout constant: `const preflightTimeout = 10 * time.Second` (in `preflight.go` or `main.go`). Applied via `ctx, cancel := context.WithTimeout(parent, preflightTimeout); defer cancel()` at the daemon-ping and image-inspect call sites — NOT inside the long-lived `Run`/`Build`.
- `stageDockerfile` reads `assets`-written `Dockerfile` bytes via `os.ReadFile(path)` and writes `filepath.Join(tmp, "Dockerfile")`; `buildImageOptions` already uses `filepath.Base(o.DockerfilePath)` so the daemon-requested name stays `Dockerfile`. `.dockerignore` siblings are intentionally not staged (none expected in `~/.makeslop`; document this).

## What Goes Where

- **Implementation Steps** (checkboxes): code + tests + docs within this repo.
- **Post-Completion** (no checkboxes): manual proxy verification against the production image; any proxy transport redesign if Task 4 proves `unix://` unsupported; the deferred bottlenecks from the review (shared client, sidecar poll cost, scan caching).

## Implementation Steps

### Task 1: Signal-cancellable root context

**Files:**
- Modify: `cmd/makeslop/main.go`
- Modify: `cmd/makeslop/main_test.go`

Design note: keep `runWithExitCode`'s signature unchanged and create the signal context *inside* it. This avoids editing the ~21 `runWithExitCode(...)` call sites in `main_test.go` and makes every test run against a live cancellable context. `main()` stays thin.

- [x] in `runWithExitCode`, add `ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)`; `defer stop()`; call `cmd.ExecuteContext(ctx)` instead of `cmd.Execute()`.
- [x] confirm `main()` is unchanged except for any unused imports; the ~21 `main_test.go` call sites compile without edits (signature preserved).
- [x] write test: a command that consults `cmd.Context()` observes a non-`Background`, cancellable context (e.g. assert `cmd.Context() != context.Background()` via a tiny injected hook, or that `signal.NotifyContext` wiring is present). Keep the assertion lightweight — full signal delivery is not unit-testable here.
- [x] write test: a normal command (`version`) still succeeds (no regression from `ExecuteContext`).
- [x] run `go test ./...` — must pass before Task 2.

### Task 2: Preflight timeouts

**Files:**
- Modify: `internal/docker/preflight.go`
- Modify: `cmd/makeslop/main.go` (wrap `CheckDaemon` + `ImageExists` call sites in `runRun`)
- Modify: `cmd/makeslop/status.go` (wrap daemon + image + socat-image checks)
- Modify: `internal/docker/preflight_test.go`
- Modify: `internal/docker/testing.go` (add a blocking-Ping/blocking-Inspect fake option)

Design note: the existing fakes **discard** the context — `FakeRunClient.Ping(_ context.Context, ...)` (testing.go:241) and `ImageInspect(_ context.Context, ...)` (testing.go:256). To test the timeout, the fakes must actually honor `ctx`. The resulting unit test proves the wrapper *passes a deadline through*, not that the real SDK aborts a black-hole `DOCKER_HOST` (that is integration-only) — state this in the test comment so it isn't mistaken for end-to-end proof.

- [x] add `const preflightTimeout = 10 * time.Second` and apply `context.WithTimeout(parent, preflightTimeout)` (with `defer cancel()`) around `CheckDaemon` and `ImageExists` invocations on the preflight paths (`runRun`, `status`). Do NOT add a deadline to `docker.Run` or `docker.Build`.
- [x] thread `ctx` into the fakes: rename the discarded `_ context.Context` to `ctx` in `FakeRunClient`'s `Ping`/`ImageInspect` (and `FakeBuildClient` as needed) and add an opt-in "block until `ctx.Done()`" field so the fake returns `ctx.Err()` on cancellation.
- [x] write test: `CheckDaemon` returns `ErrDaemonUnreachable` wrapping `context.DeadlineExceeded` when the fake `Ping` blocks past a short timeout.
- [x] write test: `ImageExists` returns promptly with `context.DeadlineExceeded` when the fake `ImageInspect` blocks (and is NOT misreported as "image absent").
- [x] write test: happy-path preflight still succeeds well within the timeout (no regression).
- [x] run `go test ./...` — must pass before Task 3.

### Task 3: Isolate the Dockerfile build context

**Files:**
- Modify: `internal/docker/build.go`
- Modify: `internal/docker/build_test.go`

- [x] add unexported `stageDockerfile(path string) (dir string, cleanup func(), err error)` that creates a temp dir and copies the Dockerfile into it as `Dockerfile`.
- [x] in `build()`, replace `dockerfileDir := filepath.Dir(o.DockerfilePath)` with a call to `stageDockerfile`; sync the staged dir under `dockerui.DefaultLocalNameDockerfile`; `defer cleanup()`.
- [x] confirm `buildImageOptions` still sets `Dockerfile: filepath.Base(...)` == `"Dockerfile"` (no change expected).
- [x] write test: `stageDockerfile` produces a dir whose ONLY entry is `Dockerfile`, with byte-identical contents to the source (asserts no sibling leakage).
- [x] write test: `stageDockerfile` cleanup removes the temp dir; error path (unreadable source) returns an error without leaking a temp dir.
- [x] write/adjust `build()` test to confirm a build still succeeds with the staged dir (reuse existing `realDaemonFrame` fixture flow; capture stdout, assert vertex names appear).
- [x] run `go test ./...` — must pass before Task 4.

### Task 4: Lock settings.json read-modify-write

**Files:**
- Create: `internal/config/lock.go`
- Modify: `internal/workspace/workspace.go` (wrap `Init` RMW)
- Modify: `cmd/makeslop/main.go` (wrap `init` re-stamp + `config set` RMW)
- Modify: `internal/config/migrate.go` (wrap `Migrate` RMW)
- Create: `internal/config/lock_test.go`
- Modify: `internal/workspace/workspace_test.go`

Locking boundary (see Technical Details): each Load→mutate→Save site takes its **own** short-lived `WithLock` *sequentially*; `WithLock` is **never nested** (same-goroutine re-entry self-deadlocks on flock). The `init` RunE therefore relies on `ws.Init` locking internally and the re-stamp locking separately and sequentially — the RunE itself does NOT wrap both in an outer `WithLock`.

- [x] add `config.WithLock(baseDir string, fn func() error) error` using `syscall.Flock(fd, LOCK_EX)` on `<baseDir>/.settings.lock` (create `0o600`); `defer` unlock+close; `MkdirAll(baseDir)` first for safety. Document the no-nesting invariant in the doc comment.
- [x] wrap the Load→mutate→Save sequence inside `workspace.Init` with `config.WithLock` (internal — so concurrent `ws.Init` is safe).
- [x] wrap the fresh-seed re-stamp block (main.go:364-371) and the `config set` (Load→ConfigSet→Save, main.go:528-535) blocks with `config.WithLock` — each its own acquisition, not wrapping `ws.Init`.
- [x] wrap the `Migrate` Load→stamp→Save sequence (migrate.go) with `config.WithLock` so a concurrent `init`/`config set` cannot lose the `MigratedVersion` stamp.
- [x] verify no nesting: `ws.Init` is only invoked from the `init` RunE and is NOT called from within another `WithLock` block (audit `config.Save` callers: main.go:369, main.go:535, migrate.go, workspace.go).
- [x] write test: `WithLock` serializes — two goroutines incrementing a counter via Load/Save under the lock both persist (no lost update).
- [x] write test (concurrency): N concurrent `workspace.Init` calls for N distinct pwds under one `baseDir` all end registered in `settings.json` (the lost-update regression test).
- [x] write test: `WithLock` releases on `fn` error (panic/error path unlocks) and is re-acquirable afterward; back-to-back *sequential* acquisitions in one goroutine succeed (documents the no-nesting boundary).
- [x] run `go test ./...` — must pass before Task 5.

### Task 5: Verify the `unix://` HTTP proxy actually works

**Files:**
- Create: `internal/docker/proxy_integration_test.go`
- Modify: `docs/reference.md` (or `docs/architecture.md`) — record the verified result.

- [ ] write a gated integration test (`//go:build integration`, `MAKESLOP_DOCKER_IT=1`) that: starts the socat sidecar against a throwaway upstream HTTP proxy (or a local listener), runs a container in proxy mode (`--network none`, `HTTP_PROXY=unix:///sockets/proxy.sock`) executing the real client used by the image (e.g. `curl`/node) against a known URL, and asserts egress succeeds (or fails closed).
- [ ] run the integration test against the project image; capture the outcome (works / does-not-honor-unix://).
- [ ] document the verified result in `docs/`: if honored, note it as a supported invariant; if NOT honored, add a ⚠️ note and a Post-Completion follow-up describing the TCP-listener-on-internal-network alternative (do not implement here).
- [ ] write/keep a non-gated unit assertion that `BuildSpec` emits the `unix://` env vars + `--network none` when `ProxySocketVolume` is set (guards the spec contract regardless of client support).
- [ ] run `go test ./...` (unit) and, where a daemon is available, `MAKESLOP_DOCKER_IT=1 go test -tags integration ./internal/docker/` — must pass before Task 6.

### Task 6: Verify acceptance criteria

- [ ] verify Ctrl-C / SIGTERM interrupts `build`/`status`/preflight (signal context live) and a blocking daemon times out within `preflightTimeout`.
- [ ] verify a `build` no longer exposes `~/.makeslop` contents to the daemon (only `Dockerfile` staged).
- [ ] verify concurrent `init` registrations are not lost.
- [ ] verify the proxy verification result is documented.
- [ ] run full suite: `go test ./...`.
- [ ] run integration suite where possible: `MAKESLOP_DOCKER_IT=1 go test -tags integration ./internal/docker/`.

### Task 7: [Final] Update documentation

- [ ] update `CLAUDE.md`: note the signal-cancellable root context + preflight timeout, the Dockerfile-staging behavior, and the `config.WithLock` RMW invariant.
- [ ] update `docs/reference.md`/`docs/architecture.md` as needed (cancellation, build isolation, proxy verification result).
- [ ] move this plan to `docs/plans/completed/`.

## Post-Completion

*Items requiring manual intervention or external systems — informational only.*

**Manual verification:**
- Run `makeslop run --proxy <ip:port>` against the real production image with a live agent client to confirm end-to-end egress through socat (beyond the integration test).
- Confirm `Ctrl-C` during a long `makeslop build` aborts cleanly with no orphaned BuildKit session.

**Deferred / external (out of scope here):**
- If Task 5 proves `HTTP_PROXY=unix://` is not honored by the target client: redesign proxy transport to a shared internal Docker network with the sidecar on `TCP-LISTEN`, `HTTP_PROXY=http://sidecar:port` (keeps the app off the host bridge while using a standard proxy URL). Larger change — separate plan.
- Remaining review bottlenecks (separate plans): single shared Docker client instead of 5 constructions per `run`; replace the 4-call×50-poll sidecar readiness loop with one blocking exec/connect-retry; cache/parallelize the per-run secret scan; move `testing.go` fakes out of the production binary.
