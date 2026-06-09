# Docker Package Struct-DI Refactor

## Overview
Refactor `internal/docker` from a function-style API with package-level mutable
test-seams into struct-based constructor dependency injection (DI). This is a
**behavior-preserving structural refactor** — no observable behavior changes.

Problem it solves: the current design relies on three mutable globals
(`newClientFn`, `ttyCheck`, `termMakeRaw`) plus a fourth in `cmd` (`onContextForTest`),
and ships test helpers (`testing.go`) in the production binary. Applying the
project's Go refactoring principles literally (explicit dependencies, no global
mutable state, no test helpers in prod) requires moving to constructor DI and
faking at the consumer boundary.

Integration: `cmd/makeslop` stops calling `docker.Run/Build/CheckDaemon/ImageExists`
as package functions and instead holds a `*docker.Docker` (constructed via
`docker.New()`), accessed through small consumer-side interfaces defined in
`package main`. Tests inject in-package fakes at that boundary; `internal/docker`
keeps its own fakes in `_test.go` files only.

## Context (from discovery)
Files/components involved:
- `internal/docker/{client,run,build,preflight,spec}.go` + `testing.go` (deleted)
- `internal/docker/{client,run,build,preflight,spec}_test.go` + new `fakes_test.go`
- `cmd/makeslop/{main,status}.go` + `{main,status}_test.go`
- `CLAUDE.md` (affected sections)

Related patterns found:
- `apiClient` is an unexported 11-method interface adapting `*moby.Client`, guarded
  by `var _ apiClient = (*moby.Client)(nil)`. It is a genuine test seam.
- Pure/impure split is sacred: `spec.go` is pure (`BuildSpec`, `Spec`,
  `ContainerConfig`, `HostConfig`) and stays untouched.
- `WithPreflightTimeout` is a pure helper (no client) and stays a package function.

Dependencies identified (verified counts):
- `cmd/makeslop/main_test.go`: 8 `SetClientForTest` sites, 8
  `NewFakeRunClient`/`NewFakeBuildClient`, 5 `SetTTYCheckForTest`/`SetTermMakeRawForTest`,
  plus the `onContextForTest` global (~3811–3828). `status_test.go`: 2 sites
  (`installFakeStatusClient`). These all move to boundary fakes.
- `internal/docker` tests use the globals/unexported helpers directly:
  `preflight_test.go` (~56 refs to `SetClientForTest`/`CheckDaemon`/`ImageExists`),
  `client_test.go` (~10), and crucially **`run_test.go` and `build_test.go` call the
  unexported `run(ctx, cli, s)` / `build(...)` helpers directly** (e.g. `run_test.go:135,
  261, 282, 329`; `build_test.go:196, 697`) — those helpers are deleted in Task 5, so
  these files carry the heaviest rewrite, not the cmd tests.
- ⚠️ **No injection seam exists today.** `newRootCmd(baseDir)` (`main.go:229`)
  hardcodes its collaborators; `runRun`/`runStatus` take plain value params. The
  reason `SetClientForTest` exists at all is that there is no parameter to thread a
  fake through the cobra tree. Creating that seam is the keystone of this refactor
  (Task 3) — model it on the existing `newStatusCmd(ws, baseDir, ttyPred isTTYFunc)`
  precedent (`status.go:338`).

## Development Approach
- **Testing approach**: Regular, refactor-safe variant. This is behavior-preserving,
  so the **existing test suite is the safety net**. Each task migrates its tests in
  lockstep with the code so the suite stays green; no new behavior is introduced,
  so "new tests" means *equivalent reworked tests*, not additional coverage.
- Complete each task fully before moving to the next.
- Make small, focused changes; keep the tree compiling where possible (tasks 1–4
  add the new API alongside the old; task 5 removes the old).
- **CRITICAL: all tests must pass before starting the next task** — no exceptions.
- **CRITICAL: update this plan file when scope changes during implementation.**
- Maintain backward compatibility *within the repo* — there is no external API
  surface (`internal/`), so the only consumers are `cmd/` and tests.

## Testing Strategy
- **Unit tests**: every task keeps `go test ./internal/docker/` and
  `go test ./cmd/makeslop/` green. Tests are migrated, not added wholesale, since
  behavior is unchanged.
- **Drift-guard test**: the `Args()`/`ContainerConfig()` drift-guard in `spec_test.go`
  must remain passing (spec.go is untouched, so this is a tripwire only).
- **Integration test**: `internal/docker/build_integration_test.go`
  (`-tags integration`, `MAKESLOP_DOCKER_IT=1`) must still **compile** under the new
  API. Update its construction to `New(...)` if it references the old `build()` seam.
