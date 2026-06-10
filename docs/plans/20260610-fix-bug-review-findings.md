# Fix bug-review findings (6 items)

## Overview

Fix all six findings from the 2026-06-10 bug review:

1. **(medium)** `validatePatterns` accepts path-style scan patterns (`secrets/*.pem`, `**/*.env`) that
   can never match — `security.Scan` matches basenames only, so masking is silently lost.
2. **(low)** Dangling `.makeslop.yaml` symlink: `Scaffold` treats `EEXIST` as success and `Load`
   follows the symlink to `ENOENT` → treated as "no config" → project silently runs with no scan
   patterns.
3. **(low)** Stdin EOF is never propagated to the container — no `att.CloseWrite()` after the stdin
   copy ends, so a container reading stdin to EOF hangs (bites `WithStreams` consumers).
4. **(low)** `ContainerWait` SDK goroutine leak on the start-failure path: the moby client's
   `resultC` is unbuffered with no `ctx.Done` escape on the final send; when `ContainerStart`
   fails, nobody receives and the goroutine blocks forever holding its connection.
5. **(low)** `wr.Error` mid-session: the drain blocks until the attach stream EOFs (i.e. until the
   container exits on its own), the wait error is deferred until then, and on return the deferred
   force-remove does not fire (`startedCleanly && ctx.Err() == nil`) — container left running.
6. **(low)** SIGWINCH resize goroutine is never joined: an in-flight `ContainerResize` can still be
   executing after `runContainer` returns, racing the caller's `d.Close()`.

**Decisions made during planning (user-confirmed):**
- Finding 1: **hard error** in `projectconfig.Load` (matches fail-loud philosophy and the sibling
  `validateSkipDirs`, which already rejects `/`).
- Finding 2: **hard error in both** `Scaffold` and `Load`. A symlinked `.makeslop.yaml` — dangling
  or not — is rejected, consistent with the sandbox-policy stance (`ProtectProjectConfig` already
  refuses to protect a symlinked config). This is an explicit behavior change for users who
  deliberately symlink their project config; documented below.
- Testing: **regular** (code first, then tests, per task).

## Context (from discovery)

- `internal/projectconfig/projectconfig.go` — `validatePatterns` (rejects only empty/invalid-glob
  today), `validateSkipDirs` (the model: already rejects `/`), `Scaffold` (`O_CREATE|O_EXCL`,
  `fs.ErrExist` → `return nil`), `Load` (`os.ReadFile`, `fs.ErrNotExist` → empty defaults).
- `internal/security/security.go:68-70` — `Scan` matches patterns against `d.Name()` (basename).
- `internal/docker/run.go` — `runContainer` lifecycle; stdin goroutine at ~line 248; wait
  registration at line 189 (`cli.ContainerWait(ctx, …)`); `wr.Error` case at ~line 255; SIGWINCH
  goroutine at ~lines 215-232; deferred force-remove guarded by `startedCleanly`/`ctx.Err()` at
  ~lines 150-155.
- moby v0.4.1 facts (verified in module cache): `ContainerAttachResult` embeds `HijackedResponse`;
  `att.CloseWrite()` exists and no-ops when the conn doesn't implement `CloseWriter`; the SDK wait
  goroutine's HTTP body read is ctx-bound, so cancelling the wait context drains it into the
  buffered `errC`.
- Test fakes: `internal/docker/run_test.go` (`fakeClient`, records call order, scripts attach
  payload/EOF), `internal/docker/fakes_test.go`. `internal/projectconfig/projectconfig_test.go`
  has table tests for validators, `Scaffold`, and `Load`.
- Test invocation in this sandbox needs `GOTMPDIR=/workspace/makeslop-612d19/.gotmp` (exec is
  blocked in `/tmp`).

## Development Approach

- **testing approach**: Regular (code first, then tests, per task)
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
  - tests are not optional - they are a required part of the checklist
  - cover both success and error scenarios
- **CRITICAL: all tests must pass before starting next task** - no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- run tests after each change: `GOTMPDIR=$PWD/.gotmp go test ./...`
- maintain backward compatibility *except* the two user-approved fail-loud behavior changes
  (findings 1 and 2), which are deliberate

## Testing Strategy

