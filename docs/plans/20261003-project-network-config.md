# Project network config (`network_mode` / `networks`)

## Overview
- Add two top-level keys to `.makeslop.yaml` that control how the app container is networked, using compose's names and meaning:
  ```yaml
  network_mode: "container:proxy"   # bridge | host | none | container:<name|id> | <network-name>
  # or, instead:
  networks: [myapp_default, egress_internal]
  ```
- Main use case: `docker run --network container:proxy`. The agent shares the network namespace of an egress sidecar (VPN, transparent proxy, mitmproxy), so all of its traffic goes through that container.
- Attach only: makeslop never creates or removes networks, the same stance it takes on images. A missing network or container fails at preflight with a hint.
- When neither key is set, behavior is byte-identical to today (default bridge, no `--network` flag).

## Context (from discovery)
- `internal/projectconfig/projectconfig.go`: strict `yamlSchema`; `Load` currently returns `(Excludes, Cache, Env, error)`. Comments mention a "stale network: block" (the old proxy feature), which stays an unknown key.
- `internal/docker/spec.go`: pure `BuildSpec` → `Args()`/`ShellCommand()` (dry-run) and `ContainerConfig()`/`HostConfig()` (SDK). The drift-guard test keeps them in sync.
- `internal/docker/run.go:150`: `ContainerCreate` with `Config` and `HostConfig` only.
- `internal/docker/client.go`: the `apiClient` narrow interface, plus fakes in `internal/docker/fakes_test.go`.
- `internal/docker/preflight.go`: the `ImageExists` contract (only `cerrdefs.IsNotFound` → absent).
- `internal/cli/deps.go`: consumer interfaces, `*Preflight` helpers bounded by `preflightTimeout`, and `dockerNewErrStub`.
- `internal/cli/run.go:141` and `internal/cli/status.go:223`: the `projectconfig.Load` call sites.
- Docs: `docs/reference.md:243` ("network: block removed") and `docs/security.md:375` ("Network egress").

## Development Approach
- **testing approach**: Regular (code first, then tests in the same task)
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
- **CRITICAL: all tests must pass before starting next task**
- **CRITICAL: update this plan file when scope changes during implementation**
- run tests after each change: `GOTMPDIR=$HOME/.cache/gotmp go test -timeout=100s ./...` (/tmp is noexec here)
- lint: `golangci-lint run` if available, otherwise `go vet ./...` and `staticcheck ./...`
- backward compatibility: existing configs without the new keys behave exactly as before

## Testing Strategy
- **unit tests**: table tests in `projectconfig`, spec, and drift-guard tests in `docker`, preflight tests with SDK fakes, CLI tests through `newRootCmdWithDeps` + `fakeDocker`.
- no e2e/UI tests in this project.

## Progress Tracking
- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix

