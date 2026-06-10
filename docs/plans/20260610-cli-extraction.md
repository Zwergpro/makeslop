# Extract cmd/makeslop CLI into internal/cli

## Overview
- Move the entire cobra command tree + orchestration out of `package main`
  (`cmd/makeslop`) into a new `internal/cli` package (`package cli`), mirroring the
  current per-command file split.
- Problem: the `cmd/makeslop` package has grown large (~1089 non-test LOC + ~5200
  test LOC) with orchestration logic (`runRun`, `runStatus`, `sandboxMountGates`,
  `checkList`/`renderChecks`) living in `package main`. The goal is a tiny
  `cmd/makeslop/main.go` entry point with everything else relocated.
- This is a **near-verbatim relocation** (commit 1), followed by a **comment-trim
  pass** (commit 2). No behavior changes, no domain-package redistribution.

## Context (from discovery)
- Files involved (all under `cmd/makeslop/`, `package main`):
  - Source (10): `main.go` (root wiring + `var version` + `main()`), `deps.go`,
    `guard.go`, `run.go`, `status.go`, `init.go`, `build.go`, `config.go`,
    `migrate.go`, `version.go`
  - Tests (8, all `package main`): `main_test.go`, `run_test.go` (2421 LOC),
    `status_test.go`, `init_test.go`, `config_test.go`, `build_test.go`,
    `migrate_test.go`, `version_test.go`
- Patterns found: thin cobra `RunE` closures delegating to named `runXxx`
  functions; struct-DI via `dockerDeps`; same-package `_test.go` fakes
  (`fakeRunClient`, `fakeBuildClient`, boundary fakes in `main_test.go`); a
  `runWithExitCodeAndDeps` test helper (`main_test.go:111`) that calls
  `newRootCmdWithDeps` directly.
- Dependencies / constraints identified:
  - `version` is a `package main` global targeted by ldflags `-X main.version=...`
    in **Makefile:6** and **.goreleaser.yaml:12 & :21** (3 hits total). These MUST
    stay pointed at `main.version` (zero build edits).
  - **`version_test.go:11-13` mutates the `version` package global directly**
    (`orig := version; version = "test-1.2.3"`). `main_test.go:390/410/430` exercise
    the `version` command via `runWithExitCode`.
  - There are **11 `runWithExitCode(baseDir, ...)` call sites** in tests
    (`config_test.go:119/139/212/229/254`, `main_test.go:370/390/410/430`) plus
    `runWithExitCodeAndDeps` (calls `newRootCmdWithDeps`, not `newRootCmd`).
  - CLAUDE.md describes the CLI layout at **lines 92, 104, 106, 163** — factual
    location statements that become wrong once the code moves.

## Development Approach
- **testing approach**: Regular — this is a relocation; the existing test suite
  (which moves with the code) is the safety net. No new test logic; the gate is the
  suite staying green.
- **Version seam decision (resolved):** `internal/cli` keeps its own package-level
  `var version = "dev"`, set by `Main(v string, ...)`. **All function signatures
  stay identical** (`newRootCmd(baseDir)`, `newVersionCmd()`,
  `runWithExitCode(baseDir, stdout, stderr, args, observer)`). This means
  `version_test.go` and all 11 `runWithExitCode` call sites move **verbatim** with
  zero edits. `cmd/makeslop/main.go` keeps `var version` solely as the ldflags
  landing pad and passes it to `cli.Main`. (Chosen over threading `version` as a
  parameter, which would have forced rewriting `version_test.go` + 11 call sites —
  contrary to the verbatim-move goal.)
- **CRITICAL: `go build ./... && go vet ./... && go test ./...` must pass at each
  task boundary.** The relocation is one indivisible compile unit, so the move is a
  single atomic task (Task 1) — the package is not expected to compile mid-task,
  only at the task boundary.
- Maintain backward compatibility: binary, flags, output, exit-code contract
  unchanged. Do NOT bump `CurrentVersion` or `MigrationVersion`.

## Testing Strategy
- **unit tests**: the 8 moved `_test.go` files become `package cli` and continue to
  exercise the relocated code unchanged (same-package tests, no signature changes).
