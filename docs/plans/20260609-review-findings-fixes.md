# Fix code-review findings: run.go races, goroutine joins, decoupling, preflight order, docs

## Overview

Fixes eight findings from the architecture review of makeslop, on branch `sec-refactoring`:

1. **Output truncation race** — `runContainer` never joins the stdout copy goroutine
   (`go io.Copy(os.Stdout, att.Reader)`); tail output is lost when the container exits quickly.
2. **AutoRemove + wait-after-start race** — `ContainerWait` is registered after `ContainerStart`;
   a fast-exiting container may be auto-removed first, yielding "No such container" instead of
   the exit code and breaking the documented exit-code contract.
3. **Stdin copy goroutine leak** — one goroutine per `Run` call parks forever in a blocking
   stdin read.
4. **Layering wrinkle** — `internal/docker` imports `internal/config` solely for
   `config.WorkspacesDir` (`spec.go`).
5. **Hidden settings load** — `workspace.Lookup` calls `config.Load` internally, so `runRun`
   loads settings twice (possible inconsistency between the two reads).
6. **Dead token** — `ShellCommand`'s switch still lists `--network`, which `Args()` no longer emits.
7. **Slow failure path** — `runRun` walks the whole repo (secret scan) before pinging the daemon;
   when Docker is down the user waits through the walk for nothing.
8. **skip-dirs hole** — directories pruned from the scan (`.git`, `node_modules`, …) are still
   bind-mounted into the container unscanned. **Decision: document as a trust assumption only;
   no behavior change.**

All design decisions below were validated interactively via brainstorm — do not re-litigate them.

## Context (from discovery)

- Files involved: `internal/docker/{run.go,docker.go,spec.go,fakes_test.go}`,
  `internal/workspace/workspace.go`, `cmd/makeslop/{main.go,status.go}`,
  `docs/security.md`, `CLAUDE.md`.
- moby client API verified: `moby.ContainerWaitOptions{Condition: container.WaitConditionNextExit}`
  exists in `client@v0.4.1` / `api@v1.54.2`; the client docs themselves recommend registering a
  next-exit wait before issuing start.
- Project conventions that constrain the work: pure/impure split in `internal/docker`
  (spec.go stays pure), struct-DI with no package-level mutable globals, POSIX-only,
  test fakes only in `_test.go` files, drift-guard test between `Args()` and SDK projections.
- ⚠️ Fake naming: the Run *lifecycle* tests use `fakeClient` in `internal/docker/run_test.go`
  (it already has an `attachPayload` field, set but never asserted). The `fakeRunClient` in
  `fakes_test.go` (documented as `FakeRunClient` in CLAUDE.md — stale capitalization) serves
  only preflight/client tests and cannot script delayed attach output. Task 1 extends
  **`fakeClient`**, not `fakeRunClient`.
- `ws.Lookup` has **three** call sites: `cmd/makeslop/main.go:125` (runRun),
  `cmd/makeslop/main.go:320` (init RunE), `cmd/makeslop/status.go:240`. All three change in Task 3.

## Development Approach

- **Testing approach**: Regular (code first, then tests) — except the Task 1 drain test, which is
  written to fail against the pre-fix code to prove finding #1, then pass after the fix.
- Complete each task fully before moving to the next; one commit per task, each leaving the tree green.
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
  (success and error scenarios; tests listed as separate checklist items).
- **CRITICAL: all tests must pass before starting the next task** — `go test ./...`.
- **CRITICAL: update this plan file when scope changes during implementation.**
- Maintain backward compatibility of CLI behavior except where a fix is the point
  (status detail string change in Task 3 is intentional and documented there).

## Testing Strategy

- Unit tests required for every task (see Development Approach).
- No UI/e2e framework in this project; the gated docker integration test
  (`MAKESLOP_DOCKER_IT=1 go test -tags integration ./internal/docker/`) is run manually if a
  daemon is available (Post-Completion).

## Progress Tracking

- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix; blockers with ⚠️ prefix.
- Keep plan in sync with actual work.

## Solution Overview

**New `runContainer` lifecycle** (Tasks 1–2):
Create → Attach → raw mode + initial resize + SIGWINCH → **Wait (condition `next-exit`)** →
Start → stream copies → on wait result, **join stdout copy, then close the pollable stdin handle
and join stdin copy** → map `StatusCode` → `ExitError`. AutoRemove stays on; registering the wait
before start guarantees the daemon delivers the exit status even if the container dies and is
reaped within milliseconds.

