# Join other projects into `run` (`--join`)

## Overview
- Add `makeslop run --join/-j <path>[:ro|:rw]` (repeatable). Each path points to another makeslop project, meaning a directory containing `.makeslop.yaml`. It is mounted into the current project's container at `/workspace/<basename>`.
- A joined project is masked using **only its own** `exclude:` block (scan patterns, skip-dirs, files, dirs), and those masks apply **only to its own tree**. Its `cache:`, `environments:` and `network_*` keys are ignored. The main project's masks never apply to a join, and a join's masks never apply to the main project.
- `run -n` with joins groups the mounts per project and prints a pasteable separator before each group: `` `: '--- join: /home/me/lib (ro) ---'` `` (a no-op command substitution that stays valid when pasted into bash, dash and interactive zsh).
- Without `--join`, both the `run` and `-n` output are byte-identical to today.

## Context (from discovery)
- `internal/docker/spec.go`: pure `BuildSpec(Options) Spec`, with the mount order main bind → global → sandbox (config read-only, hooks tmpfs) → cache overlays → masks. `filterOut` drops a `/dev/null` mask on the config file. `ShellCommand()` renders `Args()` as backslash-continued lines.
- `internal/docker/spec_test.go`: about 49 references to the Options fields being replaced. The drift guards are `TestDriftGuard_ArgsAndSDKProjectionsAgree`, `_SandboxFlags`, `_CacheMountCombos` and `_Network`.
- `internal/docker/run_test.go`: 2 Options references.
- `internal/cli/run.go`: `runRun` covers load → scan → `reportScanResults` → `mergeUniqueSorted` → `sandboxMountGates` → `docker.Options`.
- `internal/cli/run_test.go`: about 26 references (mostly assertions on the spec passed to `fakeDocker`).
- `internal/cli/guard.go`: `resolvePwd` and `ensureWithinHome(stderr, pwd, outOfHome)`.
- `internal/projectconfig/projectconfig.go`: `Load` returns the default config when `.makeslop.yaml` is missing and rejects a symlinked one.
- `internal/security/security.go`: `Scan(ctx, root, patterns, skipDirs)`.
- Docs: `docs/reference.md` (`### run`, `## Container layout and mount table`, `## Dry run`) and `docs/security.md` (`## Secret masking`, `## Sandbox-policy protection`).

## Development Approach
- **testing approach**: Regular (code first, then tests in the same task)
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
- **CRITICAL: all tests must pass before starting next task**
- **CRITICAL: update this plan file when scope changes during implementation**
- run tests after each change: `GOTMPDIR=$HOME/.cache/gotmp go test -timeout=100s ./...` (/tmp is noexec here)
- lint: `golangci-lint run` if available, otherwise `go vet ./...`
- backward compatibility: with no `--join`, mounts and dry-run output are unchanged

## Testing Strategy
- **unit tests**: table tests for `BuildSpec`, `ShellCommand` golden strings, extended drift guards, `resolveJoins` with `t.TempDir()` trees, and CLI tests through `newRootCmdWithDeps` + `fakeDocker`.
- no e2e/UI tests in this project.

## Progress Tracking
- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope

## Solution Overview
- **Spec refactor (Approach B)**: `Options` describes a list of projects. `Projects[0]` is the main project (workdir, global mounts, cache overlays); the rest are joins. One helper emits the per-project bind, sandbox and mask mounts, so main and join masking share the same code path.
- **Sections**: `Spec.Sections` records where each project's mounts start. Only `ShellCommand()` reads it. `Args()` and the SDK projections ignore it, so "printed == executed" holds as before.
- **CLI**: `resolveJoins` (in `cli`, touches the filesystem) validates and normalizes the flag values. A shared `loadProject` helper does load → scan → report → `docker.Project` for every root.
- **Safety**: overlapping roots are rejected. Otherwise the same file could be visible through two mounts with different mask sets, which leaks secrets.