- **unit tests**: required for every task; follow the repo's table-test style. All fakes stay in
  `_test.go` files (never compiled into the production binary — project invariant).
- no e2e framework in this repo; the gated integration test
  (`MAKESLOP_DOCKER_IT=1 go test -tags integration ./internal/docker/`) is unaffected and not run
  here (no daemon in sandbox).
- run with `-race` for the `internal/docker` lifecycle tasks (3-6).

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- keep plan in sync with actual work done

## Solution Overview

Two fail-loud validation fixes in `internal/projectconfig` (tasks 1-2), then four contained
lifecycle fixes in `internal/docker/run.go` (tasks 3-6). Each lifecycle fix is independent; they
are ordered stdin-half-close → wait-cancel → wr.Error-reap → SIGWINCH-join so the `wr.Error` task
can reuse the wait-context plumbing from task 4 if needed. No public API changes; no
`CurrentVersion`/`MigrationVersion` bump (no `Settings` field change, no Dockerfile change).

## Technical Details

### Task 1 — reject `/` in scan patterns
In `validatePatterns`, after the empty/syntax checks, add:
`if strings.Contains(p, "/") { return nil, fmt.Errorf("projectconfig: scan pattern %q contains a path separator — patterns match basenames only", p) }`.
(`strings` is already imported; POSIX-only project, so `/` is the only separator.)

### Task 2 — reject symlinked `.makeslop.yaml`
- `Load`: `os.Lstat(path)` before `os.ReadFile`. `fs.ErrNotExist` → current empty-defaults path.
  Lstat succeeds and `mode&fs.ModeSymlink != 0` → error
  `"projectconfig: %s is a symlink — the project config must be a regular file"`. Otherwise fall
  through to `ReadFile` (directory case keeps failing loud via EISDIR as today).
- `Scaffold`: in the `fs.ErrExist` branch, `os.Lstat(path)`; if symlink → same-shaped error
  (init now fails loud instead of reporting success on a broken setup); otherwise keep
  `return nil` (idempotency for regular files preserved).

### Task 3 — propagate stdin EOF (`att.CloseWrite`)
In the stdin copy goroutine, after `io.Copy(att.Conn, ps.handle)` returns and before
`close(stdinDone)`, call `_ = att.CloseWrite()`. On the join paths (handle closed to unblock the
read) this fires just before `att.Conn.Close` — harmless, and ordering is safe because the join
(`<-stdinDone`) completes before the deferred conn close. `HijackedResponse.CloseWrite` no-ops on
conns without `CloseWrite`, so no fake changes are strictly required to keep existing tests green.
On the documented fallback path (`ps.closer == nil`, goroutine leaked by design) `CloseWrite` may
fire after `att.Conn.Close`; that yields a benign discarded "use of closed connection" error, not
a panic — the leak path's behavior is otherwise unchanged.

### Task 4 — cancel the registered wait on early-return paths
Derive `waitCtx, waitCancel := context.WithCancel(ctx)` immediately before the
`cli.ContainerWait` call; pass `waitCtx`; add `defer waitCancel()`. Scope: the cancel reaps the
SDK goroutine only while it is blocked in the ctx-bound body read — exactly the start-failure
case this finding targets (the container never started, so `/wait next-exit` has produced no
body). It cannot unblock a goroutine already parked on the unbuffered `resultC` send; that
narrower residual is accepted. On normal paths the result is received before the deferred cancel
runs, so behavior is unchanged.

### Task 5 — reap the container and surface the error promptly on `wr.Error`
At the top of the `case err := <-wr.Error:` branch, force-remove inline:
`_, _ = cli.ContainerRemove(context.Background(), id, moby.ContainerRemoveOptions{Force: true})`
*before* the `outputDone` drain. Removal kills the container → attach stream EOFs → drain
completes → the wait error returns promptly instead of blocking until the container exits on its
own, and no running container is left behind. Trade-off (accepted): a transient `/wait` drop now
ends the session instead of letting it limp along with a lost exit code — matching docker CLI
behavior of erroring out the session. The deferred remove may fire a second time on the
cancellation path; double-remove of a gone container is an ignored error today (`_, _ =`), so no
flag dance is needed — but verify the fake tolerates a second remove call.

