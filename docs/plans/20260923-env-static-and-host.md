# Static and host-passthrough environment variables in `.makeslop.yaml`

## Overview
- Rework the `environments:` block in `.makeslop.yaml` so it supports two kinds of variables:
  - `static`: fixed `KEY: value` pairs (today's behaviour, moved one level down)
  - `host`: a list of variable names whose values are copied from the host environment at `run` time
- Solves: users can't forward host values (tokens such as `GITHUB_TOKEN`, or context such as `TERM`/`LANG`) into the container without hard-coding them in a file that is checked into the repo.
- Breaking change: the old flat `environments: {KEY: value}` form is rejected with a clear migration hint. Files are not auto-migrated, following the project convention.

## Context (from discovery)
- `internal/projectconfig/projectconfig.go`
  - `yamlSchema.Environments` is `map[string]yaml.Node`
  - `validateEnvironments` (~line 338) returns sorted `KEY=VALUE`
  - `Load` returns `(Excludes, Cache, []string, error)`
- `internal/cli/run.go:115`: `projectconfig.Load` is called there and `envVars` is passed to `docker.Options.Env`.
- `internal/cli/status.go:223`: calls `Load` and discards the env value (`_`).
- `internal/docker/spec.go`: `Options.Env` → `Spec.Env` → `-e KEY=VALUE` in `Args()` and `ContainerConfig().Env`, with a drift-guard test. **Not changed.**
- Existing tests:
  - `internal/projectconfig/projectconfig_test.go`: Load error table (~273–290) and `TestValidateEnvironments_*` (~1210–1330)
  - `internal/cli/run_test.go`: `TestRun_EnvironmentsBlock_ProducesEnvFlags`, `TestRun_NoEnvironmentsBlock_NoEnvFlags` (~1610–1660)
- Docs:
  - `docs/reference.md`, section "Environment variables" (~345)
  - `docs/security.md`
  - `CLAUDE.md`, "Project config" section (`Load` signature line)

## Development Approach
- **testing approach**: Regular (code first, then tests)
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
- **CRITICAL: all tests must pass before starting next task** - no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- run tests after each change: `go test -timeout=100s ./...`; lint: `golangci-lint run`
- backward compatibility is intentionally broken for the flat `environments:` form, but only with a targeted, actionable error

## Testing Strategy
- **unit tests**: table-driven, in `projectconfig_test.go` (parsing/validation) and `run_test.go` (host resolution + dry-run output)
- no UI/e2e tests in this project

## Progress Tracking
- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- keep plan in sync with actual work done

## Solution Overview
- New shape:
  ```yaml
  environments:
    static:            # optional map KEY: scalar
      NODE_ENV: production
      PORT: 8080
    host:              # optional list of names, copied from host, same name
      - GITHUB_TOKEN
      - TERM
  ```
- `projectconfig.Load` parses and validates the block but never reads the process environment. It returns a new `Env{Static, Host}` struct.
- `run.go` resolves `Host` names with `os.LookupEnv` and merges the result with `Static` into one sorted `[]string` of `KEY=VALUE`, which it passes unchanged as `docker.Options.Env`.
- `internal/docker` does not change. `--dry-run` prints resolved host values in full; the user chose this explicitly. The printed and executed forms stay identical, so the drift guard needs no change.
- Out of scope (YAGNI):
  - renaming (host `A` → container `B`)
  - fallback defaults
  - required-var markers
  - masking in dry-run

## Technical Details
- `yamlSchema.Environments` becomes `yaml.Node` and is walked manually in `validateEnvironments(node yaml.Node) (Env, error)`.
  - **Why the walker needs its own duplicate check:** when the decode target is a `yaml.Node`, yaml.v3 sets the node without calling `d.mapping()`, so its "mapping key already defined" check never runs. The walker keeps its own seen-map at both levels.
  - **Keys:** every key, at the top level and inside `static`, must be a `ScalarNode` whose tag is not `!!null`. This rejects `? [a]: x` and `~: x`.
  - **Null values:** a zero node or `!!null` (absent or empty block) gives `Env{}`, no error. So does an empty or null `static:`/`host:` sub-key.
  - Error messages include only names, never values. All of them start with the `projectconfig: ` prefix.
  - Errors, all asserted in tests:

    | condition | message |
    |---|---|
    | `environments` is not a mapping (e.g. `environments: [A]`) | `projectconfig: environments must be a mapping with optional "static" and "host" keys` |
    | unknown key with a **scalar** value (old flat form) | `projectconfig: environments: flat "KEY: value" form is no longer supported; move entries under environments.static` |
    | unknown key with a non-scalar value (e.g. typo `hosts: [A]`) | `projectconfig: unknown key %q in environments (allowed: static, host)` |
    | duplicate `static`/`host` key | `projectconfig: duplicate key %q in environments` |
    | `static` not a mapping (scalar or sequence) | `projectconfig: environments.static must be a mapping of KEY: value` |
    | `host` not a sequence (e.g. `host: GITHUB_TOKEN`) | `projectconfig: environments.host must be a list of variable names` |
    | duplicate key inside `static` | `projectconfig: duplicate key %q in environments.static` |
    | name in both `static` and `host` | `projectconfig: environment key %q listed in both environments.static and environments.host` |

  - **`static` pair rules** are today's rules, moved into the walker:
    - reject empty keys, and keys containing `=` or `\n\r\t`
    - values must be scalars; reject `!!null` and values containing `\n\r\t`
    - `""` is allowed
    - numbers and booleans are coerced via `node.Value`
  - **`host` entries** must be `ScalarNode`s:
    - reject `!!null` entries (`- ~`, and a bare `-`)
    - reject empty names, and names containing `=` or any whitespace (`unicode.IsSpace`)
    - the whitespace rule is stricter than for static keys on purpose: a variable name, unlike a static key, has to exist on the host
    - dedupe silently and sort
  - YAML merge keys (`<<:`) inside `environments:` are no longer expanded. They end up rejected as unknown or non-scalar keys, which is acceptable; mention it in the migration note.
- New type:
  ```go
  // Env is the parsed environments: block.
  type Env struct {
      Static []string // sorted "KEY=VALUE"
      Host   []string // sorted, deduped variable names to copy from the host
  }
  ```
- `Load` signature: `func Load(root string) (Excludes, Cache, Env, error)`
- `run.go` resolution (new small helper, e.g. `resolveEnv(env projectconfig.Env, lookup func(string) (string, bool)) []string`, called with `os.LookupEnv`):
  - unset → skipped silently
  - set but empty → `NAME=`
  - result = `Static` + resolved host pairs, sorted
  - empty result → `nil`, so no `-e` flags appear
  - host values are passed verbatim, including newlines (e.g. PEM keys); `shellQuote` and the SDK handle them. The static `\n\r\t` rule does not apply to them; state this in the docs.

## What Goes Where
- **Implementation Steps**: code, tests, docs in this repo
- **Post-Completion**: manual smoke test against a real daemon

## Implementation Steps

### Task 1: Parse and validate `environments.static` / `environments.host`

**Files:**
- Modify: `internal/projectconfig/projectconfig.go`
- Modify: `internal/projectconfig/projectconfig_test.go`

- [x] add `Env` struct; change `yamlSchema.Environments` to `yaml.Node` and update its comment (~projectconfig.go:105)
- [x] rewrite `validateEnvironments(node yaml.Node) (Env, error)` per the Technical Details:
  - error table
  - key-node checks
  - duplicate detection at both levels
  - static rules
  - host entry rules, dedupe and sort
  - overlap check
- [x] update its doc comment (~329-337)
- [x] change `Load` to return `Env`; update its doc comment (fourth return value)
- [x] fix the `Load` call sites in `internal/cli/run.go` and `internal/cli/status.go` so the tree compiles:
  - `run.go` uses `env.Static` only for now
  - `status.go` keeps discarding the value
  - these already use `_` and need no change: `security_test.go:50`, `init_test.go:509`, `:534`
- [x] add a test helper `envNode(t, yamlSnippet) yaml.Node`: unmarshal the snippet and return `doc.Content[0]`, not the DocumentNode
- [x] migrate existing tests to the new signature and the `static:` form:
  - `TestValidateEnvironments_*` (~1210-1400; currently built on `map[string]yaml.Node`)
  - the Load error-table env rows (~273-290)
- [x] update the Load-level env tests. Replace `envVars != nil` with `reflect.DeepEqual(env, Env{})`, and convert flat-form fixtures to `static:`:
  - `TestLoad_AbsentEnvironments_NilEnv` (~1407)
  - `TestLoad_MissingFile_NilEnv` (~1435)
  - `TestLoad_EmptyAndWhitespaceFile_NilEnv` (~1448)
  - `TestLoad_EnvironmentsBlock_ReturnsSortedPairs` (~1476)
  - `TestLoad_MissingFile_NoSymlink_ReturnsDefaults` (~1666)
- [x] add success tests: static only, host only, both, empty `environments:`, empty/null sub-keys, block absent, host dedupe/sort, scalar coercion under static
- [x] add error tests, asserting every message in the error table:
  - `environments` as a list
  - old flat form
  - unknown key with a non-scalar value
  - scalar under `static`
  - `host: NAME` (scalar)
  - `host` given as a mapping
  - duplicate `static:` block, duplicate key inside `static`
  - null key (`~: x`), complex key
  - host entries: `- ~`, bare `-`, empty, containing `=`, containing whitespace, non-scalar
  - static/host overlap
- [x] add a `status_test` row: a flat-form file shows the non-blocking `cannot read .makeslop.yaml` warning with the hint (modelled on the stale-`network:` test, ~status_test.go:403)
- [x] ➕ converted the `TestRun_EnvironmentsBlock_ProducesEnvFlags` fixture to `static:` here so the suite stays green (listed again under task 2)
- [x] run `go test -timeout=100s ./...` - must pass before task 2
  - ⚠️ `TestScan_WalkError_Propagated` and `TestStatus_Check5_ScanErrShowsWarn` fail in this dev environment only: the filesystem ignores `chmod 000`, so the unreadable dir stays readable. Unrelated to this change (`internal/security` is untouched); all other tests pass

### Task 2: Resolve host variables in `run`

**Files:**
- Modify: `internal/cli/run.go`
- Modify: `internal/cli/run_test.go`

- [x] add `resolveEnv(env projectconfig.Env, lookup func(string) (string, bool)) []string` in `run.go`; it merges static and resolved host pairs, sorted, and returns `nil` when empty
- [x] wire it in: `opts.Env = resolveEnv(env, os.LookupEnv)`
- [x] update `TestRun_EnvironmentsBlock_ProducesEnvFlags` to the `static:` form (done in task 1)
- [x] add unit tests for `resolveEnv` with a fake lookup:
  - set, set-empty (`NAME=`), unset (skipped)
  - sorted merge across static and host
  - empty → nil
  - host value containing a newline is passed verbatim
- [x] add a dry-run integration test using `t.Setenv` (set + empty). For the unset case use a unique name such as `MAKESLOP_TEST_UNSET_<rand>`, or call `t.Setenv(name, "")` then `os.Unsetenv(name)`: `t.Setenv` cannot unset a variable by itself:
  - output contains `-e SET_VAR=val` and `-e EMPTY_VAR=`
  - output lacks the unset name
  - order is sorted
- [x] add a test that the old flat form makes `run` fail with the hint and never calls the runner. `checkDaemonPreflight` runs before `Load`, so use `--dry-run` or a `fakeDocker` whose daemon check passes, and assert the fake runner was not called.
- [x] run `go test -timeout=100s ./...` - must pass before task 3 (only the two known chmod-000 environment failures remain)

### Task 3: Verify acceptance criteria
- [x] verify:
  - both kinds work (`TestValidateEnvironments_Success`, `TestRun_EnvironmentsHost_DryRunResolvesValues`)
  - flat form fails loudly with the hint (`TestRun_EnvironmentsFlatForm_FailsWithHint`, `TestStatus_Check5_FlatEnvironmentsShowsWarnWithHint`)
  - unset host vars are skipped (`TestResolveEnv`, dry-run test)
  - dry-run shows resolved values (`TestRun_EnvironmentsHost_DryRunResolvesValues`)
  - `internal/docker` is untouched (`git diff main...HEAD --stat -- internal/docker` is empty)
- [x] run full test suite: `go test -timeout=100s ./...` (only the two known chmod-000 environment failures)
- [x] run linter: `golangci-lint run` (0 issues)

### Task 4: [Final] Update documentation
- [x] `docs/reference.md`: rewrite the "Environment variables" section. Cover:
  - `static`/`host` shape and the static value rules
  - host semantics: same name, unset → skipped, empty → `NAME=`
  - overlap error
  - migration note: move flat entries under `static:`, with the exact error text
  - warning that `--dry-run` prints resolved host values, secrets included
  - host values are passed verbatim and not validated
  - YAML merge keys are not supported inside `environments:`
  - also update the short mention near line 80
- [x] `README.md` (~111-118): replace the flat `environments:` example and the "Inject static environment variables" wording with the static/host form
- [x] `docs/security.md`: add a short section:
  - `host` deliberately copies host values into the agent's container, so only list what the agent should have
  - `--dry-run` output can contain secrets
  - add a TOC entry
- [x] `CLAUDE.md`: update the `Load` return signature line to `(Excludes, Cache, Env, error)` and note that host resolution lives in `run.go`
- [x] move this plan to `docs/plans/completed/` (deferred: done after review phases)

## Post-Completion
*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification:**
- `GITHUB_TOKEN=x makeslop run --dry-run` in a project with `host: [GITHUB_TOKEN, UNSET_VAR]`: check the output shows `-e GITHUB_TOKEN=x` and no `UNSET_VAR`
- run a real container and check `env` inside it shows the host and static values
- confirm that an old project with a flat `environments:` block fails with the migration hint

**External updates:**
- users with existing flat `environments:` blocks must move their entries under `static:` (mention this in release notes)
