# Replace fd with native WalkDir + config-driven scan filters

## Overview
- Replace the `fd`/`fdfind` external-binary dependency in `internal/security.Scan` with a native Go `filepath.WalkDir` implementation.
- Move the secret-matching **patterns** and directory **skip-dirs** out of hardcoded Go and into the project-local `.makeslop.yaml`, nested under `exclude.scan`.
- Make scanning purely config-driven: the engine has **no hardcoded fallback**. If a project specifies no `exclude.scan.patterns`, nothing is scanned and nothing is masked.
- New projects stay safe because `Scaffold` seeds the default patterns + skip-dirs as **active values** in the generated `.makeslop.yaml`.
- Removes the runtime `fd` install requirement entirely — makeslop becomes self-contained for secret scanning.

## Context (from discovery)
- **Files/components involved:**
  - `internal/security/security.go` — `Scan(ctx, root)` shells out to fd/fdfind with a hardcoded regex denylist and hardcoded `--exclude` dirs (`.git`, `node_modules`, `vendor`, `.venv`). Has `fdBinary` swap var + `ErrFdMissing`.
  - `internal/security/testing.go` — `SetFdBinaryForTest`, `WriteFdShim` (fd shim helpers compiled into prod binary).
  - `internal/security/security_test.go` — fd-shim tests, fd/fdfind PATH probes, denylist pattern tests, "NOT matched" cases.
  - `internal/projectconfig/projectconfig.go` — parses `.makeslop.yaml`; `yamlSchema.Exclude{Dirs,Files}` + `Network`; `Load`, `Stub`, `Scaffold`, helpers `validateEntries`/`statFilter`/`dedupSorted`; `KnownFields(true)`.
  - `internal/projectconfig/projectconfig_test.go` — Stub/Scaffold golden + parse tests.
  - `cmd/makeslop/main.go` — `runGo` calls `security.Scan` (line ~121) **before** `projectconfig.Load` (~135), merges via `mergeUniqueSorted`; `ErrFdMissing` branch (~122-126).
  - `cmd/makeslop/main_test.go` — many `WriteFdShim` / fd-missing / `SkipNonPOSIX("fd shim…")` call sites.
  - `.makeslop.yaml` (repo root) — currently `exclude:\n  dirs: []\n  files: []`.
- **Related patterns found:** pure/impure split (validation pure in `projectconfig`, walk is impure side); `KnownFields(true)` strict decode; scan/parse failure aborts before `docker.Run` (no `.env` leak); POSIX-only invariant; `MigrationVersion` refreshes `~/.makeslop/` only, NOT project-local `.makeslop.yaml`.
- **Dependencies identified:** `gopkg.in/yaml.v3`; stdlib `path/filepath`, `io/fs`. No new external deps; **removes** the `os/exec` usage in `internal/security`.
- **Test command:** `GOTMPDIR=/home/user go test ./...` (shims must land on an executable fs; `/tmp` is noexec here).

## Development Approach
- **testing approach**: Regular (code first, then tests within each task)
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
  - tests cover both success and error scenarios
- **CRITICAL: all tests must pass before starting next task** — run with `GOTMPDIR=/home/user go test ./...`
- maintain backward compatibility where it does not conflict with the deliberate decisions below

## Testing Strategy
- **unit tests**: required for every task (table-driven, matching repo style).
- **e2e tests**: none — this is a CLI/library project with no UI e2e harness. The `cmd/makeslop` tests act as integration tests (real temp trees + real `.makeslop.yaml`).
- run the full suite with `GOTMPDIR=/home/user go test ./...` before advancing tasks.

## Progress Tracking
- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope

## Solution Overview
- **Config schema:** add `exclude.scan.patterns` (basename globs) and `exclude.scan.skip-dirs` (bare directory names) to `.makeslop.yaml`, alongside the unchanged `exclude.files`/`exclude.dirs`. `exclude.scan` drives the walk; `exclude.files`/`dirs` remain explicit user mask paths.
- **Scanner:** `security.Scan(ctx, root, patterns, skipDirs)` walks the tree with `filepath.WalkDir`, prunes skip-dirs, drops symlinks, glob-matches basenames, and returns sorted absolute paths. Empty `patterns` ⇒ `nil` (scan nothing). Walk errors are **propagated** (fail-loud), a deliberate tightening over fd's silent-skip.
- **Defaults home:** the default denylist + skip-dirs live as **active values** in the `Scaffold` stub (and the repo's own `.makeslop.yaml`), not in Go. The engine has no fallback.
- **Wiring:** flip `runGo` order — `projectconfig.Load` first (supplies patterns/skip-dirs), then `security.Scan`. Both failure paths abort before `docker.Run`.

### Key design decisions & rationale
- **Globs over regex** (decided in brainstorm): readable/editable in YAML, stdlib-only (`filepath.Match`), the existing denylist maps cleanly.
- **Nested `exclude.scan`** (decided): structurally separates "walk config" from "explicit mask paths" instead of by convention.
- **No engine fallback** (decided): pure opt-in; "nothing specified ⇒ nothing to scan". New installs safe via seeded scaffold.
- **Propagate walk errors**: an unreadable directory means we cannot prove it is secret-free; failing loud matches the no-`.env`-leak invariant.

## Technical Details

### Config schema (target `.makeslop.yaml`)
```yaml
exclude:
  scan:                # drives the WalkDir scan (replaces fd)
    patterns:          # basename globs → matching files masked (/dev/null)
      - "*.env"
      - ".env.*"
      - "*.pem"
      - "*.key"
      - "id_rsa*"
      - "id_ed25519*"
      - ".npmrc"
      - ".netrc"
      - ".git-credentials"
    skip-dirs:         # directory names pruned during the walk
      - ".git"
      - "node_modules"
      - "vendor"
      - ".venv"
  files: []            # explicit paths → /dev/null (unchanged)
  dirs: []             # explicit paths → tmpfs (unchanged)
network:
  proxy:
    address: ""
```

### Struct changes (`internal/projectconfig/projectconfig.go`)
```go
// yamlSchema.Exclude
Exclude struct {
    Scan struct {
        Patterns []string `yaml:"patterns"`
        SkipDirs []string `yaml:"skip-dirs"`
    } `yaml:"scan"`
    Dirs  []string `yaml:"dirs"`
    Files []string `yaml:"files"`
} `yaml:"exclude"`

// Excludes (result) grows:
type Excludes struct {
    Files    []string
    Dirs     []string
    Patterns []string // basename globs for the walk
    SkipDirs []string // directory names to prune during the walk
}
```

### Scanner signature & behavior (`internal/security/security.go`)
```go
func Scan(ctx context.Context, root string, patterns, skipDirs []string) ([]string, error)
```
1. `len(patterns) == 0` ⇒ return `nil, nil` (no walk).
2. Build `skip := map[string]struct{}` from `skipDirs`.
3. `filepath.WalkDir(root, fn)`:
   - top of `fn`: `if err := ctx.Err(); err != nil { return err }`.
   - if `walkErr != nil` (arg) ⇒ `return walkErr` (propagate, fail-loud).
   - if `d.IsDir()`: if path != root and `d.Name()` in `skip` ⇒ `return filepath.SkipDir`.
   - if `d.Type()&fs.ModeSymlink != 0` ⇒ `return nil` (drop symlink; WalkDir does not follow it).
   - if regular file: for each pattern, `filepath.Match(pat, d.Name())`; first match ⇒ append abs path. (Match error is defensively handled but cannot occur — patterns validated at Load.)
4. `sort.Strings(paths)`; return.
   - Note: WalkDir visits dotfiles and ignores `.gitignore` automatically (replaces fd `--hidden --no-ignore`). Paths are lexically under root by construction (no symlink following), so no `Rel`/`IsLocal` guard needed.

### Validation (in `projectconfig.Load`, pure helpers)
- `patterns`: reject `""`; validate each via `filepath.Match(pattern, "")` — `filepath.ErrBadPattern` ⇒ `projectconfig: invalid scan pattern %q`. No `IsLocal`/reserved checks (patterns are names, not paths). Dedup + sort.
- `skip-dirs`: reject `""`; reject entries containing `os.PathSeparator`/`"/"` or equal to `"."`/`".."` ⇒ `projectconfig: skip-dir %q must be a bare directory name`. Dedup + sort.

## What Goes Where
- **Implementation Steps** (`[ ]`): all code, tests, and in-repo doc/config updates.
- **Post-Completion** (no checkboxes): manual re-migration guidance for users with pre-existing `.makeslop.yaml` files (external/manual action — cannot be automated per the no-MigrationVersion-coverage decision).

## Implementation Steps

### Task 1: Parse & validate `exclude.scan` in projectconfig

**Files:**
- Modify: `internal/projectconfig/projectconfig.go`
- Modify: `internal/projectconfig/projectconfig_test.go`

- [x] add `Scan struct { Patterns []string `yaml:"patterns"`; SkipDirs []string `yaml:"skip-dirs"` } `yaml:"scan"`` to `yamlSchema.Exclude`
- [x] add `Patterns []string` and `SkipDirs []string` fields to the `Excludes` result struct (with doc comments)
- [x] add `validatePatterns(entries []string) ([]string, error)` — reject empty, validate each via `filepath.Match(p, "")`, dedup+sort
- [x] add `validateSkipDirs(entries []string) ([]string, error)` — reject empty, reject separator/`.`/`..`, dedup+sort
- [x] wire both into `Load`, populating `Excludes.Patterns`/`Excludes.SkipDirs`; keep `KnownFields(true)` strict decode (rejects typos under `scan`)
- [x] update package doc header to describe `exclude.scan` (walk config) vs `exclude.files`/`dirs` (explicit mask paths); keep the existing "symlinks silently dropped" note scoped to the *path* lists — scan `patterns` are names, not paths, so don't conflate them
- [x] write tests: valid patterns/skip-dirs parsed, deduped, sorted (success cases)
- [x] write tests: bad glob rejected (`[`), non-bare skip-dir rejected (`foo/bar`, `.`, `..`), empty entry rejected, unknown key under `exclude.scan` rejected by KnownFields (error cases)
- [x] run tests: `GOTMPDIR=/home/user go test ./internal/projectconfig/...` — must pass before next task

### Task 2: Seed default scan filters in the Scaffold stub

**Files:**
- Modify: `internal/projectconfig/projectconfig.go`
- Modify: `internal/projectconfig/projectconfig_test.go`
- Modify: `.makeslop.yaml` (repo root)

- [x] replace the `Stub` var with the populated default schema (patterns + skip-dirs as active values, empty `files`/`dirs`, `network.proxy.address: ""`) — see Technical Details target
- [x] ensure the new `Stub` round-trips through `Load` without error (defaults are valid input)
- [x] update the repo's own `.makeslop.yaml` to the populated defaults
- [x] update tests: `Scaffold` golden compares against new `Stub`; add a test asserting `Load` of the scaffolded stub yields the expected default `Patterns`/`SkipDirs`
- [x] confirm `Scaffold` idempotency test still holds (O_EXCL: existing files not clobbered)
- [x] run tests: `GOTMPDIR=/home/user go test ./internal/projectconfig/...` — must pass before next task

### Task 3: Rewrite security.Scan as a WalkDir walk

**Files:**
- Modify: `internal/security/security.go`

- [x] change signature to `Scan(ctx context.Context, root string, patterns, skipDirs []string) ([]string, error)`
- [x] early-return `nil, nil` when `len(patterns) == 0`
- [x] implement `filepath.WalkDir` walk: ctx cancellation check, propagate walk errors, prune skip-dirs (non-root, by base name), drop symlinks, glob-match regular-file basenames, collect abs paths
- [x] `sort.Strings` results before returning
- [x] remove `fdBinary` var, `ErrFdMissing`, the `os/exec` import, and the PATH-lookup/argv/subprocess block
- [x] rewrite the package doc header: config-driven glob walk (no fd, no in-code denylist)
- [x] run build: `go build ./...` — must compile before writing tests (Scan tests come in Task 4 after callers/helpers settle)

### Task 4: Replace security tests with WalkDir table tests

**Files:**
- Modify: `internal/security/security_test.go`
- Modify/Delete: `internal/security/testing.go`

- [x] delete `SetFdBinaryForTest` and `WriteFdShim` from `testing.go`; if the file becomes empty, delete the file
- [x] remove all fd-shim / `ErrFdMissing` / fd-fdfind PATH-probe tests from `security_test.go`
- [x] add table-driven matcher tests against a real `t.TempDir()` tree: each default glob hits its file; near-misses (`.envrc`, `environment`, `keyfile`, `keyboard.txt`) do NOT match
- [x] add skip-dirs pruning test: a secret planted in `node_modules/` and `.git/` is NOT returned
- [x] add empty-patterns test ⇒ `nil`; add nested/hidden-file-found test; add `.gitignore`d-file-still-found test
- [x] add symlink tests: symlink-to-secret-file is dropped; for a symlinked dir, assert the **target's contents are not walked** (plant a matching secret behind the link and confirm it is absent), not merely that the link basename is gone (guard with `docker.SkipNonPOSIX`)
- [x] add walk-error propagation test: `chmod 0o000` a subdir ⇒ `Scan` returns an error (guard with `docker.SkipNonPOSIX`)
- [x] add under-root invariant test: every path returned by `Scan` satisfies `filepath.IsLocal(rel)` relative to `root` — pins the `internal/docker/spec.go:95` "host is under ProjectRoot" contract
- [x] run tests: `GOTMPDIR=/home/user go test ./internal/security/...` — must pass before next task

### Task 5: Rewire runGo in main.go

**Files:**
- Modify: `cmd/makeslop/main.go`

- [x] move `projectconfig.Load(workspaceRoot)` ahead of the scan; abort on error (preserves no-leak invariant)
- [x] call `security.Scan(cmd.Context(), workspaceRoot, yamlExcludes.Patterns, yamlExcludes.SkipDirs)`; abort on error
- [x] keep the `mergeUniqueSorted(masked, yamlExcludes.Files)` merge
- [x] reposition the `makeslop: masked N secret file(s)` message after the scan; **preserve current semantics** — N counts scan hits (`len(masked)`), not the merged total (no behavior change unless intentionally decided otherwise)
- [x] remove the `ErrFdMissing` branch (the "install fd" hint) and any now-unused imports
- [x] run build: `go build ./...` — must compile before next task

### Task 6: Update cmd/makeslop tests to config-driven masking

**Files:**
- Modify: `cmd/makeslop/main_test.go`

- [x] remove `WriteFdShim` usages, fd-missing assertions, and `SkipNonPOSIX("fd shim…")` gates that only existed for fd
- [x] **delete the `init`-with-fd-shim tests** (~lines 1017-1028 and ~1604-1614): they asserted "init must succeed regardless of fd shim" but `init` never scanned, and they reference symbols deleted in Task 4 — they become moot and would otherwise break the `go test ./cmd/...` compile
- [x] update `go`-command masking tests to write a real `.makeslop.yaml` (with `exclude.scan.patterns`) plus on-disk secret files, then assert those files are masked in the docker argv
- [x] add a test asserting that an empty/absent `exclude.scan` ⇒ no files masked (the opt-in rule), and that `go` no longer errors when fd is absent
- [x] keep `SkipNonPOSIX` only where docker-shim POSIX behavior is still required
- [x] run tests: `GOTMPDIR=/home/user go test ./cmd/...` — must pass before next task

### Task 7: Verify acceptance criteria
- [x] verify all Overview requirements implemented: no `fd`/`os/exec` in `internal/security`; scan driven by `exclude.scan`; empty patterns ⇒ nothing masked; scaffold seeds active defaults
- [x] `grep -rn "fd\|fdfind\|ErrFdMissing\|WriteFdShim\|SetFdBinaryForTest" --include="*.go" .` returns only intentional/no hits
- [x] verify edge cases: bad glob/skip-dir rejected at Load; walk error aborts launch; symlinks dropped
- [x] run full test suite: `GOTMPDIR=/home/user go test ./...`
- [x] run linter: `golangci-lint run` (config `.golangci.yml`)

### Task 8: [Final] Update documentation
- [x] update `README.md`: document the `exclude.scan` schema; remove the "install fd" prerequisite; **rewrite the now-false safety claims** — "Secret masking is non-negotiable… refuses to launch" (~lines 145-150) and "the secret auto-scan… still runs unconditionally" (~line 181) — to describe the opt-in, config-driven behavior (empty patterns ⇒ nothing masked); add the manual re-migration caveat for pre-existing projects
- [x] update `CLAUDE.md`: remove the "fd shim requires POSIX shell" note and the fd-shim noexec-`/tmp` note; KEEP the build-context-dir `/tmp` note; add a note that scan filters are config-driven (no in-code denylist, no engine fallback) and that pre-existing project files are not auto-migrated
- [x] move this plan to `docs/plans/completed/`

## Post-Completion
*Items requiring manual intervention or external systems — no checkboxes, informational only*

**Manual verification:**
- Sanity-check on a real project: `makeslop init` a fresh dir, confirm the generated `.makeslop.yaml` contains the seeded defaults, drop a `.env`, run `makeslop go --dry-run`, and confirm the `.env` appears as a `/dev/null` mask in the printed argv.
- Confirm `makeslop go` succeeds on a machine with **no** `fd`/`fdfind` installed (the whole point of the change).

**External / user-facing migration (cannot be automated):**
- Users with a project `.makeslop.yaml` created before this change have the old empty stub → no `exclude.scan.patterns` → secret masking silently stops. They must manually add an `exclude.scan` block (copy the defaults from the README) to restore masking. This is **not** covered by `MigrationVersion` (which only refreshes `~/.makeslop/`, never the project-local file). Call this out in release notes.