**Fake wiring (required, not optional):** in production the remove → attach-EOF → drain chain
holds because the daemon kills the container; in `fakeClient` it does NOT — `ContainerRemove`
today only sets `removed = true` and never closes the attach pipe writer, so the new
open-stream test would hang forever on the drain (no `ctx.Done()` escape under
`context.Background()`). `fakeClient.ContainerRemove` must close the attach pipe writer (store
`pw`/a close-channel on the struct) so the post-remove drain can complete. The existing
`TestRun_WaitErrorChannel` only passes today because its empty `attachPayload` closes `pw`
immediately — the new test exercises a genuinely different path.

### Task 6 — join the SIGWINCH goroutine
Add `resizeDone := make(chan struct{})`; the goroutine does `defer close(resizeDone)` around its
`for range winchCh` loop. Change the teardown defer to
`signal.Stop(winchCh); close(winchCh); <-resizeDone`. `signal.Stop` guarantees no further sends,
so the close is race-free (unchanged) and the receive guarantees no `ContainerResize` is in
flight after `runContainer` returns.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): all code, test, and doc changes below.
- **Post-Completion** (no checkboxes): manual interactive-TTY verification against a live daemon.

## Implementation Steps

### Task 1: Reject path separators in exclude.scan.patterns

**Files:**
- Modify: `internal/projectconfig/projectconfig.go`
- Modify: `internal/projectconfig/projectconfig_test.go`

- [ ] add the `/`-rejection check to `validatePatterns` with a message that explains patterns are
      basename globs
- [ ] write table-test cases: `secrets/*.pem`, `**/*.env`, `a/b` all rejected with the new message
- [ ] write a `Load`-level test: a `.makeslop.yaml` with a path-style pattern fails loud
- [ ] confirm existing valid-pattern cases (e.g. `.env.*`, `*.pem`) still pass unchanged
- [ ] run `GOTMPDIR=$PWD/.gotmp go test ./internal/projectconfig/` — must pass before task 2

### Task 2: Fail loud on symlinked .makeslop.yaml in Scaffold and Load

**Files:**
- Modify: `internal/projectconfig/projectconfig.go`
- Modify: `internal/projectconfig/projectconfig_test.go`

- [ ] `Load`: Lstat before ReadFile; symlink (dangling or live) → hard error; ENOENT keeps
      returning empty defaults; directory keeps failing via ReadFile
- [ ] `Scaffold`: on `fs.ErrExist`, Lstat; symlink → hard error; regular file → success (unchanged)
- [ ] write tests: dangling symlink → `Scaffold` errors AND `Load` errors; live symlink to a valid
      config file → both error; existing regular file → `Scaffold` still returns nil (idempotency)
- [ ] update doc comments on `Scaffold` ("Idempotent: EEXIST is success") and `Load` to state the
      regular-file requirement
- [ ] run `GOTMPDIR=$PWD/.gotmp go test ./internal/projectconfig/ ./internal/cli/` — must pass
      before task 3 (cli init tests exercise Scaffold)

### Task 3: Propagate stdin EOF to the container via att.CloseWrite

**Files:**
- Modify: `internal/docker/run.go`
- Modify: `internal/docker/run_test.go`

- [ ] call `_ = att.CloseWrite()` in the stdin goroutine after `io.Copy` returns, before
      `close(stdinDone)`; update the goroutine comment
- [ ] add a recording conn to `fakeClient`'s attach result: a `net.Conn` wrapper exposing
      `CloseWrite() error` that records the call (keep it in `run_test.go`)
- [ ] write test: injected stdin reader (pipe) hits EOF → `CloseWrite` was called before `Run`
      returned
- [ ] write test: normal join path (closer-based) → `Run` returns cleanly and no panic from
      `CloseWrite` after conn close
- [ ] run `GOTMPDIR=$PWD/.gotmp go test -race ./internal/docker/` — must pass before task 4

### Task 4: Cancel the registered ContainerWait on early-return paths

**Files:**
- Modify: `internal/docker/run.go`
- Modify: `internal/docker/run_test.go`

- [ ] wrap the `ContainerWait` call in a derived cancellable context with `defer waitCancel()`
- [ ] change `fakeClient.ContainerWait`'s signature to capture its context (currently
      `_ context.Context`) and record it on the struct; keep `blockingWaitClient`'s override
      (which already names its ctx) consistent
