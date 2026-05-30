# makeslop config subcommand (list + set)

## Overview
- Add a `makeslop config` subcommand letting users view and change makeslop settings persisted in `~/.makeslop/settings.json`:
  - `makeslop config list` — print current effective settings as `key = value` lines
  - `makeslop config set <key> <value>` — validate, set, persist; echo `key = value` on success
  - bare `makeslop config` prints help
- Solves: today `image`/`shell` are only editable by hand-editing settings.json, and the `/tmp` mount size is hardcoded. This exposes them (plus a new `tmp_dir_size`) through a first-class CLI.
- Integrates with the existing `config.Settings` load/save layer and the pure `docker.BuildSpec` argv assembly.

## Context (from discovery)
- Files/components involved:
  - `internal/config/config.go` — `Settings` already has `Image` (default `claudebox`, `DefaultImage`) and `Shell` (default `/bin/zsh`, `DefaultShell`), both `omitempty` with Load-time defaulting. `Load` tolerates a missing file (returns defaults). `Save` is atomic (temp+rename) and does `os.MkdirAll(baseDir)`. `Bootstrap` seeds the dir tree.
  - `internal/docker/spec.go` — `Options` is the pure input to `BuildSpec`. `Tmpfs: []string{"/tmp:size=100m"}` is hardcoded (~line 112). `BuildSpec`, `Args()`, `ShellCommand()` are pure.
  - `cmd/makeslop/main.go` — `newRootCmd` wires `init`/`go`/`migrate`/`build`. `runGo` builds `docker.Options` from `config.Load`. `errSilent` marks pre-reported errors; other errors print via `"makeslop: %v"`.
  - Tests: `internal/config/config_test.go`, `internal/docker/spec_test.go`, `cmd/makeslop/main_test.go`.
- Related patterns found:
  - Pure/impure split (argv assembly pure & table-tested; exec isolated).
  - `omitempty` + Load-time defaulting keeps existing settings.json byte-stable until overridden.
  - `migrate`/`build` are CI/pipe-safe and exempt from the home-directory guard; `go`/`init` enforce it; TTY check is `go`-only.
- Dependencies identified: `github.com/spf13/cobra`, stdlib only for config logic.

## Development Approach
- **testing approach**: Regular (code first, then tests within the same task)
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
  - write unit tests for new functions/methods
  - write unit tests for modified functions/methods
  - add new test cases for new code paths
  - update existing test cases if behavior changes
  - tests cover both success and error scenarios
- **CRITICAL: all tests must pass before starting next task** — no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- run tests after each change
- maintain backward compatibility: a legacy settings.json is byte-stable only until the first write — any `config set` (or other `Save`) materializes all defaulted fields (`image`/`shell`/`tmp_dir_size`), same as the existing `Save` behavior. Until a write happens, omitempty keeps the on-disk file unchanged.

## Testing Strategy
- **unit tests**: required for every task (see Development Approach).
- **e2e tests**: project has no UI-based e2e harness; not applicable. CLI behavior is covered by `cmd/makeslop/main_test.go` table tests.
- Config tests need no shell shims, so the noexec-`/tmp` + `GOTMPDIR=/home/user` constraint does not apply to them (it still applies to the existing docker shim tests).
- Project test command: `GOTMPDIR=/home/user go test ./...`

## Progress Tracking
- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope

## Solution Overview
- **Approach (Option A): pure key registry in `internal/config`.** A fixed whitelist of settable keys, each described by a `configKey{name, get, set}` table entry. `set` runs per-key validation before mutating, so a bad value never partially writes `Settings`. `config list`, `config set`, and the unknown-key error all derive from the one table (registry order throughout — no separate sorted ordering), so the supported-key set cannot drift.
- snake_case key names matching settings.json json tags: `image`, `shell`, `tmp_dir_size`.
- Logic lives in `internal/config` (pure, table-testable); `cmd/makeslop/main.go` stays thin (Load → mutate → Save → print) — matches the project's pure/impure convention.
- `tmp_dir_size` flows through a new `Options.TmpDirSize` into `BuildSpec`'s `--tmpfs /tmp:size=…` **verbatim** (like `Image`/`Shell` — no re-defaulting inside `BuildSpec`; `Load` is the single source of the default), keeping `BuildSpec` pure and visible under `makeslop go --dry-run`.