- **integration test**: the gated `Build` integration test (`MAKESLOP_DOCKER_IT=1`,
  `-tags integration`) stays in `internal/docker/` — untouched.
- **no e2e suite** in this project.
- Verification command at every task boundary: `go build ./... && go vet ./... && go test ./...`

## Progress Tracking
- Mark completed items `[x]` immediately. Add ➕ for new tasks, ⚠️ for blockers.
- Keep this plan in sync with actual work.

## Solution Overview
- **New package `internal/cli`** holds the cobra tree and orchestration, one file
  per command plus `root.go` for the root wiring.
- **`cli.Main(version string, args []string) int`** is the single public entry
  point. It assigns the package `version` global, performs `config.DefaultBaseDir()`
  + `filepath.EvalSymlinks` resolution (moved out of `main.go`, same error
  messages → returns 1 on failure), then calls the package-internal
  `runWithExitCode(baseDir, os.Stdout, os.Stderr, args, nil)`.
- **`cmd/makeslop/main.go`** shrinks to ~10 lines: imports, `var version = "dev"`
  (ldflags target), `func main() { os.Exit(cli.Main(version, os.Args[1:])) }`.
- `cli.Main` is the **only** exported symbol; everything else stays unexported and
  is covered by the moved same-package tests.

## Technical Details
- File moves (commit 1), all `package main` → `package cli`:
  | from `cmd/makeslop/` | to `internal/cli/` | change |
  |---|---|---|
  | root logic in `main.go` | `root.go` | `newRootCmd`, `newRootCmdWithDeps`, `runWithExitCode` verbatim; **add** `var version` + `Main` |
  | `deps.go` `guard.go` `run.go` `status.go` `init.go` `build.go` `config.go` `migrate.go` `version.go` | same names | verbatim (only `package` clause changes) |
  | 8 `*_test.go` | same names | only `package main` → `package cli` |
- **No signature changes.** `newRootCmd`, `newRootCmdWithDeps`, `newVersionCmd`,
  `runWithExitCode`, `runWithExitCodeAndDeps` keep current signatures. `newVersionCmd`
  reads the package `version` global (as today).
- `cmd/makeslop/main.go` after: only `var version` + `main()`; the
  `DefaultBaseDir`/`EvalSymlinks`/exit-code logic lives in `cli.Main`.

## What Goes Where
- **Implementation Steps** (`[ ]`): the atomic move + `Main`/`main.go` (Task 1),
  CLAUDE.md layout fixes (Task 2) → commit 1; comment trim (Task 3) → commit 2.
- **Post-Completion** (no checkboxes): none required — fully verifiable in-repo.

## Implementation Steps

### Task 1: Atomic move — relocate all files to internal/cli, add cli.Main, shrink main.go

**Files:**
- Create: `internal/cli/root.go` (root logic from `cmd/makeslop/main.go` + new
  `var version` + `Main`)
- Move (`git mv`): `cmd/makeslop/{deps,guard,run,status,init,build,config,migrate,version}.go`
  → `internal/cli/`
- Move (`git mv`): all 8 `cmd/makeslop/*_test.go` → `internal/cli/`
- Modify: `cmd/makeslop/main.go` (reduce to entry point)

- [x] `git mv` the 9 non-`main` source files into `internal/cli/`; change
      `package main` → `package cli` in each (no logic/comment edits)
- [x] Create `internal/cli/root.go` from the root wiring in old `main.go`
      (`newRootCmd`, `newRootCmdWithDeps`, `runWithExitCode` verbatim) and **add**
      package-level `var version = "dev"`
- [x] Add `func Main(v string, args []string) int` to `root.go`: set
      `version = v`; resolve `config.DefaultBaseDir()` + `filepath.EvalSymlinks`
      (moved from old `main.go`, same error handling → print to `os.Stderr`,
      return 1); call `runWithExitCode(baseDir, os.Stdout, os.Stderr, args, nil)`
- [x] `git mv` all 8 `*_test.go` into `internal/cli/`; change `package main` →
      `package cli` (no other edits — signatures unchanged, so `version_test.go`'s
      `version =` mutation and the 11 `runWithExitCode` call sites compile as-is)
