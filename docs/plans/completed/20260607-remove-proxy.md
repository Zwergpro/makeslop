# Remove socat sidecar / egress proxy feature

## Overview
Remove the optional egress-proxy feature (socat sidecar + `--proxy` flag + `network.proxy.address`
config) entirely, reverting makeslop to **always-on plain Docker bridge networking** for the app
container. This is the third proxy iteration (gateway → socket-by-default → socat-to-remote); the
feature never reliably routed traffic because `unix://` proxy URLs are non-standard and most HTTP
clients silently ignore them (documented in `proxy_integration_test.go` itself). The end state is
exactly the existing "Direct (default)" path with every proxy-only branch deleted.

**Problem solved:** removes ~1,000 lines of code plus dead SDK plumbing that supported a
non-functional feature, simplifying `spec.go`, `main.go`, `client.go`, `testing.go`, `status.go`,
and `projectconfig.go`.

**Integration:** the app container already runs bridge-by-default; removing the proxy collapses the
two-state network model to a single state with no behavioral change for the common path.

## Context (from discovery)
Files/components involved:
- Delete: `internal/docker/sidecar.go`, `internal/docker/sidecar_test.go`, `internal/docker/proxy_integration_test.go`
- Modify: `internal/docker/spec.go`, `internal/docker/client.go`, `internal/docker/testing.go`
- Modify: `internal/docker/spec_test.go`, `internal/docker/client_test.go`, `internal/docker/run_test.go`
- Modify: `cmd/makeslop/main.go`, `cmd/makeslop/main_test.go`, `cmd/makeslop/status.go`, `cmd/makeslop/status_test.go`
- Modify: `internal/projectconfig/projectconfig.go`, `internal/projectconfig/projectconfig_test.go`
- Modify: `internal/security/security_test.go` (Load signature ripple)
- Docs: `docs/security.md`, `docs/architecture.md`, `docs/reference.md`, `README.md`, `CLAUDE.md`
- Preserve (historical): `docs/plans/completed/20260602-vm-socat-proxy.md`, `docs/plans/completed/20260602-network-default-no-proxy.md`

Related patterns found:
- `apiClient` narrow interface seam in `client.go` with `var _ apiClient = (*moby.Client)(nil)` assertion.
- `newClientFn` / `newSidecarFn` package-level test seams.
- Pure/impure split: `spec.go` (pure argv/SDK projection) vs `run.go`/`build.go` (side effects).
- Drift-guard test keeps `Args()`/`ShellCommand()` honest against `ContainerConfig()`/`HostConfig()`.

Dependencies identified:
- `github.com/moby/buildkit` **stays** — used by `Build`, not the proxy.
- 7 `apiClient` methods (`ContainerInspect`, `ExecCreate`, `ExecStart`, `ExecInspect`,
  `VolumeCreate`, `VolumeRemove`, `ImagePull`) have exactly one caller each, all in `sidecar.go`
  (verified via grep). None used by `Run`/`Build` — all prune cleanly; the compile-time assertion
  stays valid since the interface only shrinks.

## Development Approach
- **Testing approach**: Regular (deletion-driven) — remove proxy code and its tests together, then
  update signature-ripple call sites and their tests in the same task.
- Complete each task fully before moving to the next; run `go build ./...` + `go test ./...` after each.
- No new functionality is added, so most "tests" work is **deleting** proxy tests and **updating**
  tests whose call sites change (`projectconfig.Load` signature, status check count).
- **CRITICAL: every task ends with the package building and its tests passing before the next task.**
- Backward-compat is intentionally broken for stale `.makeslop.yaml` files with a `network:` block
  (loud strict-parse error) — this is a locked decision, not a regression.

## Testing Strategy
- **Unit tests**: each task either deletes obsolete proxy tests or updates call-site tests; the
  package must compile and pass after each task.
- **e2e tests**: none in this project; the only integration test (`proxy_integration_test.go`,
  build-tagged, daemon-gated) is being deleted, so the final gate needs no Docker daemon.
- **Final gate**: `go build ./...`, `go vet ./...`, `go test ./...` all green, plus a grep sweep
  proving no proxy identifiers remain outside the two preserved completed-plan files.

## Progress Tracking
- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix.
- Document blockers with ⚠️ prefix.
- Keep this plan in sync with actual work.