## Solution Overview
- **Config**: two strict-decoded fields, validated into `projectconfig.Network{Mode, Networks}`. Setting both is an error. `Load` is refactored to return one `Config` struct, so adding this field (and future ones) doesn't keep growing the return tuple.
- **Spec**: `NetworkMode` and `Networks` flow verbatim through `Options` → `Spec`. They are rendered as `--network` flags for dry-run and as `HostConfig.NetworkMode` + a new `NetworkingConfig()` for the SDK. "Printed == executed" is kept and extended in the drift-guard test.
- **Preflight**: `run` (not `--dry-run`) checks that the `container:` target exists and is running, and that each named network exists, with actionable hints. `status` gets a blocking `network` row.
- **Security**: a repo's `.makeslop.yaml` can choose `host` or join any container's namespace. This is documented, with no runtime warning (consistent with the user's earlier env-config decision).

## Technical Details
- `projectconfig`:
  ```go
  type Network struct {
      Mode     string   // "" = Docker default; bridge|host|none|container:<x>|<net>
      Networks []string // file order kept; first is primary
  }
  type Config struct {
      Excludes Excludes
      Cache    Cache
      Env      Env
      Network  Network
  }
  func Load(root string) (Config, error)
  ```
  The missing-file and empty-file paths return `Config{Cache: Cache{true,true}}`.
- Name regex: `^[a-zA-Z0-9][a-zA-Z0-9_.-]*$` (Docker's network/container name charset).
- Validation (messages quote the offending name; network and container names are not secret, unlike env values):
  - normalise first: `network_mode: ""` → unset; an empty or null `networks` → unset. Only then check exclusivity.
  - `container:` prefix → remainder must be non-empty and match the name regex (container IDs match too).
  - other `network_mode` values must match the name regex; there's no hard-coded mode list, because the daemon decides.
  - `networks`: entries must be non-empty and match the name regex. Duplicates are an error. `host`, `none`, `default`, and any `container:` value are rejected ("is a network_mode, not a network").
  - compose's mapping form (`networks: {a: {}}`) → a targeted error: `networks must be a list of names; per-network options are not supported`. To get this instead of a raw yaml type error, decode `networks` as a `yaml.Node` or check the node kind.
  - both keys set (after normalising) → `set either network_mode or networks, not both`.
- `docker`:
  - `Options.NetworkMode`, `Options.Networks` → `Spec.NetworkMode`, `Spec.Networks`.
  - `Args()`: after the `--security-opt` loop and before `-e`: `--network <mode>`, or one `--network <n>` per entry.
  - `HostConfig().NetworkMode = container.NetworkMode(mode or Networks[0])`.
  - `NetworkingConfig()` → `&network.NetworkingConfig{EndpointsConfig: map[name]*network.EndpointSettings{...}}` when `len(Networks) > 0`, else nil.
  - `Run` passes `NetworkingConfig: s.NetworkingConfig()` in `ContainerCreateOptions`.
  - New `apiClient` methods (moby `client@v0.4.1`):
    - `ContainerInspect(ctx context.Context, container string, options moby.ContainerInspectOptions) (moby.ContainerInspectResult, error)`
    - `NetworkInspect(ctx context.Context, network string, options moby.NetworkInspectOptions) (moby.NetworkInspectResult, error)`
  - New `Docker` methods:
    - `ContainerRunning(ctx, name) (exists, running bool, err error)` via `ContainerInspect`. `running = res.Container.State != nil && res.Container.State.Running && !res.Container.State.Paused`: `State` is a pointer, and a paused proxy would stall traffic.
    - `NetworkExists(ctx, name) (bool, error)` via `NetworkInspect`
    - both follow the `ImageExists` not-found contract.
- What preflight inspects:
  - `container:<x>` → `ContainerRunning(x)`
  - `bridge`, `host`, `none`, `default` → nothing (built in; `default` is a bridge alias with no network object)
  - any other `network_mode`, and every `networks` entry → `NetworkExists`
  - unset → nothing
- Preflight hints (one shared helper, used by both `run` and `status`). These account for compose naming:
  - container missing: `network_mode: container "proxy" not found — start it first; compose names containers <project>-<service>-1 unless container_name is set (check 'docker ps')`
  - container stopped or paused: `network_mode: container "proxy" is not running — start it first`
  - network missing: `network "X" not found — create it with 'docker network create X'; compose prefixes networks with <project>_ (check 'docker network ls')`
- Multiple networks at create time need daemon API ≥ 1.44 (Docker Engine 25+). This is documented only, with no `NetworkConnect` fallback.

## What Goes Where
- **Implementation Steps**: code, tests, and docs in this repo.
- **Post-Completion**: manual check against a real `proxy` container.

## Implementation Steps

### Task 1: Refactor `projectconfig.Load` to return `Config`

**Files:**
- Modify: `internal/projectconfig/projectconfig.go`
- Modify: `internal/projectconfig/projectconfig_test.go`
- Modify: `internal/cli/run.go`
- Modify: `internal/cli/status.go`
- Modify: `internal/cli/init_test.go`, `internal/security/security_test.go` (➕ extra `Load` call sites in tests)

- [x] add `Config{Excludes, Cache, Env}` (Network comes in Task 2) and change `Load` to `(Config, error)`; update the doc comment
- [x] update the call sites in `run.go` and `status.go`
- [x] update existing `projectconfig` tests to the new signature (no behavior change)
- [x] run tests - must pass before next task

### Task 2: Parse and validate `network_mode` / `networks`

**Files:**
- Modify: `internal/projectconfig/projectconfig.go`
- Modify: `internal/projectconfig/projectconfig_test.go`

- [x] add the `NetworkMode`/`Networks` fields to `yamlSchema`, the `Network` type, and `Config.Network`
- [x] implement `validateNetwork(mode string, nets []string) (Network, error)` following the rules in Technical Details
- [x] add a commented `# network_mode: "container:proxy"` example to `renderStub`; reword the "stale network: block" comments (the old `network:` key is still unknown)
- [x] write tests for valid cases: unset, empty string, `bridge`/`host`/`none`/`default`, `container:proxy`, `container:<id>`, a custom network, a single and multiple ordered `networks`, `network_mode: ""` + `networks: [a]`, `network_mode: x` + `networks: []`
- [x] write tests for errors: both keys set, `container:` with an empty name, invalid chars or whitespace, empty entry, duplicate entry, `host`/`none`/`default`/`container:x` in `networks`, mapping-form `networks` (targeted message), old `network:` still rejected; check that the stub parses to zero `Network`
- [x] run tests - must pass before next task

### Task 3: Render network settings in `Spec`

**Files:**
- Modify: `internal/docker/spec.go`
- Modify: `internal/docker/spec_test.go`
- Modify: `internal/docker/run.go`
- Modify: `internal/docker/run_test.go`

- [x] add `NetworkMode`/`Networks` to `Options` and `Spec`; copy them in `BuildSpec`
- [x] emit `--network` in `Args()`; add `--network` to the `ShellCommand` paired-flag cases
- [x] set `HostConfig().NetworkMode`; add `Spec.NetworkingConfig()`
- [x] pass `NetworkingConfig` in `Run`'s `ContainerCreate` call
- [x] write spec table tests: unset (no flag, nil NetworkingConfig, empty NetworkMode), `container:proxy`, `host`, single network, multiple ordered networks; `ShellCommand` line output
- [x] rename `TestHostConfig_NetworkModeIsAlwaysBridge` (spec_test.go:717) to `...DefaultsToEmpty`; leave the default assertions in `TestSpecArgs_DefaultArgvHasNoNetworkOrEnv` and `TestDriftGuard_ArgsAndSDKProjectionsAgree` as they are
- [x] add a new `TestDriftGuard_Network`: `--network` values in `Args()` match `HostConfig.NetworkMode` and the `NetworkingConfig` keys
- [x] in `run_test`, assert that `fakeRunClient.LastContainerCreateOpts.NetworkingConfig` is set (the recording already exists)
- [x] run tests - must pass before next task

### Task 4: Docker-level container and network checks

**Files:**
- Modify: `internal/docker/client.go`
- Modify: `internal/docker/fakes_test.go`
- Modify: `internal/docker/preflight.go`
- Modify: `internal/docker/preflight_test.go`

- [ ] add `ContainerInspect` and `NetworkInspect` to `apiClient` (the `var _ apiClient` assertion must still compile) and to the fakes, including `noopClient` so `run_test.go`'s embedded `fakeClient` keeps compiling
- [ ] implement `ContainerRunning` and `NetworkExists` with the `IsNotFound`-only contract and the nil-`State` guard
- [ ] write tests: found/running, found/stopped, found/paused, found/nil State, not found, other error propagates (for both methods where they apply)
- [ ] run tests - must pass before next task

### Task 5: Network preflight in `run`

**Files:**
- Modify: `internal/cli/deps.go`
- Modify: `internal/cli/root.go`
- Modify: `internal/cli/run.go`
- Modify: `internal/cli/main_test.go`
- Modify: `internal/cli/run_test.go`

- [ ] add a `networkChecker` interface (`ContainerRunning`, `NetworkExists`) and a `network` field to `dockerDeps`, plus `dockerNewErrStub` methods
- [ ] stop the nil-field bug: `dockerDeps` is built as keyed literals in `root.go:48` (stub), `root.go:55` (production), and `main_test.go:118` (`depsFrom`), so a forgotten field compiles and nil-panics only in prod. Add `newDockerDeps(x allDocker) dockerDeps`, where `allDocker` embeds all four interfaces, and use it at all three sites
- [ ] add `networkPreflight(ctx, projectconfig.Network) error` to `deps.go`: bounded by `preflightTimeout`, returns the hint errors from Technical Details; a no-op for the zero `Network`
- [ ] in `runRun`, pass `cfg.Network` into `docker.Options`; call the preflight after the image check (skipped on `--dry-run`), printing `makeslop: <hint>` and returning `errSilent`
- [ ] extend `fakeDocker` with configurable container/network state
- [ ] write tests: dry-run prints `--network container:proxy` with no daemon calls; container missing/stopped/running; network missing/present; inspect error; unset config and `bridge`/`host`/`none`/`default` make no inspect calls; a custom `network_mode` is inspected as a network
- [ ] run tests - must pass before next task

### Task 6: `network` row in `status`

**Files:**
- Modify: `internal/cli/status.go`
- Modify: `internal/cli/status_test.go`

- [ ] after the workspace check, reuse the single `projectconfig.Load` result for both the secret-scan row and the new `network` row
- [ ] row states: workspace unresolved → `–` (same as secret scan); unset or built-in mode → `–`/`✓` without inspection; daemon unreachable with an inspected target → `✗ cannot check — daemon unreachable` (mirrors the image row); OK → `✓ container:proxy` / `✓ networks: a, b`; missing/stopped → `✗` with the shared hint; `.makeslop.yaml` invalid → `✗ cannot check — .makeslop.yaml invalid` (`run` fails hard on the same file, so `status` must not report ready)
- [ ] update the `Short` text (status.go:264) to mention network
- [ ] make sure the row appears in `--json` output
- [ ] write tests for each row state, in both text and `--json`, and check that a `✗` network makes `status` exit non-zero
- [ ] run tests - must pass before next task

### Task 7: Verify acceptance criteria
- [ ] `network_mode: "container:proxy"` → dry-run shows `--network container:proxy`; the real run passes `HostConfig.NetworkMode=container:proxy`
- [ ] no keys → dry-run output identical to before (existing golden/spec tests unchanged)
- [ ] run the full suite: `GOTMPDIR=$HOME/.cache/gotmp go test -timeout=100s ./...`
- [ ] run the linter (`golangci-lint run` or `go vet ./... && staticcheck ./...`)

### Task 8: [Final] Update documentation
- [ ] `docs/reference.md`: document `network_mode`/`networks` (values, mutual exclusion, attach-only, preflight hints, Engine 25+ for multiple networks); keep the "network: block removed" section with a pointer to the new keys; add the `status` network row
- [ ] `docs/security.md`: rewrite "Network egress": default bridge, the modes, the repo-trust note (`host` / joining another container's namespace, review cloned configs), the `container:proxy` egress-sidecar pattern, and the `internal: true` network + `HTTPS_PROXY` via `environments.static` variant
- [ ] `docs/architecture.md`: add the network fields to the spec/preflight flow description
- [ ] `CLAUDE.md`: project-config section — the new keys, `Load → (Config, error)`, the network preflight in run/status, and the new `apiClient` methods
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion
*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification:**
- `docker run -d --name proxy <some proxy image>`, set `network_mode: "container:proxy"`, run `makeslop run`, and confirm the egress IP/route inside the agent matches the proxy's
- stop `proxy`; `makeslop run` and `makeslop status` should both show the "not running" hint
- `networks: [a, b]` on Engine 25+ attaches both; check with `docker inspect`
