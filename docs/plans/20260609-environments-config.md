# Add `environments` section to `.makeslop.yaml`

## Overview
- Add an optional top-level `environments:` block to the project-local `.makeslop.yaml` that lets a user declare static environment variables to inject into the app container.
- Solves: users currently have no way to pass build/runtime config (e.g. `NODE_ENV`, `LOG_LEVEL`, an API base URL) into the agent container without editing global settings or the Dockerfile.
- Integrates by flowing the parsed vars through `projectconfig.Load` → `docker.Options` → `docker.BuildSpec` → both the argv projection (`Args`/`ShellCommand`) and the SDK projection (`ContainerConfig`), exactly mirroring how `MaskedFiles`/cache mounts already flow.

## Context (from discovery)
- **Project**: `makeslop`, Go. Pure/impure split in `internal/docker` (pure argv/spec assembly in `spec.go`; side-effecting SDK calls in `run.go`/`build.go`).
- **Files involved**:
  - `internal/projectconfig/projectconfig.go` — strict YAML decode (`KnownFields(true)`), fail-loud validation helpers (`validatePatterns`, `validateSkipDirs`, `validateEntries`), `dedupSorted`.
  - `internal/docker/spec.go` — `Options`, `Spec`, `BuildSpec`, `Args`, `ShellCommand`, `ContainerConfig`, `HostConfig`.
  - `cmd/makeslop/main.go` (~line 123, 143) — `runRun` wires `projectconfig.Load` into `docker.Options`.
  - `cmd/makeslop/status.go` (~line 280) — `runStatus` calls `projectconfig.Load`, discards cache with `_`.
  - `internal/projectconfig/projectconfig_test.go`, `internal/docker/spec_test.go` — table-driven tests + a drift-guard test.
  - **⚠️ All `Load` callers** (the signature change touches every one — verified via `grep -rn "projectconfig.Load"`):
    - `cmd/makeslop/main.go:123` (prod) — `yamlExcludes, cacheCfg, err`
    - `cmd/makeslop/status.go:280` (prod) — `yamlExcludes, _, pcErr`
    - `cmd/makeslop/main_test.go:3713` and `:3739` (test) — `_, cache, err`
    - `internal/security/security_test.go:58` (test, **different package**) — `excl, _, err`
    - The plan's earlier "only main.go and status.go" assumption was wrong: two test callers also destructure the return and will fail to compile. The `security` one is silent until the final `go test ./...`.
  - `docs/reference.md` — user-facing config reference.
- **Patterns found**:
  - `Load` returns `(Excludes, Cache, error)` — multi-value-return convention documented in CLAUDE.md ("Cache is the second return value, before the error").
  - Validation helpers reject empty/invalid input and return an error wrapped with `projectconfig: `.
  - `ShellCommand`'s flag switch **already lists `-e`** as a value-taking flag (`spec.go:225`) — currently anticipatory/dead code that activates once `Args` emits `-e`.
- **Dependencies**: `gopkg.in/yaml.v3` (already imported); `github.com/moby/moby/api/types/container` (already imported; `container.Config.Env` is `[]string` of `"VAR=value"`).
- **⚠️ Critical discovery**: the drift-guard test `TestDriftGuard_ArgsAndSDKProjectionsAgree` (`spec_test.go:1016–1024`) currently **asserts that NO `-e` flags appear** and `cfg.Env` is empty ("no env injection expected"). This assertion MUST be rewritten to a positive Args-vs-`ContainerConfig.Env` agreement check, or the test will fail once env injection lands.

## Development Approach
- **Testing approach**: Regular (code first, then tests) — small, focused, well-understood change.
- Complete each task fully (including tests) before moving to the next.
- **Every task includes new/updated tests** — success and error scenarios.
- **All tests must pass before starting the next task.**
- Maintain backward compatibility: absent `environments:` block ⇒ `nil` ⇒ byte-identical output to today.
- Keep the pure/impure split: `spec.go` stays pure.
- POSIX-only; no Windows paths.

## Testing Strategy
- **Unit tests**: required for every task (table-driven, matching existing style).
- **e2e tests**: project has none (CLI tool); N/A.
- Test command: `go test ./...`

## Progress Tracking
- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix.
- Document blockers with ⚠️ prefix.
- Keep this plan in sync with actual work.