## Solution Overview
Strip the feature in dependency order so the tree never sits broken longer than one task:
delete the self-contained sidecar files first, then remove the `spec.go` proxy branch, then the
`main.go` wiring + flag, then change `projectconfig.Load`'s signature and fix every caller, then
prune the `apiClient` interface + `FakeRunClient`, then collapse `status` to five checks, then sweep
docs. The `Load` signature change `(Excludes, Network, Cache, error) → (Excludes, Cache, error)` is
the only cross-package ripple; it is handled in one task that also fixes `runRun`, `status`, and the
projectconfig/status tests together.

## Technical Details
- `spec.go`: remove `Options.ProxySocketVolume`, the `proxySocketDir` const, and the `BuildSpec`
  proxy block (`NetworkMode="none"`, `HTTP_PROXY`/`HTTPS_PROXY` env, read-only `/sockets` volume
  mount). Keep generic `Mount`/`mountsFor` volume support (orthogonal plumbing).
- `client.go`: remove the 7 sidecar-only interface methods; keep `ImageInspect`, `Ping`,
  `ContainerCreate/Attach/Start/Wait/Resize/Remove`, `ImageBuild`, `DialHijack`, `Close`.
- `testing.go`: remove `FakeRunClient` sidecar-only fields and methods; revert the socat branch in
  the fake's `ImageInspect` to a plain lookup. Keep `FakeBuildClient`, `ImageMissing`, `PingErr`.
- `main.go`: remove `validHostRe`, the `sidecarRunner` interface, the `newSidecarFn` seam, upstream
  resolution, volume-name derivation, `sc.Start`/`defer sc.Close()`, and the `--proxy` flag.
- `projectconfig.go`: delete `type Network` + the `network.proxy.address` yamlSchema nesting; change
  `Load` signature; stop emitting `network:` in `renderStub`/`Scaffold`.
- `status.go`: drop check 6 (proxy) and check 7 (socat image); five checks remain.
- No `CurrentVersion`/`MigrationVersion` bump (Settings struct + embedded Dockerfile unchanged).

## What Goes Where
- **Implementation Steps** (checkboxes): all code, test, and doc changes — fully in-repo.
- **Post-Completion** (no checkboxes): user-facing note that existing `network:` configs now break;
  `docker volume prune --filter label=managed-by=makeslop` to reap any orphaned proxy volumes.

## Implementation Steps

### Task 1: Delete the self-contained sidecar files

**Files:**
- Delete: `internal/docker/sidecar.go`
- Delete: `internal/docker/sidecar_test.go`
- Delete: `internal/docker/proxy_integration_test.go`

