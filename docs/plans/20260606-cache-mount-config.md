# Configurable Per-Workspace Cache Mount Overlays

## Overview
`docker.BuildSpec` (`internal/docker/spec.go:97-106`) mounts 8 agent paths over the
project. Four of them are **per-workspace cache overlays** seeded empty by
`scaffoldTemplate` (`internal/workspace/workspace.go:81`) and mounted on top of the
user's real project files:

- **content group** — `docs/` + `CLAUDE.md`
- **agent-state group** — `.claude/` + `.codex/`

When a project ships its own `docs/`, the empty cache `docs/` shadows it inside the
container. This change lets users disable either group:

- **per-project, declaratively** — a new `cache:` block in `.makeslop.yaml`
- **at init time** — a new `makeslop init --global-only` flag that scaffolds the YAML
  with both groups disabled (only the global `~/.makeslop` mounts remain)

Both default to today's behavior (everything mounted), so every existing config is
unaffected.

## Context (from discovery)
- **Project**: Go CLI (`makeslop`) wrapping `docker run` with a per-workspace cache.
- **Files involved**:
  - `internal/projectconfig/projectconfig.go` — YAML schema, `Load`, `Scaffold`, `Stub`
  - `internal/docker/spec.go` — pure `BuildSpec` (the 8 mounts), `Options`
  - `cmd/makeslop/main.go` — `runRun` wiring + `init` command
  - `cmd/makeslop/status.go` — **second** `projectconfig.Load` caller (discovered)
- **Related patterns**:
  - Pure/impure split: `BuildSpec` is pure and table-tested; keep it pure.
  - `*bool` pointer trick to distinguish "absent" from "explicit false".
  - Flag-scope convention: `--out-of-home`/`--proxy` are registered on specific
    subcommands only, and other commands reject them as unknown flags.
  - Strict decode (`KnownFields(true)`) already rejects unknown/typo keys.
- **Dependencies / ripple discovered during planning**:
  - `projectconfig.Load` has **two** production callers, not one:
    `cmd/makeslop/main.go:153` and `cmd/makeslop/status.go:279`. Both must be updated
    for the widened signature.
  - `projectconfig.Scaffold` is also called by `internal/security/security_test.go:55`
    — that test must be updated for the new parameter.
  - Test callers of `Load`: `cmd/makeslop/main_test.go`, `internal/security/security_test.go:58`.

## Development Approach
- **testing approach**: Regular (code first, then tests) — matches the existing
  table-driven test style in this repo.
- complete each task fully before moving to the next
- make small, focused changes
- **every task includes new/updated tests** (success + error/edge cases)
- **all tests must pass before starting the next task**: `go test ./...`
- maintain backward compatibility (absent `cache:` block ⇒ both groups mounted)

## Testing Strategy
- **unit tests**: required per task. The repo is CLI/library only — no UI e2e tests.
- **drift-guard**: `spec_test.go`'s `Args()`-vs-`HostConfig()` cross-check runs on a
  single hardcoded `sampleOptions()` spec (`spec_test.go:1155`) — it does **not**
  parameterize over `Options`, so the new combos are **not** covered automatically.
  Task 3 extends coverage explicitly and updates `sampleOptions()`.
- **gated integration test** (`MAKESLOP_DOCKER_IT=1`) is unaffected and out of scope.

## Progress Tracking
- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- keep this plan in sync with actual work

## Solution Overview
Single source of truth = the project `.makeslop.yaml`. The `init --global-only` flag is
pure convenience: it scaffolds the file with the cache groups disabled. At runtime,
`projectconfig.Load` returns a new `Cache{Content, Agent bool}` (defaulting both to
`true`), which flows into two new `docker.Options` booleans. `BuildSpec` conditionally
emits the two per-workspace mount groups; global mounts are always present and mount
order is preserved (entries are omitted, never reordered).

## Technical Details

### Config shape
```yaml
cache:
  content: true   # mount docs/ + CLAUDE.md from per-workspace cache
  agent: true     # mount .claude/ + .codex/ from per-workspace cache
```

### Schema / default resolution
- `yamlSchema.Cache.Content *bool` / `.Agent *bool` (pointer ⇒ absent vs explicit-false).
- `Content: schema.Cache.Content == nil || *schema.Cache.Content` (same for `Agent`).
- Absent block ⇒ `Cache{true, true}` ⇒ identical to current behavior.

