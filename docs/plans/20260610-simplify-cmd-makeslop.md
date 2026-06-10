# Simplify cmd/makeslop — per-command files + targeted dedupe

## Overview
- Behavior-preserving refactor of `cmd/makeslop`: same CLI, same flags, same output bytes, same exit codes.
- Problem: `main.go` (661 lines) holds all 7 commands inline; `runStatus` is ~200 lines of append boilerplate; the preflight-timeout block is repeated 4×; `main_test.go` is 4,293 lines.
- Solution: split into per-command files (the pattern `status.go` already follows), extract three dedupe helpers, decompose `runRun`/`runInit` into named steps, and split the test file to mirror production.
- Design validated interactively via brainstorm on 2026-06-10. Options explicitly rejected: feature trimming, collapsing the four docker interfaces into one (`dockerDeps` four-interface pattern is documented in CLAUDE.md and stays).

## Context (from discovery)
- Files involved: `cmd/makeslop/main.go` (661), `cmd/makeslop/status.go` (347), `cmd/makeslop/main_test.go` (4,293, 127 tests), `cmd/makeslop/status_test.go` (584, 19 tests; package total **146**).
- Most test names follow `TestRun_` / `TestInit_` / `TestBuild_` / `TestConfig` / `TestMigrate_` / `TestRoot_` / `TestRunWithExitCode_` prefixes. **Cross-cutting exceptions** (each spans multiple commands): `TestErrorVoice_*` (5), `TestQuiet_*` (3), `TestOutOfHome_RejectedOnVersion`, `TestGlobalOnly_RejectedOnNonInitCommands`; plus `TestVersion_*` (3). All have explicit destinations in Task 3.
- Shared test fakes/helpers are NOT contiguous: most live in the first ~220 lines, but `assertSnapshotsEqual` (~line 1078) and `mapKeys` (~line 1101) are mid-file and shared across init/run/migrate/config tests; `newFakeBuildDocker` (~line 315) is build-only; `hasMountWithContainer`/`hasMountWithContainerAndHost` (~lines 3793/3803) are run-only. Task 3 lists where each goes.
- Cobra v1.10.2 confirmed: `ParseFlags` merges parent persistent flags into the subcommand's flag set (shared `*Flag` pointers), so `cmd.Flags().GetBool("quiet")` in a subcommand `RunE` resolves the root persistent flag.
- `internal/docker` has its own `run_test.go`; different package, no conflict with a new `cmd/makeslop/run_test.go`.
- Output-pinning tests exist (`TestRun_YamlAbsentIsBitIdenticalArgv` etc.) — they are the byte-identical-behavior guard.

## Development Approach
- **Testing approach**: Regular. This is a behavior-preserving refactor — the existing 127-test suite is the safety net. Tests move between files but their logic never changes. New tests are added only for the new helpers' direct contracts.
- Complete each task fully before moving to the next.
- Make small, focused changes; one reviewable commit per task group (3 commits total).
- **CRITICAL: all tests must pass before starting next task** — no exceptions.
- **CRITICAL: update this plan file when scope changes during implementation.**
- After every task: `go build ./...`, `go vet ./...`, `gofmt -l .` (empty), `go test ./...` green.
- Package test count in `cmd/makeslop` must stay **146** + any new helper tests (assert with `go test ./cmd/makeslop/ -list '.*' | grep -c '^Test'` — note `-list` counts the whole package incl. `status_test.go`'s 19, not just `main_test.go`'s 127). Cross-check per file with `grep -cE '^func Test' cmd/makeslop/*_test.go` summed.

## Testing Strategy
- **Unit tests**: existing suite moves verbatim; new tests only for new helper contracts (`checkList`, preflight wrappers, `sandboxMountGates`, `stampMigratedVersion` — the latter two are mostly covered transitively by existing run/init tests; add direct tests only where a contract is not already pinned).
- **No e2e framework** in this project; the gated docker integration test (`MAKESLOP_DOCKER_IT=1`) is unaffected (lives in `internal/docker`).
- Byte-identical output is pinned by existing tests; do not weaken any assertion.

## Progress Tracking
- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix.
- Document issues/blockers with ⚠️ prefix.

## Solution Overview

Target layout (every command gets a `newXCmd(...)` constructor taking only what it needs — the pattern `newStatusCmd` already uses):

```
cmd/makeslop/
  main.go      — main(), runWithExitCode, newRootCmd, newRootCmdWithDeps
                 (root cmd + AddCommand wiring only), errSilent, version var
  deps.go      — containerRunner/imageBuilder/daemonChecker/imageChecker,
                 dockerDeps (+ 2 preflight wrapper methods), dockerNewErrStub
  guard.go     — resolvePwd, ensureWithinHome, quietWriter
  init.go      — newInitCmd(ws, baseDir), runInit, stampMigratedVersion
  run.go       — newRunCmd(ws, baseDir, deps), runRun, sandboxMountGates,
                 reportScanResults, filterOut, mergeUniqueSorted
  build.go     — newBuildCmd(baseDir, deps)
  config.go    — newConfigCmd(baseDir) with list/set subcommands
  migrate.go   — newMigrateCmd(baseDir)
  version.go   — newVersionCmd()
  status.go    — same shape; runStatus rewritten over the checkList collector
```