- [x] delete `internal/docker/sidecar.go` (Sidecar type, `SocatImage`, `proxySocketName`, lifecycle, readiness poll)
- [x] delete `internal/docker/sidecar_test.go`
- [x] delete `internal/docker/proxy_integration_test.go` (build-tagged daemon-gated unix:// test)
- [x] confirm package still references `SocatImage`/`Sidecar` only from not-yet-edited files (expected build break until Task 2/5/6 — note compile errors, do not fix yet)

### Task 2: Remove the proxy branch from `spec.go`

**Files:**
- Modify: `internal/docker/spec.go`
- Modify: `internal/docker/spec_test.go`

- [x] remove `Options.ProxySocketVolume` field
- [x] remove the `proxySocketDir` const
- [x] remove the `BuildSpec` proxy conditional (`NetworkMode="none"`, `HTTP_PROXY`/`HTTPS_PROXY` env, read-only `/sockets` volume mount)
- [x] keep generic `Mount`/`mountsFor` volume support untouched
- [x] delete proxy/`ProxySocketVolume` cases from `spec_test.go`, including `TestHostConfig_NetworkModeNone` (now unreachable); **keep** `TestHostConfig_NetworkModeDefault` (default bridge) and the drift-guard test (still valid)
- [x] run `go test ./internal/docker/` for spec coverage — package compiles for spec; must pass before next task

### Task 3: Remove proxy wiring + `--proxy` flag from `main.go`

**Files:**
- Modify: `cmd/makeslop/main.go`

- [x] remove `validHostRe` and the host/port validation block in `runRun`
- [x] remove the `sidecarRunner` interface and the `newSidecarFn` seam
- [x] remove upstream resolution, volume-name derivation, `sc.Start(...)`, and `defer sc.Close()`
- [x] remove the `--proxy` flag registration on `runCmd`
- [x] remove the `proxyFlag` parameter from `runRun`'s signature and update its call site (bottom of `newRunCmd`, ~line 433)
- [x] remove now-unused imports from `main.go`: `crypto/sha256` (volume-name hash), `net` (`SplitHostPort`), `strconv` (`Atoi`), `regexp` (`validHostRe`) — verified each has no other use; `io`/`os` survive
- [x] leave the `projectconfig.Load` call temporarily adjusted to drop the unused `Network` return (final form set in Task 4)
- [x] run `go build ./cmd/makeslop/` — note any remaining errors from the pending `Load` signature change (fixed in Task 4)

### Task 4: Change `projectconfig.Load` signature and fix all callers

**Files:**
- Modify: `internal/projectconfig/projectconfig.go`
- Modify: `internal/projectconfig/projectconfig_test.go`
- Modify: `cmd/makeslop/main.go`
- Modify: `cmd/makeslop/status.go`
- Modify: `internal/security/security_test.go`
- Modify: `cmd/makeslop/main_test.go`

- [x] delete `type Network` and the `network.proxy.address` nesting from `yamlSchema`
- [x] change `Load` signature `(Excludes, Network, Cache, error)` → `(Excludes, Cache, error)`
- [x] stop emitting the `network:` block in `renderStub`/`Scaffold` stub
- [x] update **all** `Load` call sites to the 3-value destructure — not just proxy ones:
      - `cmd/makeslop/main.go` (`runRun`), `cmd/makeslop/status.go`
      - `internal/security/security_test.go:~58` (`excl, _, _, err := projectconfig.Load(dir)`)
      - `cmd/makeslop/main_test.go:~4503` and `~4529` (non-proxy init / init-global-only tests)
- [x] delete proxy/`ProxyAddress` cases from `projectconfig_test.go`; add/keep a test asserting a `network:` block now fails strict parse with an "unknown field" error
- [x] run `go test ./internal/projectconfig/ ./internal/security/` — must pass before next task

### Task 5: Prune the `apiClient` interface

**Files:**
- Modify: `internal/docker/client.go`
- Modify: `internal/docker/run_test.go`

- [x] remove `ContainerInspect`, `ExecCreate`, `ExecStart`, `ExecInspect`, `VolumeCreate`, `VolumeRemove`, `ImagePull` from the `apiClient` interface
- [x] confirm `var _ apiClient = (*moby.Client)(nil)` still compiles (interface only shrank)
- [x] delete the 7 now-orphaned `fakeClient` methods in `run_test.go` (`ContainerInspect`/`ExecCreate`/`ExecStart`/`ExecInspect`/`VolumeCreate`/`VolumeRemove`/`ImagePull`, ~lines 124-149) — legal extra methods, but dead and uncaught by the grep sweep
- [x] keep `noopImagePullResponse` in `testing.go` — still used by the `FakeBuildClient`/Build flow, not the proxy (confirm before deleting any return type)
- [x] run `go build ./internal/docker/` — must succeed before next task

### Task 6: Strip sidecar surface from `FakeRunClient` in `testing.go`

**Files:**
- Modify: `internal/docker/testing.go`
- Modify: `internal/docker/client_test.go`

- [x] remove sidecar-only `FakeRunClient` fields (`SocatImageMissing`, `SocatImageErr`, `SidecarExited`, `ImagePullCalled`, `ImagePullErr`, `CreatedVolumes`, `RemovedVolumes`, `CreatedVolumeLabels`, `VolumeCreateErr`, `ExecExitCode`, `ExecRunning`, `ExecCreateErr`, `ExecStartErr`, `ExecInspectErr`, `ContainerInspectErr`)
- [x] remove the corresponding fake methods (`ImagePull`, `VolumeCreate`, `VolumeRemove`, `ExecCreate`, `ExecStart`, `ExecInspect`, `ContainerInspect`)
- [x] revert the socat branch in the fake's `ImageInspect` to a plain image lookup
- [x] keep `FakeBuildClient` and the general `ImageMissing`/`PingErr` fields (serve `Run`/preflight)
- [x] delete the sidecar/proxy `FakeRunClient` tests in `client_test.go` (`TestFakeRunClient_VolumeCreate`, `_VolumeRemove`, `_VolumeCreateRemove_RecordBoth`, `_ExecHandshake_*`, `_ContainerInspect_Running`, `_ContainerInspect_SidecarExited`, `_ImagePull_Success`, `_ImagePull_Error`, `_SocatImageMissing`); keep the `*moby.Client` satisfies-`apiClient` assertion test and non-proxy fake tests
- [x] run `go test ./internal/docker/` — package must build **and** pass before next task

### Task 7: Collapse `status` to five checks

**Files:**
- Modify: `cmd/makeslop/status.go`
- Modify: `cmd/makeslop/status_test.go`

- [x] remove check 6 (proxy config / `Network`) and check 7 (socat image / `docker.SocatImage`) — **note:** the proxy `statusCheck` is appended in THREE branches of `status.go` (the `pcErr` error branch ~line 282, the success branch ~line 317, and the workspace-not-resolved `else` branch ~line 329); remove it from all three. Removing the success-branch entry makes the `netCfg` value unused there (absorbed by the `_` from the new 3-value `Load`).
- [x] confirm remaining checks: Daemon, Base config, Image, Workspace, Secret scan (verdict/exit logic unchanged — both removed checks were non-blocking)
- [x] update the `--json` expected shape (five checks)
- [x] delete socat-image and proxy-detail cases from `status_test.go`
- [x] run `go test ./cmd/makeslop/` for status coverage — must pass before next task

### Task 8: Delete proxy tests + fixtures from `main_test.go`

**Files:**
- Modify: `cmd/makeslop/main_test.go`

- [x] delete the ~17 proxy-specific tests (dry-run-with-proxy, proxy-flag-enables, sidecar-receives-upstream, config-proxy-wiring, invalid-proxy validations, sidecar-start-failure, proxy daemon-down/image-missing, etc.)
- [x] delete the `capturedSidecar`/`fakeSidecar`/`setSidecarFnForTest`/`expectedProxyVolumeName` fixtures
- [x] delete the now-vacuous "`--proxy` rejected on non-run commands" tests (generic cobra behavior)
- [x] keep surviving bridge-default dry-run/run tests (direct mode was always the default rendering)
- [x] run `go test ./cmd/makeslop/` — full package must pass before next task

### Task 9: Update documentation

**Files:**
- Modify: `docs/security.md`
- Modify: `docs/architecture.md`
- Modify: `docs/reference.md`
- Modify: `README.md`
- Modify: `CLAUDE.md`

- [x] `docs/security.md`: remove the two-state network model, data-path diagram, and `unix://` section; replace with a short statement that the container uses standard Docker bridge networking with no built-in egress proxy or isolation
- [x] `docs/architecture.md`: remove the `newSidecarFn` seam and socat-sidecar lifecycle sections; trim the `apiClient` method list to the surviving set
- [x] `docs/reference.md`: drop the `--proxy` flag row and the proxy/socat status-check lines
- [x] `README.md`: remove proxy mode, `--proxy`, socat sidecar, `--network none`, and the proxy status-check line
- [x] `CLAUDE.md`: delete the "Network model", "Sidecar", "newSidecarFn seam", "--proxy flag scope", socat-volume notes, and sidecar `apiClient` bullets; add a one-line note that proxy/socat was removed and the container always uses bridge networking
- [x] do NOT touch the two preserved completed-plan files under `docs/plans/completed/`

### Task 10: Verify acceptance criteria
- [x] `go build ./...` clean
- [x] `go vet ./...` clean
- [x] `go test ./...` all green (no Docker daemon needed — integration proxy test removed)
- [x] grep sweep returns empty (except the two preserved completed-plan files):
      `grep -rn -E 'socat|Socat|Sidecar|ProxySocket|proxySocket|proxySocketName|newSidecarFn|validHostRe|ProxyAddress|--proxy|HTTP_PROXY' --include='*.go' --include='*.md' .`
      Remaining benign hits: CLAUDE.md (documents removal), security.md ("no socat sidecar"), main_test.go (checks HTTP_PROXY absent; uses HTTP_PROXY as --build-arg example); spec_test.go cleaned to use FOO=bar/BAZ=qux; completed plan files excluded.
- [x] confirm no orphaned proxy-method bodies remain in `run_test.go` (the grep above won't catch `ExecCreate`/`VolumeCreate` method names — rely on this check + `go vet`)
- [x] verify the Overview end-state holds: app container always bridge, no proxy flag/config/SDK plumbing remains

### Task 11: [Final] Finalize plan
- [x] confirm `CLAUDE.md` reflects the removed sections (done in Task 9)
- [x] move this plan to `docs/plans/completed/`

## Post-Completion
*Items requiring manual intervention or external systems — informational only*

**User-facing breaking change:**
- Existing project `.makeslop.yaml` files containing a `network:` / `network.proxy.address` block
  will now fail `makeslop run`/`status` with a strict-parse "unknown field network" error. Users
  must remove that block manually. This is the intended loud break (locked decision).

**Cleanup of orphaned resources:**
- Any leftover socat proxy volumes from prior runs can be reaped with
  `docker volume prune --filter label=managed-by=makeslop`.