### Mount grouping in `BuildSpec`
- **global** (always): `BaseDir/.claude/`, `BaseDir/.claude.json`, `BaseDir/.codex/`.
- **agent-state** (`if o.MountAgentCache`): `workspaceHost/.claude/`, `workspaceHost/.codex/`.
- **content** (`if o.MountContentCache`): `workspaceHost/docs/`, `workspaceHost/CLAUDE.md`.
- Masked files (`/dev/null`) and masked dirs (tmpfs) still append after, so a masked
  path under `docs/` still wins.

### Flag semantics
- `--global-only` only affects a **fresh** scaffold. `Scaffold` is idempotent
  (EEXIST = success, never clobbers user edits), so on an already-init'd project the
  flag is a no-op — documented, not silent.

### No version bump
`Settings` struct fields and the embedded Dockerfile are unchanged ⇒ neither
`CurrentVersion` nor `MigrationVersion` changes (same reasoning as the existing proxy
notes in CLAUDE.md).

## What Goes Where
- **Implementation Steps**: all code, tests, and doc updates live in this repo.
- **Post-Completion**: a manual sanity run of `makeslop init --global-only` against a
  real project (requires a Docker daemon) — informational only.

## Implementation Steps

### Task 1: Parse the `cache:` block in projectconfig

**Files:**
- Modify: `internal/projectconfig/projectconfig.go`

- [x] add `Cache` sub-struct to `yamlSchema`: `Content *bool` / `Agent *bool` with
      `yaml:"content"` / `yaml:"agent"` tags, under a `yaml:"cache"` key
- [x] add exported result type `type Cache struct { Content bool; Agent bool }` with
      doc comments describing the two groups
- [x] widen `Load` to return `(Excludes, Network, Cache, error)`; resolve defaults via
      the `== nil || *ptr` idiom (absent ⇒ `true`); return `Cache{}` on every error path
- [x] update the package doc comment to describe the `cache:` block alongside
      `exclude:` and `network:`
- [x] write tests in `internal/projectconfig/projectconfig_test.go`: absent block ⇒
      `{true,true}`; `content:false`+`agent:false` ⇒ `{false,false}`; mixed
      `content:false` with `agent` absent ⇒ `{false,true}`
- [x] write error-case test: unknown key under `cache:` ⇒ strict-decode error
- [x] run `go test ./internal/projectconfig/` — must pass before next task

### Task 2: Parameterize Scaffold/Stub with cache defaults

**Files:**
- Modify: `internal/projectconfig/projectconfig.go`

- [x] add `func renderStub(c Cache) []byte` that renders the existing stub content plus
      a `cache:\n  content: %t\n  agent: %t` block placed **between** `exclude:` and
      `network:`
- [x] redefine `var Stub = renderStub(Cache{Content: true, Agent: true})` (stays
      exported; now the default rendering)
- [x] change `Scaffold(root string)` → `Scaffold(root string, c Cache)` writing
      `renderStub(c)`; preserve idempotency (EEXIST = success, partial-write cleanup)
- [x] write test: `renderStub(Cache{true,true})` round-trips through `Load` to
      `{true,true}`; `renderStub(Cache{false,false})` round-trips to `{false,false}`
- [x] write test: scaffolded file from `Scaffold(root, Cache{false,false})` parses to
      `{false,false}`
- [x] note: `Stub`'s rendered bytes now include the `cache:` block. Confirm callers that
      compare against the `Stub` **variable** (`main_test.go:586,1526`) stay green (they
      do — they reference the variable, not a literal); update any hardcoded-literal stub
      assertion if one surfaces
- [x] run `go test ./internal/projectconfig/` — must pass before next task

### Task 3: Conditional cache mounts in BuildSpec

**Files:**
- Modify: `internal/docker/spec.go`

- [x] add `MountContentCache bool` and `MountAgentCache bool` to `docker.Options` with
      doc comments
- [x] split the mount slice in `BuildSpec`: keep the project-root bind + 3 global mounts
      unconditional; gate the two agent-state mounts behind `if o.MountAgentCache` and
      the two content mounts behind `if o.MountContentCache` (omit only, never reorder)
- [x] update the `BuildSpec` doc comment to note the two per-workspace groups are
      config-gated while global mounts are always present
- [x] ⚠️ update `sampleOptions()` (`internal/docker/spec_test.go:13`) to set
      `MountContentCache: true, MountAgentCache: true` — the new fields default to `false`,
      which would otherwise drop 4 mounts and break every existing full-mount assertion
      (e.g. `TestBuildSpec_MountListMatchesReferenceOrder`) and the drift guard