- [x] Rewrite `cmd/makeslop/main.go` to ~10 lines: `package main`, import `cli` +
      `os`, `var version = "dev"`, `func main() { os.Exit(cli.Main(version, os.Args[1:])) }`
- [x] run `go build ./... && go vet ./... && go test ./...` — must pass before Task 2

### Task 2: Update CLAUDE.md CLI layout references (commit 1)

**Files:**
- Modify: `CLAUDE.md`

- [x] Line 92: "`cmd/makeslop` (package `main`)" → "`internal/cli` (package `cli`)"
- [x] Line 104/106: keep "each command gets its own file" but relocate to
      `internal/cli`; correct the `main.go` sentence — `runWithExitCode`,
      `newRootCmd`/`newRootCmdWithDeps` now live in `internal/cli/root.go`;
      `cmd/makeslop/main.go` keeps only `var version` + `main()`; note `cli.Main` is
      the entry point; `errSilent` still in `internal/cli/guard.go`
- [x] Line 163: "`runWithExitCode` in `main.go`" → "`runWithExitCode` in
      `internal/cli/root.go`"
- [x] `grep -nE "cmd/makeslop|main\.go" CLAUDE.md` — confirm the only remaining
      `cmd/makeslop` reference is the binary entry point, and no `main.go` sentence
      still claims to hold relocated symbols
- [x] run `go test ./...` (docs-only; confirm still green)
- [x] **Commit 1**: "refactor: extract cmd/makeslop CLI into internal/cli package"

### Task 3: Comment trim — why-not-what (commit 2)

**Files:**
- Modify: `internal/cli/run.go`, `internal/cli/status.go`, `internal/cli/init.go`,
  `internal/cli/guard.go`, `internal/cli/deps.go`, `internal/cli/root.go`, and
  other moved files as warranted

- [ ] Delete what-restating doc comments where the signature already says it:
      `mergeUniqueSorted`, `filterOut`, `quietWriter`, `resolvePwd`, similar
- [ ] Tighten why-comments to 1–2 lines (keep rationale, drop prose):
      `sandboxMountGates` last-write-wins / `filterOut` ordering; daemon-preflight-
      before-scan; `ContainerWait`-before-`Start`
- [ ] Drop the repeated "named function so it can be called from the RunE closure,
      keeping the closure thin" comment on `runRun`/`runInit`/`runStatus`
- [ ] run `go build ./... && go vet ./... && go test ./...` — must pass
- [ ] **Commit 2**: "refactor: trim comments in internal/cli (why-not-what)"

### Task 4: Verify acceptance criteria
- [ ] `cmd/makeslop/main.go` is ~10 lines: only `var version` + `main()`
- [ ] `cli.Main` is the only exported symbol in `internal/cli`
      (`grep -nE "^func [A-Z]|^var [A-Z]|^type [A-Z]" internal/cli/*.go` excluding
      `_test.go` → only `Main`)
- [ ] ldflags untouched: `grep -n "main.version" Makefile .goreleaser.yaml` → 3 hits
- [ ] version injection works end-to-end:
      `go build -ldflags "-X main.version=smoke" -o /tmp/makeslop ./cmd/makeslop &&
      /tmp/makeslop version` → prints `smoke`
- [ ] `/tmp/makeslop --help` lists all subcommands unchanged (run, init, build,
      config, migrate, status, version)
- [ ] `CurrentVersion`/`MigrationVersion` unchanged in `internal/config/config.go`
- [ ] full suite: `go test ./...`
- [ ] CLAUDE.md has no stale layout claims (the line-92/104/106/163 set corrected)

### Task 5: [Final] Plan housekeeping
- [ ] Move this plan to `docs/plans/completed/`

## Post-Completion
*Informational only — no checkboxes.*

**Manual smoke (optional):** `go build -ldflags "-X main.version=smoke" -o
/tmp/makeslop ./cmd/makeslop && /tmp/makeslop version` prints `smoke`;
`/tmp/makeslop --help` shows the unchanged command set. No external/consuming-project
changes required — `internal/cli` is not importable outside this module.