- **No e2e harness** in this project (CLI tool, no UI) — N/A.
- Verification commands per task: `go build ./...`, `go vet ./...`,
  `go test ./internal/docker/`, `go test ./cmd/makeslop/`, and a compile check of
  the integration tag: `go test -tags integration -run XXX ./internal/docker/`.

## Progress Tracking
- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope

## Solution Overview
**New `internal/docker` shape:**
```go
type Docker struct {
    client  apiClient
    isTTY   func() bool
    makeRaw func(fd int) (*term.State, error)
}

func New(opts ...Option) (*Docker, error) // defaults: real client + real tty/raw

type Option func(*Docker)
func WithClient(c apiClient) Option        // used by docker's own _test.go
func WithTTYCheck(fn func() bool) Option
func WithRawMode(fn func(int) (*term.State, error)) Option

func (d *Docker) Run(ctx context.Context, s Spec) error
func (d *Docker) Build(ctx context.Context, o BuildOptions, out, errw io.Writer) error
func (d *Docker) CheckDaemon(ctx context.Context) error
func (d *Docker) ImageExists(ctx context.Context, image string) (bool, error)
```
- `apiClient` **stays internal and intact** (11-method moby SDK adapter; genuine
  seam — not split into single-method interfaces, which would be cargo-culting).
- Globals `newClientFn`, `ttyCheck`, `termMakeRaw` **deleted**. `testing.go`
  **deleted**; fakes move to `fakes_test.go` (package `docker`, never in prod).
- `WithPreflightTimeout`, `ErrNoTTY`, `ExitError`, `ErrDaemonUnreachable`,
  `BuildSpec`, `Spec`, `Options`, `BuildOptions` stay as package-level symbols.

**New `cmd/makeslop` shape:** small unexported consumer-side interfaces; one
`*docker.Docker` satisfies all four in production; tests inject boundary fakes.

Key design decisions / rationale:
- **Functional options** because `Docker` has >3 configurable fields (prompt rule).
- **`WithClient(apiClient)`** keeps `apiClient` unexported: only same-package
  `_test.go` files call it, so no export needed (satisfies "don't export
  internal-only interfaces").
- **Consumer-side interfaces in `cmd`** are where the prompt's "small, single-method,
  `-er`, point-of-use" rule actually applies — not on the SDK adapter.
- **`onContextForTest` global** replaced by an explicit observer field on the cmd
  wiring struct, set by tests.

## Technical Details
- `New` default path replicates old `newClient()`: `moby.New(moby.FromEnv)`, real
  `isTTY` (`term.IsTerminal` on stdin+stdout), real `termMakeRaw` (`term.MakeRaw`).
- Methods read `d.client`/`d.isTTY`/`d.makeRaw` instead of globals/params. The old
  unexported `run(ctx, cli, s)` / `build(ctx, cli, o, ...)` bodies become the method
  bodies (move the client-construction + Close lifecycle into the method, since the
  Docker struct owns the client for its lifetime — see ⚠️ Client lifecycle below).
- ⚠️ **Client lifecycle — USE-AFTER-CLOSE HAZARD**: today each package func / helper
  constructs a client and `Close()`s it at the end of the call (`run.go:59`,
  `build.go:140`, `preflight.go:47`, `preflight.go:71`). With a long-lived
  `*docker.Docker`:
  - **Chosen approach**: `Docker` holds one client for its lifetime; methods do
    **not** `Close()` per-call. Add `func (d *Docker) Close() error` and have `cmd`
    `defer d.Close()` once after construction.
  - **CRITICAL**: the per-call `defer cli.Close()` must be removed from **all four**
    method bodies (`Run`, `Build`, `CheckDaemon`, `ImageExists`). In production
    `runRun` calls `CheckDaemon` → `ImageExists` → `Run` on the **same** shared client
    (`main.go:173/188/202`); if any method retains its `Close()`, the next call hits a
    closed connection.
  - **The test suite CANNOT catch this**: fakes' `Close()` is a no-op, so the suite
    stays green while production breaks. This is the one place the "existing suite is
    the safety net" premise fails — it MUST be covered by the live smoke test
    (Post-Completion: preflight-then-run on a shared client).
  - This changes *when* Close happens but not observable behavior (single short-lived
    process per invocation). The preflight helpers previously built+closed their own
    client per call — now they share `d.client` (CLAUDE.md "Shared preflight helpers"
    no longer builds two clients per status run).
- **Two distinct TTY notions** (do not conflate): `docker`'s `isTTY` checks stdin+stdout
  and gates `Run` (`run.go:20`); `cmd`'s `isTTYFunc`/`defaultIsTTY` is writer-based and
  gates status glyphs (`status.go`, `main.go:505`). They stay separate.
- cmd consumer interfaces (package `main`):
  ```go
  type containerRunner interface { Run(ctx context.Context, s docker.Spec) error }
  type imageBuilder    interface { Build(ctx context.Context, o docker.BuildOptions, out, errw io.Writer) error }
  type daemonChecker   interface { CheckDaemon(ctx context.Context) error }
  type imageChecker    interface { ImageExists(ctx context.Context, image string) (bool, error) }
  ```
- Processing flow unchanged: preflight (timeout-wrapped CheckDaemon/ImageExists) →
  Run/Build. Exit-code contract via `*docker.ExitError` unchanged.

## What Goes Where
- **Implementation Steps** (`[ ]`): all code + test + doc changes in this repo.
- **Post-Completion** (no checkboxes): manual smoke test against a live daemon;
  confirm `make`/CI lint config (if any) still passes.

## Implementation Steps

### Task 1: Introduce `Docker` struct, `New`, options, and methods (additive)

**Files:**
- Create: `internal/docker/docker.go`
- Modify: `internal/docker/run.go`, `internal/docker/build.go`, `internal/docker/preflight.go`, `internal/docker/client.go`

- [x] create `internal/docker/docker.go` with `Docker` struct (`client`, `isTTY`,
      `makeRaw`), `New(...Option) (*Docker, error)`, `Option func(*Docker)`,
      `WithClient`/`WithTTYCheck`/`WithRawMode`, and `(*Docker) Close() error`
- [x] move client construction logic from `client.go` into `New`'s default (keep
      `apiClient`, the compile-time assertion, and `newClient`; remove `newClientFn`
      only in Task 5)