## Technical Details
- `Settings` gains `TmpDirSize string \`json:"tmp_dir_size,omitempty"\``; `const DefaultTmpDirSize = "100m"` mirrors the previously-hardcoded value. `Load` defaults it in both the missing-file branch and the post-unmarshal block (like `Image`/`Shell`).
- `tmp_dir_size` validation regex: `^[0-9]+[kKmMgG]?$` — accepts `1000m`, `2g`, `512k`, `1048576`, `100M`; rejects `10mb`, `abc`, ``, `50%`, negatives. **Unit semantics:** a bare number with no suffix is interpreted by docker as **bytes** (e.g. `1048576` = 1 MiB, NOT 1048576 MB). The validation error message and README must state this explicitly so users do not set `512` expecting megabytes.
- `image`/`shell` validation: reject `strings.TrimSpace(v) == ""`; otherwise assign verbatim.
- Public config API: `type ConfigEntry struct { Name, Value string }`; `ConfigList(*Settings) []ConfigEntry` (registry order); `ConfigSet(*Settings, key, val string) error` (unknown key → error listing valid keys in registry order, built from the `configKeys` table). No separate sorted `ConfigKeyNames` — the valid-keys list is derived from the registry inline. Plain `fmt.Errorf` errors (no sentinels) so `main.go` prints them via the existing `"makeslop: %v"` path.
- `config set`/`list` are CI/pipe-safe: no `ttyCheck`, no home-directory guard (consistent with `migrate`/`build`). `config set` does **not** call `Bootstrap` — `Load` tolerates a missing file and `Save` does `MkdirAll`, so it self-heals the dir without seeding the full tree.

## What Goes Where
- **Implementation Steps** (`[ ]`): all code, tests, and doc updates in this repo.
- **Post-Completion** (no checkboxes): manual smoke test of `makeslop config` against a real `~/.makeslop`.

## Implementation Steps

### Task 1: Add `TmpDirSize` setting + defaulting

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

- [x] add `TmpDirSize string \`json:"tmp_dir_size,omitempty"\`` to the `Settings` struct
- [x] add `const DefaultTmpDirSize = "100m"` alongside `DefaultImage`/`DefaultShell`
- [x] default `TmpDirSize` to `DefaultTmpDirSize` in `Load`'s missing-file branch and the post-unmarshal defaulting block
- [x] write tests: `Load` on missing file yields `DefaultTmpDirSize`; `Load` on a file without `tmp_dir_size` yields `DefaultTmpDirSize`
- [x] write tests: Save→Load round-trip preserves a custom `tmp_dir_size`; an on-disk settings.json without the field that is read-only (never re-saved) stays byte-stable (omitempty only protects files that are not written back — assert the read-only case, not a Load→Save cycle)
- [x] run tests — must pass before next task

### Task 2: Thread `tmp_dir_size` into the docker spec

**Files:**
- Modify: `internal/docker/spec.go`
- Modify: `internal/docker/spec_test.go`
- Modify: `cmd/makeslop/main.go`

- [x] add `TmpDirSize string` to `docker.Options`
- [x] change the hardcoded tmpfs line to `Tmpfs: []string{"/tmp:size=" + o.TmpDirSize}` — use `o.TmpDirSize` **verbatim** (no fallback inside `BuildSpec`, matching how `o.Image`/`o.Command` are used); `Load` is the single source of the default
- [x] in `cmd/makeslop/main.go` `runGo`, set `opts.TmpDirSize = s.TmpDirSize` when building `docker.Options` (always non-empty since `Load` defaults it)
- [x] write tests: `BuildSpec` with `TmpDirSize: "1000m"` → `Tmpfs == ["/tmp:size=1000m"]` and `Args()` reflects it
- [x] write tests (separate case): `ShellCommand()` for a custom `tmp_dir_size` renders `--tmpfs /tmp:size=1000m` — this is the user-facing `--dry-run` verification path
- [x] write test: existing default-path tests still see `/tmp:size=100m` via the `Load`-supplied default (no regression in `runGo`)
- [x] run tests — must pass before next task