**Stream DI**: `Docker` gains `stdin io.Reader` / `stdout io.Writer` (defaults `os.Stdin`/`os.Stdout`)
plus `WithStreams(in, out)` Option. Both `io.Copy` calls use the injected streams. Fd-based
terminal ops (raw mode, `GetSize`, resize) stay on real `os.Stdin` — no extra abstraction.

**Stdin join (pollable dup)**: production path sets `O_NONBLOCK` on stdin's open file description,
dups the fd, wraps with `os.NewFile` — Go registers a nonblocking fd with the runtime poller, so
`Close()` unblocks a pending `Read`. The blocking flag is restored in the same deferred cleanup
that restores raw mode. If `fcntl`/`dup` fails, fall back to reading `os.Stdin` directly with the
documented one-goroutine leak (what docker CLI does). Injected test readers that implement
`io.Closer` (e.g. `os.Pipe`) are closed the same way, so the join logic is testable without a PTY.

**Decoupling**: `docker.Options` gains `WorkspaceHost string`; `BuildSpec` stops computing it from
`BaseDir + config.WorkspacesDir + WorkspaceName` and the `config` import disappears from `spec.go`.
`workspace.Lookup` becomes `Lookup(s *config.Settings, pwd string)` with no internal load;
`runRun` loads settings once. Dead `--network` case removed from `ShellCommand`.

**runRun order**: pwd → home guard → `config.Load` → `ws.Lookup` → **daemon preflight (only when
`!dryRun`)** → `projectconfig.Load` → `security.Scan` → `BuildSpec` → dry-run print/return →
image preflight → `Run`. Preserves: dry-run works without a daemon; printed == executed;
YAML-parse-aborts-before-Run.

**Docs**: `docs/security.md` gains a "Trust assumptions" subsection for skip-dirs;
CLAUDE.md's no-`.env`-leak wording is scoped to unskipped paths and updated for the new signatures.

## Technical Details

- Wait registration: `wr := cli.ContainerWait(ctx, id, moby.ContainerWaitOptions{Condition: container.WaitConditionNextExit})`
  placed after attach/raw-mode, before `ContainerStart`.
- Drain: stdout copy goroutine does `io.Copy(stdout, att.Reader)` then `close(outputDone)`;
  on the `wr.Result` path, `select { case <-outputDone: case <-ctx.Done(): }` before the
  status-code mapping. No timeout — a TTY attach stream EOFs on container exit. **Known bound,
  stated explicitly:** the sole production safety net is `ctx.Done()` (SIGINT/SIGTERM via
  `signal.NotifyContext`); an attach stream that never closed would hang until signal. Accepted.
- Stdin join: after the drain, close the stdin handle (pollable dup or injected `io.Closer`),
  then `select { case <-stdinDone: case <-ctx.Done(): }`. **Ordering constraint:** the join must
  stay inline before return — do NOT convert it to a defer. Defers run LIFO, so the existing
  `defer att.Conn.Close()` fires only after the inline join completes; this guarantees the stdin
  copy (which writes into `att.Conn`) is fully stopped before its destination conn is closed.
- Pollable dup sketch (POSIX-only, consistent with project invariant):
  `syscall.SetNonblock(0, true)` → `syscall.Dup(0)` → `os.NewFile(uintptr(dupFd), "stdin")`;
  restore via `syscall.SetNonblock(0, false)` in the deferred terminal cleanup. Crash-without-restore
  leaves stdin nonblocking — strictly milder than the raw-termios damage already risked/restored.
- `status.go` workspace check: reuse `loadedSettings` from check 2. Corrupt settings →
  `checkFail` with detail "cannot check — settings unreadable" (improvement over today's
  redundant parse error). Absent settings → pass default empty `Settings` → "not registered"
  (unchanged output).
- `status` scan cost: intentionally left as-is (YAGNI).

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code, tests, docs in this repo.
- **Post-Completion** (no checkboxes): manual PTY verification, gated integration test.

## Implementation Steps

### Task 1: Wait-before-start + output drain + stream DI