- [x] add `(d *Docker) Run(ctx, Spec) error` mirroring the existing `run()` body but
      using `d.client`/`d.isTTY`/`d.makeRaw`; **remove the per-call `defer cli.Close()`**
- [x] add `(d *Docker) Build(ctx, BuildOptions, out, errw) error` mirroring `build()`;
      **remove the per-call `defer cli.Close()`** (`build.go:140`)
- [x] add `(d *Docker) CheckDaemon(ctx) error` and `(d *Docker) ImageExists(ctx, image) (bool, error)`
      using `d.client`; **remove the per-call `defer c.Close()`** from both (`preflight.go:47/71`)
- [x] ⚠️ verify NO method body retains a per-call `Close()` (use-after-close hazard;
      fakes cannot detect it — see Technical Details / Client lifecycle)
- [x] relocate the `golang.org/x/term` import to `docker.go` (currently in `run.go` +
      `testing.go`); fold real `isTTY` (stdin+stdout) and `termMakeRaw` defaults into `New`
- [x] keep the old package funcs (`Run`/`Build`/`CheckDaemon`/`ImageExists`) and
      globals temporarily so the tree still compiles
- [x] run `go build ./...` and `go vet ./...` — must pass before next task

### Task 2: Migrate `internal/docker` tests to `New(WithClient(...))`

**Files:**
- Create: `internal/docker/fakes_test.go`
- Modify: `internal/docker/client_test.go`, `internal/docker/preflight_test.go`, `internal/docker/run_test.go`, `internal/docker/build_test.go`, `internal/docker/build_integration_test.go`

- [x] move `noopClient`, `FakeRunClient`(+`NewFakeRunClient`), `FakeBuildClient`
      (+`NewFakeBuildClient`), and `SkipNonPOSIX` from `testing.go` into
      `fakes_test.go` (package `docker`); keep them exported-name or unexport as the
      tests require (same package → unexport is fine)
      Note: type definitions stay in testing.go (used by cmd tests via exported names);
      fakes_test.go provides constructor helpers (newDockerWithClient, noopMakeRaw,
      alwaysTTY, neverTTY). Full move deferred to Task 5 when testing.go is deleted.
- [x] rewrite `client_test.go` to construct `New(WithClient(fake))` instead of
      `SetClientForTest`
      Note: client_test.go didn't use SetClientForTest; it tests fake types directly.
      No changes needed — already correct for this task.
- [x] rewrite `preflight_test.go` to call `d.CheckDaemon`/`d.ImageExists` on a
      `New(WithClient(fake))` instance (incl. BlockPing/BlockImageInspect timeout cases)