## Solution Overview
- **Schema**: new optional `environments:` YAML map (`map[string]yaml.Node`), decoded as `yaml.Node` to enable lenient scalar coercion.
- **Validation** (`validateEnvironments`): reject empty keys (correctness — `-e =value` is broken); require scalar values (`node.Kind == yaml.ScalarNode`), take `node.Value` as the string form (`8080`, `true`); reject non-scalar values fail-loud. **Null scalars** (`KEY:` / `KEY: null`, i.e. `node.Tag == "!!null"`) are rejected fail-loud — a valueless key is almost always a mistake (and we deliberately rejected host-passthrough). **Explicit empty string** (`KEY: ""`) is *accepted* → `-e KEY=` (legal docker; intentional empty value). No reserved-key blocking.
- **Output**: sorted `[]string` of `"KEY=VALUE"` (sorted for determinism; exactly the shape `docker run -e` and `container.Config.Env` consume).
- **Signature**: `Load(root) (Excludes, Cache, []string, error)` — add a value, not a struct refactor.
- **Rendering**: `Args` emits `-e KEY=VALUE` after `--security-opt`, before mounts; `ShellCommand` already handles `-e`; `ContainerConfig` sets `Env: s.Env`; `HostConfig` untouched.

## Technical Details
- `yamlSchema.Environments map[string]yaml.Node \`yaml:"environments"\``.
- `validateEnvironments(env map[string]yaml.Node) ([]string, error)`:
  - iterate map; for each `k, v`:
    - if `k == ""` → error `projectconfig: empty key in environments`.
    - if `v.Kind != yaml.ScalarNode` → error `projectconfig: environment %q must be a scalar value` (rejects lists/maps).
    - if `v.Tag == "!!null"` → error `projectconfig: environment %q has no value` (rejects bare `KEY:` / `KEY: null`).
    - append `k + "=" + v.Value` (explicit `KEY: ""` yields `"KEY="`, accepted).
  - return `dedupSorted(result)`.
  - (yaml.v3 already rejects duplicate map keys at decode time, so no dup-key handling needed here.)
- `Load`: ~10 return statements; every early return (ErrNotExist, read error, EOF/empty, decode error, each validation error) gets a `nil` in the new env slot.
- `docker.Options.Env []string`; `Spec.Env []string`; `BuildSpec` copies verbatim.
- `Args`: `for _, e := range s.Env { args = append(args, "-e", e) }` placed after the `--security-opt` loop, before the mounts loop.
- `ContainerConfig`: add `Env: s.Env`.

## What Goes Where
- **Implementation Steps** (`[ ]`): all code, tests, and doc updates — achievable in this repo.
- **Post-Completion** (no checkboxes): none anticipated (no consuming projects; no deployment config).

## Implementation Steps

### Task 1: Parse and validate the `environments` block

**Files:**
- Modify: `internal/projectconfig/projectconfig.go`
- Modify: `internal/projectconfig/projectconfig_test.go`

- [x] Add `Environments map[string]yaml.Node \`yaml:"environments"\`` to `yamlSchema` (required — `KnownFields(true)` would otherwise reject `environments:` as unknown).
- [x] Add `validateEnvironments(map[string]yaml.Node) ([]string, error)`: reject empty key; require `yaml.ScalarNode` value (else fail-loud error); build `"KEY=VALUE"` from `node.Value`; return `dedupSorted(...)`.
- [x] Update the package doc comment at the top of the file to document the `environments:` block (static vars, scalar coercion, fail-loud on non-scalar).
- [x] Write test: valid map → sorted `[]string` of `KEY=VALUE`.
- [x] Write test: scalar coercion (`PORT: 8080` → `"PORT=8080"`, `DEBUG: true` → `"DEBUG=true"`).
- [x] Write test (error): empty key rejected.
- [x] Write test (error): non-scalar value (`FOO: [a, b]` and a nested map) rejected.
- [x] Write test (error): null scalar (`KEY:` / `KEY: null`) rejected.
- [x] Write test: explicit empty string (`KEY: ""`) → `"KEY="` accepted.
- [x] Run tests — must pass before next task.

### Task 2: Thread env vars through `Load` and both call sites

**Files:**
- Modify: `internal/projectconfig/projectconfig.go`
- Modify: `cmd/makeslop/main.go`
- Modify: `cmd/makeslop/status.go`
- Modify: `cmd/makeslop/main_test.go`
- Modify: `internal/security/security_test.go`
- Modify: `internal/projectconfig/projectconfig_test.go`