**Files:**
- Modify: `internal/docker/run.go`
- Modify: `internal/docker/docker.go`
- Modify: `internal/docker/fakes_test.go`
- Modify: `internal/docker/run_test.go` (home of `fakeClient`, the Run-lifecycle fake)

- [ ] add `stdin io.Reader` / `stdout io.Writer` fields to `Docker` with `os.Stdin`/`os.Stdout`
      defaults in `New`; add `WithStreams(in io.Reader, out io.Writer) Option`
- [ ] thread streams into `runContainer` (new params, same style as `isTTYFn`/`makeRawFn`);
      both `io.Copy` calls use them
- [ ] move `ContainerWait` before `ContainerStart` with `Condition: container.WaitConditionNextExit`
- [ ] add `outputDone` channel closed by the stdout copy goroutine; on `wr.Result`, select on
      `outputDone` / `ctx.Done()` before mapping `StatusCode` → `ExitError`
      (stdin-goroutine join is explicitly OUT of scope until Task 2 — Task 1 leaves the stdin
      copy unjoined, as today)
- [ ] extend `fakeClient` in `run_test.go` (NOT `fakeRunClient` — see Context) to record call
      order and to script an attach reader that releases a final payload only after the wait
      result is delivered, then EOFs (the fake must ALWAYS eventually EOF the attach reader,
      or the drain select would need ctx cancellation to terminate)
- [ ] write drain test: via `WithStreams` buffer, every scripted output byte present when `Run`
      returns (verify it fails against pre-fix code, then passes); inject a NON-blocking stdin
      (e.g. `bytes.NewReader`) so the unjoined stdin goroutine exits on EOF and Task 1 stays green
- [ ] write call-order test: `ContainerWait` invoked before `ContainerStart`
- [ ] write fast-exit test: wait result available immediately → correct `ExitError` code
- [ ] run `go test ./...` — must pass; commit:
      `fix(docker): register ContainerWait before start; drain output before returning`

### Task 2: Join the stdin copy goroutine (pollable dup)

**Files:**
- Modify: `internal/docker/run.go`
- Modify: `internal/docker/run_test.go`

- [ ] add `newPollableStdin()` helper: `SetNonblock(0)` + `Dup` + `os.NewFile`; returns handle,
      restore func, ok flag; on any failure return `os.Stdin` path with documented-leak fallback
- [ ] production path (stream == `os.Stdin`): copy from the pollable handle; injected readers
      implementing `io.Closer` are treated identically
- [ ] add `stdinDone` channel; after output drain, close the stdin handle and select on
      `stdinDone` / `ctx.Done()`; restore the blocking flag in the same deferred cleanup as
      `term.Restore`; keep the join INLINE before return (never a defer) so it completes before
      the deferred `att.Conn.Close()` fires (see Technical Details ordering constraint)
- [ ] document the fallback leak in a comment on `Run`
- [ ] write join test: pipe-backed stdin via `WithStreams`; assert both copy goroutines exited
      before `Run` returned (done-flag on the fake, not goroutine counting)
- [ ] write fallback-branch unit test (non-pollable injected reader without `io.Closer` → Run
      still returns correctly, leak documented path)
- [ ] run `go test ./...` — must pass; commit:
      `fix(docker): join stdin copy via pollable dup`

### Task 3: Decoupling refactors

**Files:**
- Modify: `internal/docker/spec.go`
- Modify: `internal/workspace/workspace.go`
- Modify: `cmd/makeslop/main.go`
- Modify: `cmd/makeslop/status.go`
- Modify: `internal/docker/spec_test.go`, `internal/workspace/workspace_test.go`,
  `cmd/makeslop/main_test.go`, `cmd/makeslop/status_test.go`

- [ ] add `WorkspaceHost string` to `docker.Options`; `BuildSpec` uses it for the per-workspace
      overlay mounts; remove the `internal/config` import from `spec.go`
      (`WorkspaceName` STAYS — still needed for the container-side `/workspace/<name>` path)
- [ ] change `workspace.Lookup` to `Lookup(s *config.Settings, pwd string)` (no internal
      `config.Load`); `workspace.Init` unchanged