- [x] ⚠️ rewrite `run_test.go`/`build_test.go` — these call the **unexported
      `run(ctx, cli, s)` / `build(...)` helpers directly** (`run_test.go:135, 261, 282,
      329`; `build_test.go:196, 697`), which are deleted in Task 5. Convert each to
      `New(WithClient(fake), WithTTYCheck(...), WithRawMode(...))` + method call. This
      is the heaviest single-file rewrite in the refactor.
- [x] update `build_integration_test.go:44` (`Build(ctx, o, io.Discard, io.Discard)`)
      to the new API; confirm it references no symbol deleted in Task 5 other than the
      package func it is moving off of (must compile under `-tags integration`)
- [x] run `go test ./internal/docker/` and
      `go test -tags integration -run XXX ./internal/docker/` (compile check) — must pass

### Task 3: Add cmd consumer interfaces + create the injection seam (KEYSTONE)

**Files:**
- Modify: `cmd/makeslop/main.go`, `cmd/makeslop/status.go`

⚠️ No injection seam exists today: `newRootCmd(baseDir)` (`main.go:229`) hardcodes
collaborators and `runRun`/`runStatus` take plain value params. This task **creates**
the seam, modelled on the existing `newStatusCmd(ws, baseDir, ttyPred isTTYFunc)`
pattern (`status.go:338`). Without it, Task 4 cannot inject fakes.

- [x] define unexported consumer interfaces in `package main`: `containerRunner`,
      `imageBuilder`, `daemonChecker`, `imageChecker` (a single `*docker.Docker`
      satisfies all four in production)
- [x] decide the seam shape: bundle the four into one small struct (e.g.
      `type dockerDeps struct { runner containerRunner; builder imageBuilder;
      daemon daemonChecker; image imageChecker }`) to avoid threading four params
- [x] add a constructor seam: `newRootCmd(baseDir)` keeps its public signature and
      internally builds production deps via `docker.New()`; add a test-only variant
      (e.g. `newRootCmdWithDeps(baseDir, deps dockerDeps, ...)`) that injects fakes —
      mirroring how `newStatusCmd` already takes an injectable `ttyPred`
- [x] thread `dockerDeps` into `runRun` (`main.go:103`), `runStatus` (`status.go:131`),
      and the build command's `RunE` (`main.go:~403`) — replacing direct
      `docker.Run/Build/CheckDaemon/ImageExists` calls with method calls through the
      interfaces (keep `docker.BuildSpec`, `docker.WithPreflightTimeout`,
      `docker.Options`, `docker.BuildOptions` as package-level — unchanged)
- [x] construct the production `*docker.Docker` via `docker.New()` once at the wiring
      point and `defer d.Close()` (single Close — see lifecycle note)
      Note: `docker.New()` now delegates through `newClientFn`/`ttyCheck`/`termMakeRaw`
      globals so existing `SetClientForTest`/`SetTTYCheckForTest` seams still work
      during the Task 3→4 transition. `dockerNewErrStub` handles the (rare) case where
      `docker.New()` itself fails. `newStatusCmd` updated to accept `deps dockerDeps`.
- [x] keep `onContextForTest` for now (removed in Task 4)
- [x] run `go build ./...` and `go vet ./...` — must pass before next task

### Task 4: Migrate cmd tests to boundary fakes; remove `onContextForTest` global

**Files:**
- Modify: `cmd/makeslop/main_test.go`, `cmd/makeslop/status_test.go`, `cmd/makeslop/main.go`

- [x] add an in-package fake (e.g. `fakeDocker` implementing all four consumer
      interfaces with scripted exit code / ping err / image-missing / build opts
      capture, mirroring the old `FakeRunClient`/`FakeBuildClient` fields)
- [x] replace the 8 `docker.SetClientForTest(docker.NewFakeRunClient(n))` sites + 5
      `SetTTYCheckForTest`/`SetTermMakeRawForTest` sites with `fakeDocker` injection via
      the Task-3 `newRootCmdWithDeps` seam. Do it **incrementally**, one test function
      at a time, running `go test ./cmd/makeslop/` frequently.
- [x] replace the `onContextForTest` global usage (main_test.go ~3811–3828) with an
      explicit observer field/param on the cmd wiring; delete the
      `var onContextForTest` global from `main.go`
- [x] update `status_test.go`'s 2 sites (`installFakeStatusClient`) to inject a
      `daemonChecker`/`imageChecker` fake
- [x] run `go test ./cmd/makeslop/` — must pass before next task

> Note: the genuinely heaviest test rewrite is Task 2's `run_test.go`/`build_test.go`
> (direct `run()`/`build()` helper calls being deleted), not this task — the cmd
> churn is 8+5 sites, smaller than originally estimated.