## Technical Details
- `docker`:
  ```go
  type Project struct {
      Host          string   // absolute, EvalSymlinks'd host root
      Name          string   // mounted at /workspace/<Name>
      Label         string   // dry-run separator text
      ReadOnly      bool     // joins only; ignored for Projects[0]
      MaskedFiles   []string // abs host paths under Host → /dev/null
      MaskedDirs    []string // abs host paths under Host → tmpfs
      ProtectConfig bool     // <Host>/.makeslop.yaml ro self-bind
      MaskGitHooks  bool     // <Name>/.git/hooks tmpfs
  }
  type Section struct {
      Label string
      Start int // index into Spec.Mounts
  }
  ```
  - `Options` removes `ProjectRoot`, `WorkspaceName`, `MaskedFiles`, `MaskedDirs`, `ProtectProjectConfig` and `MaskGitHooks`, and adds `Projects []Project`. The caller guarantees `len(Projects) >= 1`, unique `Name`s and non-overlapping `Host`s.
  - Main group: bind → `.claude/`, `.claude.json`, `.codex/` → sandbox → cache overlays → masks (the current order, unchanged).
  - Join group:
    - order: bind (`ReadOnly`) → sandbox (only when `!ReadOnly`) → masks
    - rw join: `filterOut` the join's own config path when `ProtectConfig`
    - ro join: there is no self-bind, so no `filterOut`; a `/dev/null` mask over the config is allowed
    - mask targets are `/workspace/<Name>/<Rel(Host, path)>`, relative to the **join's** `Host`
  - Comment in code: `Section.Start` indexes `Spec.Mounts`, which maps 1:1 to `--mount` tokens in `Args()`.
  - `Workdir = /workspace/<Projects[0].Name>`.
  - `Sections` is set only when `len(Projects) > 1`: one entry per project, with `Start` = index of the project's bind mount.
  - `ShellCommand()` tracks a `--mount` counter. When the counter equals a section's `Start`, it emits `` "  `: " + shellQuote("--- " + sanitize(label) + " ---") + "`" `` before that mount line, rendering as `` `: '--- join: /home/me/lib (ro) ---'` ``, and the line gets the usual ` \` continuation.
    - Why `:` and not `#`: interactive zsh (the macOS default) doesn't set `INTERACTIVE_COMMENTS`, so `#` isn't a comment there. `(ro)` then becomes a glob qualifier and quotes break parsing.
    - The reviewer tested the `:` form with no stderr in interactive and non-interactive zsh, interactive bash, `bash --posix` and dash.
    - `sanitize` replaces `` ` ``, `\`, `$` and control characters (including newlines) with `?`. Backslash and backtick processing inside backticks happens before `shellQuote` matters.
- `cli`:
  ```go
  type joinTarget struct {
      Host     string // abs, EvalSymlinks'd
      Name     string // filepath.Base(Host)
      ReadOnly bool
      Raw      string // as typed (for messages)
  }
  func resolveJoins(pwd, mainRoot, mainName, baseDir string, raw []string, outOfHome bool) ([]joinTarget, error)
  ```
  - Suffix: if the last `:`-separated segment is `ro` or `rw`, strip it; otherwise the whole value is the path (rw). A directory literally named `foo:ro` needs `foo:ro:rw`. `~` is not expanded by makeslop (document both).
  - Relative path → `filepath.Join(pwd, p)`. Then `EvalSymlinks` and `Stat` (must be a directory). `Lstat(<dir>/.makeslop.yaml)` must exist and be a regular file. The YAML is **not parsed here**; `loadProject` parses it after the daemon preflight, which keeps the existing daemon-first contract.
  - Reject a `Name` (basename) of `/`, `.`, `..` or empty. That covers joining `/`, which would otherwise mount at `/workspace` and shadow main.
  - Home guard: refactor `guard.go` into an `isWithinHome(path) (ok bool, home string, err error)` helper used by both `ensureWithinHome` and `resolveJoins`. The join message is `--join %q: outside <home> — pass --out-of-home to override`.
  - Errors (each names the raw flag value):
    - `--join %q: not a makeslop project (no .makeslop.yaml)`
    - `… is the current project`
    - `… is inside the current project`
    - `… contains the current project`
    - `… overlaps the makeslop data dir <baseDir>`
    - `… overlaps --join %q`
    - `… mount name %q collides with --join %q`
  - Overlap test (`overlaps(a, b string) bool`, checked in both directions for join vs main, join vs `baseDir`, and join vs join):
    - string containment via `filepath.Rel` + `filepath.IsLocal` (the same idiom as `guard.go`; no `HasPrefix`, so `/` is handled)
    - **plus** an inode check: walk the ancestors of each root and compare with `os.SameFile` against the other root. This catches case-insensitive filesystems (APFS: `../APP/sub` vs `/Users/me/app`) and bind/alias paths that `EvalSymlinks` doesn't canonicalize.
  - Labels: main `project: <host>`, join `join: <host> (ro|rw)`.
- `run` order: settings → image → `ws.Lookup` → `resolveJoins` → daemon preflight → `loadProject(main)` → `loadProject(join…)` in flag order → `BuildSpec` → dry-run / image + network preflight → `Run`.
- `loadProject(ctx, stderr, chrome, root, prefix string) (docker.Project, projectconfig.Config, error)`:
  - Warnings print as `makeslop: warning: <prefix><w>`, where the prefix is `""` for main and `join <host>: ` for joins.
  - The scan report says `masked N secret file(s)` for main and `masked N secret file(s) in <host>` for joins.
  - Symlink warnings show paths relative to that project's root.
  - A join's `.makeslop.yaml` parse/validation error is wrapped as `join <host>: projectconfig: …`.
  - If a join sets `cache:`, `environments:` or `network_*`, print one chrome line: `makeslop: join <host>: cache/environments/network settings ignored`. `projectconfig.Config` has no "set" marker, so use only what is cheaply observable: non-empty `Env` or `Network`. Skip the cache check, since cache defaults to true. Document the rest.

## What Goes Where
- **Implementation Steps**: code, tests and docs in this repo.
- **Post-Completion**: manual container checks.

## Implementation Steps

### Task 1: Refactor `docker.Options` to `Projects []Project` (no behavior change)

**Files:**
- Modify: `internal/docker/spec.go`
- Modify: `internal/docker/spec_test.go`
- Modify: `internal/docker/run_test.go`
- Modify: `internal/cli/run.go`
- Modify: `internal/cli/run_test.go`
- Modify: `internal/cli/status_test.go`

Approach B was chosen deliberately in brainstorm, even though it means about 88 reference updates. Mitigation: port tests mechanically and keep every expected `Mounts`/argv literal unchanged. A diff of expectation literals must show zero changes.

- [x] add `docker.Project`, replace the per-project `Options` fields with `Projects []Project`, and update the field docs
- [x] extract a per-project mount helper (bind, sandbox, masks) and have `BuildSpec` emit `Projects[0]` in exactly the current order (global and cache mounts inserted between sandbox and masks)
- [x] update `runRun` to build `Projects: []docker.Project{{…main…}}` with `Label: "project: " + workspaceRoot`
- [x] port all existing `spec_test.go` / `run_test.go` / cli `run_test.go` / `status_test.go` cases to the new shape (status_test.go had no Options references; nothing to port), keeping the expected `Mounts` / argv identical
- [x] run tests; they must pass with unchanged expectations before Task 2

### Task 2: Join mount groups and `Spec.Sections` in `BuildSpec`

**Files:**
- Modify: `internal/docker/spec.go`
- Modify: `internal/docker/spec_test.go`

- [x] emit a group for each `Projects[1:]` entry: bind (`ReadOnly`), sandbox mounts only when `!ReadOnly`, masks with a per-project `filterOut` of the config path
- [x] add `Section` / `Spec.Sections`, populated only when `len(Projects) > 1`
- [x] write tests:
  - an rw join: bind + config ro + hooks tmpfs + masks
  - an ro join: readonly bind + masks, no sandbox; a config `/dev/null` mask is kept
  - two joins: order + Section `Start` indices
  - an rw join's config-file mask is filtered
  - join masks map to `/workspace/<join>/rel`, computed against the join `Host`, not the main root
  - no joins → `Sections == nil`
- [x] extend the drift guards with a joins case: `Args()` mounts == `HostConfig().Mounts` in count and order
- [x] run tests; they must pass before Task 3

### Task 3: Render sections in `ShellCommand()`

**Files:**
- Modify: `internal/docker/spec.go`
- Modify: `internal/docker/spec_test.go`

- [x] in `ShellCommand()`, count `--mount` tokens and emit `` `: '<--- label --->'` `` (via `shellQuote`) before the mount whose index matches a section's `Start`
- [x] add a `sanitizeLabel` helper (`` ` ``, `$`, `\`, control characters → `?`)
- [x] write golden tests: with joins, separators sit at the right lines with correct continuation; without joins, output is identical to the existing golden
- [x] write tests: `Args()` never contains separator text; labels containing `` ` ``, `$`, `\`, newline, `'`, `"`, `(`, `)` and `;` render as a single safe line
- [x] run tests; they must pass before Task 4 (also added a paste test running the output through bash, bash --posix, dash, zsh and interactive zsh)