- [x] write table-driven tests in `internal/docker/spec_test.go` covering all 4 combos
      of `MountContentCache`/`MountAgentCache`, asserting the exact mount set per combo
      (content off ⇒ no `docs/`+`CLAUDE.md`; agent off ⇒ no `.claude/`+`.codex/`; global
      always present)
- [x] confirm the existing `Args()`-vs-`HostConfig()` drift-guard test exercises the new
      combos (extend its cases if it is not parameterized over `Options`)
- [x] run `go test ./internal/docker/` — must pass before next task

### Task 4: Wire Cache through runRun and status (Load callers)

**Files:**
- Modify: `cmd/makeslop/main.go`
- Modify: `cmd/makeslop/status.go`
- Modify: `internal/security/security_test.go`

- [x] `cmd/makeslop/main.go:153` — capture the new `Cache` return from
      `projectconfig.Load`; set `opts.MountContentCache = cacheCfg.Content` and
      `opts.MountAgentCache = cacheCfg.Agent` at the `docker.Options` literal (~line 206)
- [x] `cmd/makeslop/status.go:279` — update the `projectconfig.Load` call for the new
      4th return value (discard the `Cache` with `_` unless a status check needs it)
- [x] `internal/security/security_test.go:55` — update the `Scaffold` call to pass
      `projectconfig.Cache{Content: true, Agent: true}`; update the
      `projectconfig.Load` call at line 58 for the 4th return value
- [x] write/extend a test in `cmd/makeslop/main_test.go`: a `.makeslop.yaml` with
      `cache: {content:false, agent:false}` produces a `docker.Options` (or dry-run
      `ShellCommand`) with the cache mounts absent; absent block keeps them present
- [x] run `go test ./...` — must pass before next task

### Task 5: Add the `init --global-only` flag

**Files:**
- Modify: `cmd/makeslop/main.go`

- [x] declare `var globalOnly bool` in `newRootCmd` alongside the other flag vars
- [x] register on `initCmd` only:
      `initCmd.Flags().BoolVar(&globalOnly, "global-only", false, "scaffold .makeslop.yaml with project cache overlays disabled (only global ~/.makeslop mounts)")`
- [x] update the `Scaffold` call (`main.go:353`) to
      `projectconfig.Scaffold(workspaceRoot, projectconfig.Cache{Content: !globalOnly, Agent: !globalOnly})`
- [x] write test in `cmd/makeslop/main_test.go`: `init --global-only` writes a YAML that
      `Load`s to `{false,false}`; `init` without the flag ⇒ `{true,true}`
- [x] write scope test: `--global-only` is rejected as an unknown flag on
      `run`/`build`/`migrate`/`config`/`status`/`version` (mirror the existing
      `--out-of-home` scope tests)
- [x] run `go test ./...` — must pass before next task

### Task 6: Verify acceptance criteria
- [ ] absent `cache:` block ⇒ byte-identical mounts to pre-change behavior
- [ ] `content:false` lets project `docs/`+`CLAUDE.md` show through; `agent:false` lets
      project `.claude/`+`.codex/` show through; global mounts unaffected in all cases
- [ ] `--global-only` is a no-op on an already-scaffolded project (no clobber)
- [ ] run full suite: `go test ./...`
- [ ] confirm no `CurrentVersion`/`MigrationVersion` change crept in

### Task 7: [Final] Update documentation
- [ ] `docs/reference.md` — document the `cache:` block and the `init --global-only` flag
      (including the no-op-on-existing-file caveat)
- [ ] `docs/architecture.md` — describe the three mount groups (global / agent-state
      cache / content cache) and that the latter two are config-gated
- [ ] `CLAUDE.md` — add `--global-only` to the flag-scope section; note the `cache:`
      block in the projectconfig description; record that this change does **not** bump
      `CurrentVersion`/`MigrationVersion` (Settings struct & embedded Dockerfile
      unchanged)
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion
*Informational only — no checkboxes.*

**Manual verification** (requires a Docker daemon):
- In a throwaway project that ships its own `docs/`, run `makeslop init --global-only`,
  then `makeslop run`, and confirm the project's real `docs/` is visible inside the
  container (not the empty cache overlay).
- Confirm an existing project with a hand-edited `cache:` block behaves per the YAML.