### Task 5: Delete old API, globals, and `testing.go`

**Files:**
- Delete: `internal/docker/testing.go`
- Modify: `internal/docker/run.go`, `internal/docker/build.go`, `internal/docker/preflight.go`, `internal/docker/client.go`
- Modify: `internal/docker/fakes_test.go` (now holds all fake types)
- Modify: `internal/docker/docker.go` (New uses direct defaults, no global delegation)
- Modify: `cmd/makeslop/main_test.go` (inline skipNonPOSIX)
- Modify: `internal/projectconfig/projectconfig_test.go` (inline skipNonPOSIX)

- [x] delete `internal/docker/testing.go`
- [x] delete package funcs `Run`, `Build`, `CheckDaemon`, `ImageExists` (now methods)
- [x] delete globals `newClientFn`, `ttyCheck`, `termMakeRaw`; fold the real
      `isTTY`/`termMakeRaw` defaults into `New`
- [x] delete the now-unused `run()`/`build()` unexported helpers (renamed to
      `runContainer`/`buildImage` and remain as internal helpers for the methods)
- [x] confirm no remaining references to deleted symbols: `grep -rn "newClientFn\|SetClientForTest\|ttyCheck\|termMakeRaw\|onContextForTest\|func run(\|func build(" --include='*.go'`
      returns nothing (the last two guard against leftover direct helper calls)
- [x] run `go build ./...`, `go vet ./...`, `go test ./...`, and the integration
      compile-check `go test -tags integration -run XXX ./internal/docker/` — must all pass

### Task 6: Verify acceptance criteria
- [ ] verify behavior-preserving: exit-code contract (`*ExitError`, 137 passthrough),
      `ImageExists` (true,nil)/(false,nil)/(false,err) contract, fail-loud preflight,
      `ErrNoTTY` on non-TTY run, daemon-unreachable wrapping
- [ ] verify no package-level mutable state remains in `internal/docker` and `cmd`
      (sentinel errors / regexes / lookup tables are fine; swap-seams are gone)
- [ ] run full suite: `go build ./... && go vet ./... && go test ./...`
- [ ] compile-check integration tag: `go test -tags integration -run XXX ./internal/docker/`

### Task 7: Update documentation
- [ ] rewrite CLAUDE.md sections: "apiClient seam and SetClientForTest",
      "testing.go in the production binary", "Shared preflight helpers" (now methods,
      single shared client; also fix the stale "5 s" timeout → actual
      `preflightTimeout = 10 * time.Second`), "ExitError and the exit-code contract"
      (if signatures changed) — reflect struct-DI via `New`+options, no globals,
      `testing.go` deleted, consumer-side interfaces in `cmd`
- [ ] note in CLAUDE.md that `CurrentVersion`/`MigrationVersion` are NOT bumped
      (no `Settings` struct or Dockerfile change)
- [ ] update README.md only if it documents the docker package API (likely no change)
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion
*Items requiring manual intervention or external systems — informational only*

**Manual verification:**
- Smoke test against a live Docker daemon: `makeslop status`, `makeslop build`,
  and an interactive `makeslop run` in a real TTY — confirm preflight messages,
  build trace rendering, and exit-code passthrough behave identically.
- ⚠️ **Mandatory** (catches the use-after-close hazard that no unit test can):
  exercise `makeslop run` against a freshly-built image so that `CheckDaemon` →
  `ImageExists` → `Run` all execute on the same shared client. If any per-call
  `Close()` survived, this is where it surfaces — fakes' no-op `Close()` keeps the
  suite green regardless.

**Rollback note:**
- The refactor is staged so Tasks 1–4 are additive (old API coexists). If trouble
  surfaces, revert Task 5 (`git checkout` the four docker files + restore
  `testing.go`) to regain the old seams while keeping the new struct API. A full
  rollback is `git revert`/`git reset` of the feature branch — no migrations, no
  schema/version bumps, so there is no persistent state to unwind.
[2026-06-09 12:00:36] Task 4 completed Migrated all cmd tests from docker.SetClientForTest/SetTTYCheckForTest globals to in-package fakeDocker struct injected via newRootCmdWithDeps. Replaced onContextForTest global with explicit contextObserver parameter on runWithExitCode. Migrated status_test.go to use newFakeStatusDeps/runStatusCmd with deps injection. All 8 docker.SetClientForTest sites and all SetTTYCheckForTest/SetTermMakeRawForTest sites removed from cmd package tests. go build, go vet, and go test ./cmd/makeslop/ all pass.