- [ ] update ALL THREE `ws.Lookup` call sites:
      (a) `runRun` (main.go:125) — load settings once near the TOP of the function (before
      Lookup; this position is a prerequisite for Task 4's preflight insertion), pass to
      `Lookup` and into `Options` (`WorkspaceHost` = the `workspaceDir` Lookup returns);
      (b) init RunE (main.go:320) — it calls `ws.Init(pwd)` first (own load under lock,
      unchanged), then needs a `config.Load` for the subsequent `Lookup`; the existing-install
      branch already loads settings — restructure to load once after `ws.Init` and reuse for
      both `Lookup` and the stale-nudge check;
      (c) `status.go:240` — reuse `loadedSettings` from check 2; corrupt settings →
      "cannot check — settings unreadable"; absent → default empty `Settings`
- [ ] remove the dead `"--network"` case from `ShellCommand`'s switch
- [ ] update spec table tests + drift-guard for `WorkspaceHost`; update workspace tests for the
      new signature; update status test for the new corrupt-settings detail string; update any
      `main_test.go` init/run tests that exercise the changed call sites
- [ ] write workspace tests covering `Lookup` with explicitly-passed settings (registered,
      ancestor, not-registered cases). No load-count assertion — `config.Load` has no seam and
      adding one would violate the no-test-helpers-in-production convention; the single-load
      property is structural (one `config.Load` call in `runRun`)
- [ ] run `go test ./...` — must pass; commit:
      `refactor: decouple docker from config; explicit settings in Lookup; drop dead --network token`

### Task 4: Daemon preflight before secret scan

**Files:**
- Modify: `cmd/makeslop/main.go`
- Modify: `cmd/makeslop/main_test.go`

- [ ] reorder `runRun`: daemon preflight (with `WithPreflightTimeout`) directly after `ws.Lookup`,
      gated on `!dryRun`; projectconfig/scan/BuildSpec follow; dry-run print/return and image
      preflight keep their relative positions
      (depends on Task 3 having moved `config.Load` to the top of `runRun` — if Task 3 changed,
      revisit this ordering)
- [ ] write ordering test: failing fake daemon + invalid `.makeslop.yaml` in the workspace →
      the daemon error is reported (proves ping precedes parse/scan)
- [ ] write dry-run test: daemon down + `--dry-run` → command still prints the shell command
      and exits 0
- [ ] run `go test ./...` — must pass; commit:
      `fix(cmd): daemon preflight before secret scan in run`

### Task 5: Documentation — skip-dirs trust assumption + signature updates

**Files:**
- Modify: `docs/security.md`
- Modify: `CLAUDE.md`

- [ ] add "Trust assumptions" subsection to `docs/security.md`: skip-dirs are mounted but not
      scanned; secrets inside them (e.g. `.git/config` credentials, files under `node_modules`)
      are the user's responsibility; shrinking skip-dirs widens the guarantee
- [ ] CLAUDE.md: scope the no-`.env`-leak invariant wording to unskipped paths
- [ ] CLAUDE.md: update for new signatures/fields — `Lookup(s, pwd)`, `Options.WorkspaceHost`,
      `WithStreams`, stream fields on `Docker`, new Run lifecycle (wait-before-start, joined copies)
- [ ] CLAUDE.md: fix stale fake naming — docs say `FakeRunClient`/`FakeBuildClient`; actual code
      uses unexported `fakeRunClient` etc., and the Run-lifecycle fake is `fakeClient` in
      `run_test.go` (document both fakes and their distinct roles)
- [ ] run `go test ./...` (docs-only, sanity); commit:
      `docs: scope no-leak invariant to unskipped paths`

### Task 6: Verify acceptance criteria

- [ ] re-check each of the eight findings against the final code (1–7 fixed, 8 documented)
- [ ] verify invariants held: dry-run works daemonless; printed == executed; YAML-parse aborts
      before Run; no package-level mutable globals in `internal/docker`; spec.go still pure
- [ ] run full suite: `go test ./...` and `go vet ./...`
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

**Manual verification** (needs a real PTY + docker daemon):
- `makeslop run` with a fast-exiting shell command — confirm full output and correct exit code
  (e.g. `exit 7` → host exit 7), no "No such container" flake.
- Gated integration test: `MAKESLOP_DOCKER_IT=1 go test -tags integration ./internal/docker/`.
- Interactive session sanity: typing, resize (SIGWINCH), Ctrl-D exit, terminal restored to
  cooked + blocking mode afterward (`stty -a` shows sane state).

**External system updates**: none.
