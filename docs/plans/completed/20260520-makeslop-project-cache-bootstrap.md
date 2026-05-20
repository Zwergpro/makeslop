# Makeslop Project Cache Bootstrap

## Overview
First milestone of the `makeslop` CLI: establish the on-disk cache layout under `~/.makeslop/` and the project-resolution algorithm that maps the current working directory to a per-project cache directory. For this milestone the binary exposes two behaviors:

- `makeslop init` — register the current directory as a project (if it isn't already covered by an ancestor), create `~/.makeslop/` layout if missing, create the project's cache dir, persist settings, print the project cache path, exit 0.
- `makeslop` (no args) — look up the project for the current directory by walking ancestors. If a registered project covers pwd, print its cache path and exit 0. If none does, print an error to stderr suggesting `makeslop init` and exit non-zero. **Never modifies state.**

Docker execution and additional subcommands land in later milestones.

This is the foundation every subsequent feature relies on: per-project state has to live somewhere stable and predictable before we can cache build artifacts, logs, or container outputs.

## Context (from discovery)
- Go module `github.com/Zwergpro/makeslop`, Go 1.26.2.
- `cmd/makeslop/main.go` is a hello-world stub — entry point is ours to define.
- `go.mod` already requires `github.com/spf13/cobra v1.10.2` (currently marked indirect; will become direct once used).
- No existing `internal/` packages.
- No existing tests, no test command documented yet — use standard `go test ./...`.

## Development Approach
- **testing approach**: Regular (code first, then tests within the same task)
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
  - tests are not optional - they are a required part of the checklist
  - write unit tests for new functions/methods
  - write unit tests for modified functions/methods
  - add new test cases for new code paths
  - update existing test cases if behavior changes
  - tests cover both success and error scenarios
- **CRITICAL: all tests must pass before starting next task** - no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- run `go test ./...` and `go vet ./...` after each change
- maintain backward compatibility (n/a for this milestone — greenfield)

## Testing Strategy
- **unit tests**: required for every task. Use the standard library `testing` package (no test framework dep yet). Drive `internal/cache` tests by passing a `t.TempDir()` as the base directory — never touch the real `~/.makeslop` from tests.
- **integration test**: one end-to-end test in `cmd/makeslop` that runs the cobra root command against a temp HOME via the injected base dir and asserts the printed path + on-disk layout.
- **no e2e/UI tests**: not applicable, CLI only.

## Progress Tracking
- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope
- keep plan in sync with actual work done

## Solution Overview

Single binary with a cobra root command (lookup-only) and one subcommand (`init`).

Shared resolution step (used by both commands):
1. Resolve pwd to an absolute, symlink-evaluated path.
2. Load `~/.makeslop/settings.json` (defaulting to an empty in-memory `{version:1, projects:{}}` if the file doesn't exist — but **only `init` ever writes it back**).
3. Walk pwd and its ancestors upward; first registered ancestor wins.

`makeslop` (root, no args):
- If walk finds a match → print `~/.makeslop/projects/<name>` and exit 0.
- If not → print to stderr: `makeslop: no project registered for <pwd>; run 'makeslop init' to register it` and exit 1. No filesystem mutation.

`makeslop init`:
- If walk finds a match → print that existing project's cache path and exit 0 (idempotent, no mutation, no warning — covers re-running `init` in a subdir of an already-registered project).
- If not → create `~/.makeslop/` and `~/.makeslop/projects/` if missing, generate `name = <basename>-<sha256(absPath)[:6]>`, create `~/.makeslop/projects/<name>/`, insert entry into settings, persist, print the new cache path, exit 0.

All cache I/O lives behind a `Cache` type in `internal/cache` that takes the base directory as a constructor argument — this is the seam tests use to avoid hitting the real home directory.

### Key design decisions

- **Lookup-only default vs explicit `init`** — bare `makeslop` never mutates state; registration is an opt-in action via `init`. This avoids silently creating cache entries when the user mistypes a path or runs the tool from the wrong directory.
- **`init` is idempotent** — running it inside an already-registered project (or subdir thereof) is a no-op that just prints the existing path. Re-running `init` should never error.
- **Map keyed by absolute path** in `settings.json` — O(1) lookup, natural uniqueness, no special list-scanning code.
- **First-match-wins ancestor walk** — running in a subdir of a registered project reuses that project. Running `init` in a parent of a registered project DOES register a new project (overlapping roots are allowed; explicitly chosen for simplicity).
- **No file locking** on `settings.json` — single-user CLI, concurrent invocation is not a real scenario yet.
- **No config migrations** — `version: 1` is the floor; migration logic is YAGNI until v2 exists.
- **Symlink-evaluated paths** — `filepath.EvalSymlinks(pwd)` ensures `/var/foo` and `/private/var/foo` map to the same project on macOS-style symlinked roots. **Invariant**: every path stored as a key in `settings.Projects` is an absolute, EvalSymlinks-evaluated path. Both `Cache.Lookup` and `Cache.Init` rely on callers passing an EvalSymlinks-evaluated `pwd`; this is enforced by the CLI layer (cobra commands evaluate before calling) and documented on the method godoc. Tests must exercise the case where `t.TempDir()` resolves to a different path than its raw form (e.g. `/tmp` → `/private/tmp` on macOS).
- **POSIX-only** — this milestone targets POSIX filesystems. Absolute paths as JSON map keys work fine on Linux/macOS but interact awkwardly with Windows path casing and separators. Windows support is explicitly out of scope.

## Technical Details

### Directory layout (created on demand)

```
~/.makeslop/
├── settings.json
└── projects/
    └── <basename>-<sha256[:6]>/
```

### settings.json schema

```json
{
  "version": 1,
  "projects": {
    "/workspace/makeslop": {
      "name": "makeslop-a3f1c7",
      "created_at": "2026-05-20T16:45:00Z"
    }
  }
}
```

- `version` (int): schema version, currently `1`.
- `projects` (map\[string\]Project): key is the absolute, symlink-evaluated project root path.
- `Project.name` (string): `<basename>-<hash>`. Used as the directory name under `projects/`.
- `Project.created_at` (RFC3339 string): timestamp of first registration. Informational.

### Cache API surface

```go
// internal/cache/cache.go
package cache

type Project struct {
    Name      string    `json:"name"`
    CreatedAt time.Time `json:"created_at"`
}

type Settings struct {
    Version  int                `json:"version"`
    Projects map[string]Project `json:"projects"`
}

type Cache struct {
    baseDir string // e.g. ~/.makeslop
}

// ErrNotRegistered is returned by Lookup when no ancestor of pwd is registered.
var ErrNotRegistered = errors.New("no project registered for path")

func New(baseDir string) *Cache
func DefaultBaseDir() (string, error) // resolves ~/.makeslop from os.UserHomeDir

// Lookup walks pwd's ancestors and returns the cache dir of the first registered
// project found. Returns ErrNotRegistered if none match. Never mutates state.
// pwd MUST be an absolute, EvalSymlinks-evaluated path; callers are responsible.
func (c *Cache) Lookup(pwd string) (projectDir string, err error)

// Init returns the cache dir for pwd: existing if an ancestor is already
// registered (idempotent), otherwise registers pwd as a new project, creates
// directories, persists settings, and returns the new cache dir.
// pwd MUST be an absolute, EvalSymlinks-evaluated path; callers are responsible.
func (c *Cache) Init(pwd string) (projectDir string, err error)
```

`Lookup` and `Init` are the only exported operations on `Cache`. Settings load/save and project-naming stay unexported. The ancestor-walk logic is a single private helper used by both — duplication is avoided here because the algorithm is identical and non-trivial.

### Project name derivation

```
name = filepath.Base(absPath) + "-" + hex(sha256(absPath))[:6]
```

If `Base` is `/` (project root is `/`), use `"root"` as the basename to keep the name non-empty.

### Processing flow

Root (`makeslop`):
```
main → rootCmd.RunE
  → cache.DefaultBaseDir() → cache.New(baseDir)
  → os.Getwd() → filepath.Abs → filepath.EvalSymlinks
  → cache.Lookup(pwd)
      → ok  → fmt.Fprintln(cmd.OutOrStdout(), projectDir); exit 0
      → ErrNotRegistered → fmt.Fprintf(cmd.ErrOrStderr(), "makeslop: no project registered for %s; run 'makeslop init' to register it\n", pwd); exit 1
      → other err → return err (cobra prints + exit 1)
```

`init` (`makeslop init`):
```
main → initCmd.RunE
  → cache.DefaultBaseDir() → cache.New(baseDir)
  → os.Getwd() → filepath.Abs → filepath.EvalSymlinks
  → cache.Init(pwd) → projectDir
  → fmt.Fprintln(cmd.OutOrStdout(), projectDir); exit 0
```

`SilenceUsage: true` and `SilenceErrors: true` on both commands so the "not registered" hint is the only stderr output for the common error path (no cobra usage dump, no duplicated `Error:` prefix).

## What Goes Where
- **Implementation Steps** (`[ ]` checkboxes): all code, tests, and `go.mod` adjustments live in this repo and are tracked below.
- **Post-Completion** (no checkboxes): manual smoke test in a real shell against the real `~/.makeslop` — left to the user since automated tests use a temp dir.

## Implementation Steps

### Task 1: Cache package — settings load/save

**Files:**
- Create: `internal/cache/cache.go`
- Create: `internal/cache/cache_test.go`

- [x] create package `cache` with `Project`, `Settings`, `Cache` types as defined in Technical Details
- [x] implement `New(baseDir string) *Cache` and unexported `(c *Cache) loadSettings() (*Settings, error)` — returns an empty `{version: 1, projects: {}}` if `settings.json` doesn't exist; returns wrapped error on malformed JSON
- [x] implement unexported `(c *Cache) saveSettings(s *Settings) error` — `os.MkdirAll(baseDir, 0o755)`, write to a temp file **inside `baseDir`** (so rename is intra-device), then `os.Rename` to `settings.json`. Indented JSON via `json.MarshalIndent` 2-space
- [x] implement `DefaultBaseDir() (string, error)` using `os.UserHomeDir` joined with `.makeslop`
- [x] write tests: load returns empty defaults when missing; save→load round-trip preserves data; load returns error on malformed JSON; save creates `baseDir` if absent; save followed by load yields byte-identical content for the same settings
- [x] write tests: `DefaultBaseDir` honors `$HOME` (set via `t.Setenv`)
- [x] run `go test ./internal/cache/... && go vet ./...` — must pass before task 2

### Task 2: Cache package — project name derivation

**Files:**
- Modify: `internal/cache/cache.go`
- Modify: `internal/cache/cache_test.go`

- [x] add unexported `projectName(absPath string) string` returning `<basename>-<sha256(absPath)[:6]>`, with basename `"root"` when `filepath.Base(absPath) == string(filepath.Separator)`. Callers always pass a non-empty absolute path; document this in a one-line comment on the function
- [x] write table-driven tests covering: normal nested path, single-segment path, root `/`, paths with spaces, paths with unicode — assert stable hash and expected basename
- [x] write test asserting the hash is deterministic across calls for the same input
- [x] run `go test ./internal/cache/... && go vet ./...` — must pass before task 3

### Task 3: Cache package — Lookup and Init

**Files:**
- Modify: `internal/cache/cache.go`
- Modify: `internal/cache/cache_test.go`

- [x] define exported `ErrNotRegistered = errors.New("no project registered for path")`
- [x] implement unexported `(c *Cache) findAncestor(s *Settings, pwd string) (matchedPath string, project Project, ok bool)`: walk from `pwd` upward via `filepath.Dir`, stopping when parent equals current (filesystem root reached); first ancestor present in `s.Projects` wins
- [x] implement `(c *Cache) Lookup(pwd string) (string, error)`:
  - load settings (treat missing file as empty)
  - call `findAncestor`; if `ok`, return `filepath.Join(baseDir, "projects", project.Name)`, else return `"", ErrNotRegistered`
  - never write to disk, never create directories
- [x] implement `(c *Cache) Init(pwd string) (string, error)`:
  - load settings (treat missing file as empty)
  - call `findAncestor`; if `ok`, return `filepath.Join(baseDir, "projects", project.Name)` without mutating anything (idempotent)
  - otherwise: `proj := Project{Name: projectName(pwd), CreatedAt: time.Now().UTC()}`; `s.Projects[pwd] = proj`; `os.MkdirAll(filepath.Join(baseDir, "projects", proj.Name), 0o755)`; save settings; return `filepath.Join(baseDir, "projects", proj.Name)`
- [x] write Lookup tests using `t.TempDir()` as base dir:
  - missing settings.json → `ErrNotRegistered`, no files created
  - settings with no matching ancestor → `ErrNotRegistered`, settings unchanged byte-for-byte
  - settings with exact pwd match → returns expected cache path
  - settings with parent registered → returns parent's cache path, settings unchanged
  - corrupt settings.json → wrapped error (not `ErrNotRegistered`)
- [x] write Init tests using `t.TempDir()` as base dir:
  - fresh init creates settings, projects dir, returns expected path
  - init from a subdir of a registered project returns the parent's cache dir and does NOT add a new entry (verify by snapshotting settings.json bytes before/after)
  - init from a parent of a registered project DOES register a new project (overlap allowed by design)
  - init from a sibling registers a new project
  - second init on same path is byte-equal no-op
  - corrupt settings.json produces a wrapped error
- [x] run `go test ./internal/cache/... && go vet ./...` — must pass before task 4

### Task 4: Wire cobra root command and `init` subcommand

**Files:**
- Modify: `cmd/makeslop/main.go`
- Create: `cmd/makeslop/main_test.go`
- Modify: `go.mod` (cobra promoted from indirect → direct; `go mod tidy`)

- [x] expose a constructor `newRootCmd(baseDir string) *cobra.Command` so tests can inject a temp base dir. `main()` calls `cache.DefaultBaseDir()` then `newRootCmd(baseDir).Execute()`
- [x] root command: `Use: "makeslop"`, short description "Run docker-based project commands with per-project cache", `Args: cobra.NoArgs`, `SilenceUsage: true`, `SilenceErrors: true`. `RunE` performs: os.Getwd → filepath.Abs → filepath.EvalSymlinks → `cache.New(baseDir).Lookup(pwd)`. On success print path to `cmd.OutOrStdout()`. On `errors.Is(err, cache.ErrNotRegistered)` write the hint message to `cmd.ErrOrStderr()` and return the same error — `SilenceErrors: true` suppresses cobra's duplicate print, and `main()` exits 1 on any non-nil error from Execute
- [x] `init` subcommand: `Use: "init"`, short "Register the current directory as a makeslop project", `Args: cobra.NoArgs`, `SilenceUsage: true`. `RunE` performs the same pwd resolution then `cache.New(baseDir).Init(pwd)` and prints the returned path
- [x] keep `main()` minimal: build root cmd with default base dir, execute, exit 1 on error
- [x] integration tests in `cmd/makeslop/main_test.go` (all use `t.TempDir()` for base dir, capture stdout/stderr via `cmd.SetOut` / `cmd.SetErr`, set args via `cmd.SetArgs`):
  - bare `makeslop` with no prior settings → exit error, stderr contains "no project registered" and "run 'makeslop init'", no files created under base dir
  - `makeslop init` from scratch → prints path under `<baseDir>/projects/`, directory exists on disk, settings.json contains pwd entry
  - `makeslop` after `makeslop init` → prints the same path, no settings mutation
  - `makeslop init` twice in a row → same path, second run is byte-equal no-op on settings.json
  - `makeslop init` in a subdir of a registered project → prints parent's cache path, settings unchanged
  - symlink invariant: on a host where `t.TempDir()` resolves through a symlink (e.g. `/tmp` → `/private/tmp`), `makeslop init` then a second `makeslop init` from the chdir-raw path must be a byte-equal no-op on settings.json — proves both cobra commands EvalSymlinks before calling Cache, and that the stored key is the evaluated path
- [x] for the tests, drive pwd by `t.Chdir(...)` (Go 1.24+) into temp directories — do NOT rely on the test process's actual cwd
- [x] run `go mod tidy && go build ./... && go test ./... && go vet ./...` — must pass before task 5

### Task 5: Verify acceptance criteria

- [x] verify all bullet points in Overview are implemented (cache layout, settings.json, ancestor walk, `init` registers, bare command is lookup-only)
- [x] verify edge cases: pwd at filesystem root, pwd with unicode, pwd via symlink, pre-existing valid settings.json, missing settings.json, corrupt settings.json
- [x] verify bare `makeslop` never writes to disk: snapshot `<baseDir>` before/after a "not registered" invocation and assert no files appeared
- [x] `go test ./... -race -count=1` passes
- [x] `go vet ./...` clean
- [x] manual smoke in `/workspace/makeslop`:
  - `go run ./cmd/makeslop` → error to stderr suggesting `makeslop init`, exit 1
  - `go run ./cmd/makeslop init` → prints path under `~/.makeslop/projects/`, directory exists
  - `go run ./cmd/makeslop` → prints the same path as the prior `init`
  - second `go run ./cmd/makeslop init` → same path, no settings.json mutation

### Task 6: [Final] Update documentation and finalize

**Files:**
- Modify: `README.md`
- Modify: `CLAUDE.md`

- [x] update `README.md` with a short paragraph: what `makeslop` is, the `~/.makeslop/` layout, the `init` vs default behavior, how to build/run
- [x] update `CLAUDE.md` with the canonical test command (`go test ./...`) and the package layout (`cmd/makeslop`, `internal/cache`)
- [x] move this plan to `docs/plans/completed/`: `mkdir -p docs/plans/completed && git mv docs/plans/20260520-makeslop-project-cache-bootstrap.md docs/plans/completed/`

## Post-Completion
*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification:**
- Run `makeslop` in a fresh shell against a real `$HOME` (not a temp dir) to confirm `~/.makeslop/` is created with the right permissions and the printed path resolves on disk.
- Run `makeslop` from a deeply nested subdirectory of a previously registered project to confirm reuse behavior in practice.
- Inspect `~/.makeslop/settings.json` manually after a few runs to confirm the schema is readable and the hashes look right.

**Follow-up milestones (out of scope here, listed only so they aren't forgotten):**
- Docker image invocation in the project cache.
- `list` / `remove` / `prune` subcommands for managing registered projects.
- File locking on `settings.json` if/when concurrent invocations become a real concern.
- Config migration scaffolding when a `version: 2` schema is introduced.