### Task 4: `resolveJoins` and the `--join` flag parsing

**Files:**
- Create: `internal/cli/join.go`
- Create: `internal/cli/join_test.go`
- Modify: `internal/cli/guard.go` (extract `isWithinHome`)

- [x] implement `joinTarget` and `resolveJoins` (suffix parsing, cwd-relative resolution, `EvalSymlinks`, directory check, regular `.makeslop.yaml` check, per-join home guard)
- [x] implement the overlap and collision checks (join vs main in both directions, join vs join, duplicate, basename collision among joins and with the main mount name)
- [x] write tests for success cases: relative and absolute paths, `:ro` / `:rw` / no suffix, a path containing `:` without a valid suffix, a symlinked join dir resolved to its target
- [x] write tests for error cases:
  - path problems: missing dir, a file instead of a dir, missing `.makeslop.yaml`, symlinked `.makeslop.yaml`
  - overlaps: join == main, join inside main, main inside join, duplicate, nested joins, a join inside/containing `baseDir`
  - names: basename collision, a join of `/`, empty `-j ""` / `-j :ro` (resolves to pwd → current-project error)
  - home guard: outside `$HOME` with and without `outOfHome`, asserting the exact join message
- [x] write tests for the inode overlap check: the same dir reached through an alias path, and a direct `os.SameFile` ancestor helper test
- [x] write tests for `isWithinHome`; existing `ensureWithinHome` tests keep passing unchanged
- [x] run tests; they must pass before Task 5 (golangci-lint not installed here; `go vet ./...` clean)
- ➕ a collision with the main mount name reads `mount name %q collides with the current project`; the mount-name check runs before the `.makeslop.yaml` check so `-j /` gets the name error