Key design decisions:
- **`--quiet` consumption**: the root persistent flag is read inside each `RunE` via `cmd.Flags().GetBool("quiet")` (cobra merges parent persistent flags into the subcommand flag set at execution time). No `*bool` threading through constructors. Command-local flags (`--dry-run`, `--out-of-home`, `--no-cache`, `--build-arg`, `--refresh`, `--global-only`, `--json`) stay as locals inside their own constructor.
- **`runRun` execution order is untouched**: pwd → home guard → config.Load → ws.Lookup → [if !dryRun] CheckDaemon → projectconfig.Load → security.Scan → BuildSpec → [if dryRun] print+return → ImageExists → Run.
- **Load-bearing comments move with their code** (mount-ordering last-write-wins rationale, gitfile/worktree residual risk, wait-before-start notes) — never deleted.
- **No further fragmentation** beyond the helpers named below.

## Technical Details

New helpers and exact signatures:

```go
// deps.go — encapsulate docker.WithPreflightTimeout + cancel (4 call sites today)
func (d dockerDeps) checkDaemonPreflight(ctx context.Context) error
func (d dockerDeps) imageExistsPreflight(ctx context.Context, image string) (bool, error)

// status.go — replaces ~10-line append blocks with 1–2 line calls
type checkList struct {
    checks []statusCheck
    ready  bool // starts true; fail() clears it
}
func (c *checkList) ok(name, detail string)   // detail "" allowed (omitted in output)
func (c *checkList) fail(name, detail string) // sets ready = false
func (c *checkList) warn(name, detail string) // non-blocking
func (c *checkList) info(name string)

// run.go — the two Lstat gates + the filterOut interaction (protect gate
// feeds filterOut, so they bundle). When protect is false, filtered is
// maskedFiles unchanged (filterOut's no-op-on-absent contract); callers
// replace maskedFiles with filtered unconditionally before building opts.
func sandboxMountGates(workspaceRoot string, maskedFiles []string)
    (protect, maskHooks bool, filtered []string)

// run.go — "masked N" via chrome (quiet-gated), symlink warnings to raw
// stderr bypassing --quiet, with the filepath.Rel fallback
func reportScanResults(stderr, chrome io.Writer, root string,
    masked, symlinkMatches []string)

// init.go — the WithLock + Load + stamp MigratedVersion + Save block
func stampMigratedVersion(baseDir string) error
```

`docker.WithPreflightTimeout` itself is unchanged (it remains part of the docker package API); only the `cmd` call sites change.

JSON output and rendering consume `checkList.checks` / `checkList.ready` exactly as the current locals — byte-identical output.

## Implementation Steps

### Task 1: Extract dedupe helpers within existing files (commit a)

**Files:**
- Modify: `cmd/makeslop/main.go`
- Modify: `cmd/makeslop/status.go`
- Modify: `cmd/makeslop/main_test.go` (additions only)
- Modify: `cmd/makeslop/status_test.go` (additions only)

- [ ] add `checkDaemonPreflight` / `imageExistsPreflight` methods on `dockerDeps` in `main.go`; replace the 4 inline `WithPreflightTimeout` blocks (2 in `runRun`, 2 in `runStatus`)
- [ ] add `checkList` collector in `status.go`; rewrite `runStatus` over it (target ~110 lines; output byte-identical)
- [ ] extract `sandboxMountGates` and `reportScanResults` in `main.go`; `runRun` calls them; construct `chrome` once at top of `runRun`
- [ ] extract `runInit` from the `initCmd` closure and `stampMigratedVersion` from its WithLock block; construct `chrome` once at top of `runInit`
- [ ] write direct unit tests for `checkList` (ok/fail/warn/info, ready-clearing) and `sandboxMountGates` (regular file vs missing vs dir for both gates, filterOut interaction) — only contracts not already pinned by existing tests
- [ ] run `go build ./... && go vet ./... && gofmt -l . && go test ./...` — all green, existing assertions untouched

### Task 2: Split production code into per-command files (commit b)

**Files:**
- Modify: `cmd/makeslop/main.go` (shrinks to wiring)
- Create: `cmd/makeslop/deps.go`, `cmd/makeslop/guard.go`, `cmd/makeslop/init.go`, `cmd/makeslop/run.go`, `cmd/makeslop/build.go`, `cmd/makeslop/config.go`, `cmd/makeslop/migrate.go`, `cmd/makeslop/version.go`