### Task 3: Pure config key registry + validation

**Files:**
- Create: `internal/config/configkeys.go`
- Create: `internal/config/configkeys_test.go`

- [ ] define `type configKey struct { name string; get func(*Settings) string; set func(*Settings, string) error }`, `type ConfigEntry struct { Name, Value string }`, and `var configKeys` in order: `image`→`setImage`, `shell`→`setShell`, `tmp_dir_size`→`setTmpDirSize`
- [ ] implement `setImage`/`setShell` (reject whitespace-only) and `setTmpDirSize` (regex `^[0-9]+[kKmMgG]?$`; error names the expected form AND states a bare number is bytes); validation runs before mutation
- [ ] implement `ConfigList(*Settings) []ConfigEntry` (registry order) and `ConfigSet(*Settings, key, val string) error` (unknown key → error listing valid keys in registry order, derived from `configKeys`)
- [ ] write tests (table-driven): valid sets per key; `tmp_dir_size` accept (`1000m`,`2g`,`512k`,`1048576`,`100M`) / reject (`10mb`,`abc`,``,`50%`,negative); empty image/shell rejected
- [ ] write tests: unknown key error mentions valid keys; `Settings` left unmutated on any validation failure; `ConfigList` returns the 3 keys in order with effective values
- [ ] run tests — must pass before next task

### Task 4: Wire `config` cobra command (list + set)

**Files:**
- Modify: `cmd/makeslop/main.go`
- Modify: `cmd/makeslop/main_test.go`

- [ ] add `configCmd` (`Use: "config"`, `Args: cobra.NoArgs`, `RunE` → `cmd.Help()`) and register via `rootCmd.AddCommand(configCmd)`
- [ ] add `configListCmd` (`Use: "list"`, `Args: NoArgs`): `Load` → print each `ConfigList` entry as `"%s = %s"`
- [ ] add `configSetCmd` (`Use: "set <key> <value>"`, `Args: ExactArgs(2)`): `Load` → `ConfigSet` → `Save` → echo `"%s = %s"`; no ttyCheck, no home guard, no Bootstrap
- [ ] write tests: `config set image foo` then `config list` shows `image = foo` and it is persisted to settings.json
- [ ] write tests: `config set tmp_dir_size 9z` → exit 1, error on stderr, file unchanged; `config set bogus x` → exit 1 lists valid keys
- [ ] write tests: `config list` on a fresh baseDir prints the three defaults; `config set` works without a prior `init` (self-heal via Save's MkdirAll)
- [ ] run tests — must pass before next task

### Task 5: Verify acceptance criteria
- [ ] verify all Overview requirements implemented: `config list`, `config set <key> <value>` for `image`/`shell`/`tmp_dir_size`, bare `config` prints help
- [ ] verify edge cases: invalid value rejected without mutation, unknown key rejected, missing settings.json self-heals, existing files stay byte-stable until set
- [ ] run full test suite: `GOTMPDIR=/home/user go test ./...`
- [ ] verify `go vet ./...` and any linters used by the project pass

### Task 6: [Final] Update documentation
- [ ] update `README.md` Usage section: document `makeslop config list` and `makeslop config set <key> <value>` (keys: `image`, `shell`, `tmp_dir_size`) alongside `init`/`migrate`/`build`/`go`; for `tmp_dir_size` note the accepted forms (`1000m`, `2g`, `512k`) and that a bare number is interpreted as bytes
- [ ] update `CLAUDE.md`: add `config` to "Home-directory guard exemptions" (exempt) and note under "TTY requirement is go-only" that `config` is CI/pipe-safe
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion
*Items requiring manual intervention or external systems — informational only*

**Manual verification**:
- Run `makeslop config list` against a real `~/.makeslop`, then `makeslop config set tmp_dir_size 1000m` and confirm `makeslop go --dry-run` shows `--tmpfs /tmp:size=1000m`.
- Confirm `makeslop config set image <custom>` is picked up by `makeslop build` (image tag) and `makeslop go` (run image).