- [ ] write test: `ContainerStart` scripted to fail → `Run` returns the start error AND the
      recorded wait context is cancelled (`ctx.Err() != nil`) by the time `Run` returns
- [ ] write test: happy path → exit code still mapped correctly (wait result received before
      cancellation; no behavior change)
- [ ] run `GOTMPDIR=$PWD/.gotmp go test -race ./internal/docker/` — must pass before task 5

### Task 5: Reap the container and return promptly on wr.Error

**Files:**
- Modify: `internal/docker/run.go`
- Modify: `internal/docker/run_test.go`

- [ ] insert the inline force-remove (Background ctx, `Force: true`) at the top of the `wr.Error`
      case, before the `outputDone` drain; update the case comment with the rationale
- [ ] wire `fakeClient.ContainerRemove` to close the attach pipe writer so the post-remove drain
      completes (see Technical Details — without this the new open-stream test hangs)
- [ ] verify/extend `fakeClient` to tolerate a second `ContainerRemove` call (deferred remove on
      the cancellation path)
- [ ] write test: scripted wait error while the attach stream stays open until `ContainerRemove`
      is observed → `Run` returns the "container wait:" error without hanging, and remove was
      called exactly with `Force: true`
- [ ] write test: call-order assertion — remove precedes `Run`'s return; scripted attach output
      delivered before the error is still flushed to the injected stdout (drain still happens)
- [ ] run `GOTMPDIR=$PWD/.gotmp go test -race ./internal/docker/` — must pass before task 6

### Task 6: Join the SIGWINCH resize goroutine

**Files:**
- Modify: `internal/docker/run.go`
- Modify: `internal/docker/run_test.go`

- [ ] add `resizeDone` channel closed by the resize goroutine on exit; teardown defer becomes
      stop → close → `<-resizeDone`
- [ ] add `ContainerResize` to `fakeClient`'s `callOrder` recording (it is NOT recorded today —
      this is a required step, not conditional)
- [ ] write test (POSIX-only, use the local `skipNonPOSIX` helper): run a scripted container while
      sending `syscall.Kill(os.Getpid(), syscall.SIGWINCH)` in a loop from the test; assert `Run`
      returns cleanly under `-race` and `fakeClient` records no `ContainerResize` call after the
      final lifecycle call
- [ ] run `GOTMPDIR=$PWD/.gotmp go test -race -count=2 ./internal/docker/` — must pass before
      task 7

### Task 7: Verify acceptance criteria

- [ ] all six findings addressed: re-read each finding against the new code and confirm the
      failure mode is closed
- [ ] edge cases verified: empty patterns still short-circuit `Scan`; missing `.makeslop.yaml`
      still yields defaults; `--dry-run` path untouched; fallback (non-joinable stdin) path
      untouched
- [ ] run full suite: `GOTMPDIR=$PWD/.gotmp go test -race ./...`
- [ ] run `go vet ./...`
- [ ] confirm no `_test.go` helper leaked into production files (project invariant)

### Task 8: [Final] Update documentation

**Files:**
- Modify: `CLAUDE.md`
- Modify: `docs/reference.md` (if it documents `exclude.scan.patterns` / `.makeslop.yaml`)
- Modify: `docs/security.md` (if it states the no-leak contract / symlink stance)

- [ ] CLAUDE.md: scan-filters section — patterns reject `/` (basename-only contract now enforced);
      projectconfig section — `Scaffold`/`Load` reject symlinked config; Run-lifecycle section —
      stdin half-close, wait-context cancel, `wr.Error` inline reap, SIGWINCH join
- [ ] docs: document the two user-facing behavior changes (path-style patterns now error;
      symlinked `.makeslop.yaml` now rejected) and the migration hint (move the pattern into
      basename form, or replace the symlink with a real file)
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

**Manual verification** (requires a live Docker daemon + interactive TTY — not available in this
sandbox):
- interactive `makeslop run` session: TUI app (e.g. `htop`) under heavy redraw, window resize
  during the session, Ctrl-D/exit — confirm no freeze, terminal restored, container reaped
  (`docker ps -a` empty afterwards)
- simulate a `/wait` drop (e.g. restart the daemon mid-session) — confirm prompt error and no
  orphaned container

**External system updates:** none — no consuming projects, no version-constant bumps required.