- [x] Change `Load` signature to `(Excludes, Cache, []string, error)`; call `validateEnvironments` in the main path.
- [x] Update the `Load` doc comment (projectconfig.go:173-181) to document the new env return value (4-value return; absent block → `nil`).
- [x] Add the `nil` env slot to **every** early-return path in `Load` (ErrNotExist/missing file, read error, EOF/empty file, decode error, and each validation-error return) — there are ~10 returns.
- [x] Update `cmd/makeslop/main.go` (~line 123): `yamlExcludes, cacheCfg, envVars, err := projectconfig.Load(...)`.
- [x] Update `cmd/makeslop/status.go` (~line 280): `yamlExcludes, _, _, pcErr := projectconfig.Load(...)`.
- [x] Update `cmd/makeslop/main_test.go:3713` and `:3739`: `_, cache, _, err := projectconfig.Load(...)`.
- [x] Update `internal/security/security_test.go:58`: `excl, _, _, err := projectconfig.Load(dir)`.
- [x] Write test: absent `environments:` block → `nil` env (existing config file without the block).
- [x] Write test: missing file and empty/whitespace-only file → `nil` env.
- [x] Write test: confirm existing unknown-field/typo strictness still triggers (e.g. `enviroments:` typo errors).
- [x] Run tests (`go test ./...`) — `projectconfig`, `cmd/makeslop` must compile and pass before next task.

### Task 3: Render env vars in the docker Spec

**Files:**
- Modify: `internal/docker/spec.go`
- Modify: `cmd/makeslop/main.go`
- Modify: `internal/docker/spec_test.go`

- [x] Add `Env []string` to `Options` (doc: `"KEY=VALUE"` pairs; nil/empty no-op; caller sorts).
- [x] Add `Env []string` to `Spec`; `BuildSpec` copies `o.Env` verbatim (no reorder).
- [x] `Args()`: emit `-e KEY=VALUE` per entry, positioned after the `--security-opt` loop and before the mounts loop.
- [x] `ContainerConfig()`: set `Env: s.Env`. (Confirm `ShellCommand` needs no change — `-e` already in its switch.)
- [x] Wire `Env: envVars` into the `docker.Options{}` literal in `main.go` (~line 143).
- [x] **⚠️ Rewrite the env block in `TestDriftGuard_ArgsAndSDKProjectionsAgree` (`spec_test.go:1016–1024`)**: replace the "no `-e` expected / `cfg.Env` empty" assertions with a positive check that the ordered `-e` values collected from `Args` equal `ContainerConfig().Env`; set a non-empty `o.Env` in the test fixture.
- [x] Write test: `Args()` emits `-e K=V` in the correct position (after `--security-opt`, before mounts).
- [x] Write test: `ShellCommand()` renders the `-e` lines.
- [x] Write test: `ContainerConfig().Env` equals `Spec.Env`.
- [x] Write test: empty `Env` → no `-e` flag and output byte-identical to current (backward-compat).
- [x] Write test: determinism — same `environments` map → identical `Spec` (sorted ordering).
- [x] Run tests — must pass before next task.

### Task 4: Verify acceptance criteria
- [ ] Verify a `.makeslop.yaml` with an `environments:` block produces correct `-e` flags in `makeslop run --dry-run` output (manual or via test).
- [ ] Verify absent block → no behavior change.
- [ ] Run full test suite: `go test ./...`.
- [ ] Confirm `MigrationVersion` and `CurrentVersion` are NOT bumped (Settings struct & embedded Dockerfile unchanged; `environments` lives in per-project `.makeslop.yaml`).

### Task 5: Documentation
- [ ] Update `docs/reference.md` to document the `environments:` block (static values, scalar coercion, null-rejection, examples).
- [ ] Update `CLAUDE.md` (required): document the `Load` 4-value return and the `environments:` flow alongside the existing `cache:`/`exclude:` block sections.
- [ ] Move this plan to `docs/plans/completed/`.

## Post-Completion
*Informational only — no checkboxes.*

**Manual verification** (optional):
- Run `makeslop run` in a real workspace with an `environments:` block and confirm the vars are present inside the container (`env | grep ...`).

**External system updates**: none — no consuming projects; not a deployment/config change.