### Task 5: Wire `--join` into `run` via the `loadProject` helper

**Files:**
- Modify: `internal/cli/run.go`
- Modify: `internal/cli/run_test.go`

- [x] register `--join` / `-j` (`StringArrayVarP`) on `run` with help text
- [x] extract `loadProject` from `runRun` (load, prefixed warnings, scan, report, merge, `sandboxMountGates` → `docker.Project`), with a join variant of the `reportScanResults` message
- [x] call `resolveJoins` right after `ws.Lookup`; load joins after main in flag order; main's `Config` still supplies Cache/Env/Network
- [x] write tests: `-n -j ../lib` prints both separators and the join mounts at `/workspace/lib`; `:ro` → readonly bind with no sandbox mounts
- [x] write tests: masking isolation (a main pattern doesn't mask a join file and vice versa; a join's `exclude.files`/`dirs` apply only to the join); a join's warnings are prefixed and bypass `--quiet`
- [x] write tests: an invalid join `.makeslop.yaml` aborts with no `Run` call and the error names the join; daemon down + bad join YAML reports the daemon error (daemon-first contract, same as main)
- [x] write tests: a join scan walk error aborts with no `Run` call; a join's `environments`/`network_mode` are ignored, with the one "ignored" chrome line
- [x] write tests: `-n -j` makes no daemon calls (mirror `TestRun_DryRun_NetworkContainer_NoDaemonCalls`); `--quiet` hides the join "masked N … in <host>" line but not its warnings; main's "masked N secret file(s)" text is unchanged; join symlink warnings are relative to the join root
- [x] run tests; they must pass before Task 6 (golangci-lint not installed here; `go vet ./...` clean)
- ➕ `reportScanResults` takes `in`/`prefix` args; a join's scan symlink warnings are prefixed `join <host>: ` like its config warnings. `loadProject` takes `join bool` instead of a prefix string (the caller sets Name/Label/ReadOnly)

### Task 6: Verify acceptance criteria
- [x] verify all requirements from Overview are implemented (branch binary `-n -j ../lib:ro`: per-project separators, readonly join bind, join masked only by its own `exclude:`, "ignored" line for its `network_mode`)
- [x] verify that with no `--join`, `-n` output is byte-identical to `main` (compare built binaries on a sample project) — stdout and stderr identical (`cmp`) on a project with scan patterns, `files:` and `dirs:` excludes
- [x] run the full test suite: `GOTMPDIR=$HOME/.cache/gotmp go test -timeout=100s ./...`
- [x] run `golangci-lint run` (or `go vet ./...`) (golangci-lint not installed; `go vet ./...` clean)

### Task 7: [Final] Update documentation
- [x] `docs/reference.md`: `--join` under `### run` (syntax, suffix, the `foo:ro:rw` escape, no `~` expansion with `--join=`, cwd-relative paths, the ignored keys, the not-a-project/overlap/collision errors, the home guard), the mount table for joins, and dry-run section separators
- [x] `docs/security.md`: per-project masking (joins use only their own `exclude:`), why overlapping roots are rejected, `:ro` semantics (no config bind/hooks tmpfs needed), that symlinks in joins carry the same residual risk, that an rw join exposes its `.git/config` (`core.hooksPath`, `core.fsmonitor`) the same way main does (recommend `:ro` unless edits are needed), and that `reservedPaths` still applies to a join's excludes (conservative)
- [x] `CLAUDE.md`: mount order is per project (`Projects[0]` main, then joins); `Sections` affect only `ShellCommand`; joins resolve (path checks only) before the daemon preflight and are parsed/scanned after it; join overlap uses Rel/IsLocal + `os.SameFile` ancestors
- [x] move this plan to `docs/plans/completed/` (deferred to finalize)

## Post-Completion
*Items requiring manual intervention or external systems; informational only*

**Manual verification**:
- `makeslop run -j ../lib` against a real image: `/workspace/lib` is visible, its masked secrets read as empty, `:ro` rejects writes, and `.git/hooks` in an rw join is empty
- paste a `-n` output with joins into a shell and confirm the backtick separators don't break the command