- [ ] create `deps.go`: move the 4 interfaces, `dockerDeps` (+ preflight methods), `dockerNewErrStub`
- [ ] create `guard.go`: move `resolvePwd`, `ensureWithinHome`, `quietWriter`
- [ ] create per-command files with `newInitCmd(ws, baseDir)`, `newRunCmd(ws, baseDir, deps)`, `newBuildCmd(baseDir, deps)`, `newConfigCmd(baseDir)`, `newMigrateCmd(baseDir)`, `newVersionCmd()`; each defines its own local flags; `--quiet` read via `cmd.Flags().GetBool("quiet")` in each RunE that needs it
- [ ] move `filterOut` + `mergeUniqueSorted` into `run.go` (run-only helpers)
- [ ] shrink `newRootCmdWithDeps` in `main.go` to root-cmd construction + persistent `--quiet` + `AddCommand` wiring (~20 lines); `main.go` keeps `main()`, `runWithExitCode`, `newRootCmd`, `errSilent`, `version`
- [ ] pure moves of function bodies — no logic changes in this task; no new tests needed (compilation + existing suite is the check)
- [ ] redistribute import blocks per file (each new file gets only what it needs; `main.go` sheds e.g. `security`/`projectconfig`/`sort`); run `goimports`/`gofmt` after each move
- [ ] run `go build ./... && go vet ./... && gofmt -l . && go test ./...` — all green

### Task 3: Split main_test.go per command (commit c)

**Files:**
- Modify: `cmd/makeslop/main_test.go` (shrinks to shared infra + root/exit-code + cross-cutting tests)
- Create: `cmd/makeslop/run_test.go`, `cmd/makeslop/init_test.go`, `cmd/makeslop/build_test.go`, `cmd/makeslop/config_test.go`, `cmd/makeslop/migrate_test.go`, `cmd/makeslop/version_test.go`

**Placement rule:** tests move to the file of the command they exercise; tests that exercise *multiple* commands (cross-cutting behavior contracts) stay in `main_test.go`. Shared helpers stay in `main_test.go` regardless of their current position in the file; single-consumer helpers move with their consumer.

- [ ] `main_test.go` keeps shared helpers (explicit list, position-independent): `fakeDocker` + methods, `newFakeDocker`, `runCmd`, `runCmdWithDeps`, `depsFrom`, `runWithExitCodeAndDeps`, `snapshotTree`, `listFiles`, `evalSymlinks`, `setHomeToTestParent`, `skipNonPOSIX`, **`assertSnapshotsEqual` (~line 1078), `mapKeys` (~line 1101)** — the last two are mid-file, not in the prelude
- [ ] `main_test.go` keeps tests: `TestRunWithExitCode_*`, `TestRoot_BareInvocation_*`, and the cross-cutting groups `TestErrorVoice_*` (5), `TestQuiet_*` (3)
- [ ] `run_test.go` ← `TestRun_*` (incl. dry-run and YAML suites), `TestMergeUniqueSorted`, `TestFilterOut`, `TestOutOfHomeFlag_Bypasses`, plus run-only helpers `hasMountWithContainer`/`hasMountWithContainerAndHost`
- [ ] `init_test.go` ← `TestInit_*`, `TestGlobalOnly_RejectedOnNonInitCommands` (asserts the init-scoped flag is rejected elsewhere — init owns the flag)
- [ ] `build_test.go` ← `TestBuild_*`, plus build-only helper `newFakeBuildDocker`
- [ ] `config_test.go` ← `TestConfig*`; `migrate_test.go` ← `TestMigrate_*`
- [ ] `version_test.go` ← `TestVersion_*` (3), `TestOutOfHome_RejectedOnVersion`
- [ ] pure moves — zero changes to test bodies; helper tests from Task 1 land in their matching files; redistribute import blocks per file (e.g. `assets` import leaves `main_test.go` for `migrate_test.go`/`build_test.go`)
- [ ] verify package test count: `go test ./cmd/makeslop/ -list '.*' | grep -c '^Test'` equals **146** + Task-1 additions; cross-check `grep -cE '^func Test' cmd/makeslop/*_test.go` sums to the same
- [ ] run `go build ./... && go vet ./... && gofmt -l . && go test ./...` — all green

### Task 4: Verify acceptance criteria
- [ ] full suite green: `go test ./...`
- [ ] `gofmt -l .` empty; `go vet ./...` clean
- [ ] no behavior drift: `git diff` shows no changes to output strings, flag definitions, exit-code mapping, or `runRun` execution order
- [ ] confirm `--quiet` still works on init/build/run (covered by existing tests, e.g. `TestBuild_Refresh_Quiet_SuppressesNotice`, `TestQuiet_*`)
- [ ] size goals (approximate, non-blocking — byte-identical output is the hard gate, line counts are not): production files ≤ ~250 lines, `runStatus` ~110, `runRun` ~90

### Task 5: [Final] Update documentation
- [ ] update CLAUDE.md references: `dockerNewErrStub` "(in `main.go`)" → `deps.go`; "Both `runRun` (main.go) and `runStatus` (status.go) use it" (preflight section) → `runRun` is in `run.go`; the TTY-notions section's "(`status.go`, `main.go`)" → `runRun`'s usage now in `run.go`
- [ ] note the per-command file layout under the consumer-side interfaces section; four-interface pattern explicitly retained
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion
*No external systems involved. Optional manual check:*
- run `makeslop status` and `makeslop run --dry-run` in a registered workspace and eyeball that output is unchanged.
