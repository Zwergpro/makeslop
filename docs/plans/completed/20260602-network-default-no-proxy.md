# Network rewrite: default no-proxy, opt-in remote upstream proxy

## Overview
Invert makeslop's network default. Today the proxy/gateway is **on by default**
(socket-by-default gateway mode), `--no-proxy` turns it off, and
`network.proxy.address` selects upstream mode. This plan flips that:

- **Direct is the new default** — plain Docker bridge networking, app has direct
  internet, no socat/volume/`--network none`.
- **Proxy is opt-in** — enabled only by a new `--proxy ip:port` flag *or* a
  `network.proxy.address` config entry (flag wins). In proxy mode the app is
  airtight (`--network none`) and its sole egress is the volume unix socket →
  socat → `TCP-CONNECT:<ip>:<port>` → a **remote** HTTP proxy.

Because the upstream is a remote `ip:port`, socat connects to it directly over
bridge networking. The host-side Gateway (`internal/networks`) is removed
entirely, along with `host.docker.internal`, probe-dial, request logging, and
the "Docker Desktop only" limitation.

Key benefits: simpler model (two states, not three), no host-side proxy process,
cross-platform (works on native Linux), and the airtight guarantee is preserved
for the proxy case.

## Context (from discovery)
- **Files involved:**
  - `internal/networks/proxy.go` + `internal/networks/proxy_test.go` — DELETE.
  - `internal/docker/sidecar.go` — `Start` signature: port → upstream string.
  - `internal/projectconfig/projectconfig.go` — keep `ProxyAddress`, drop `LogPath`/`network.log`.
  - `cmd/makeslop/main.go` — remove gateway wiring + `newGatewayFn`, add `--proxy`, invert proxy-on condition, drop `--no-proxy`.
  - `cmd/makeslop/status.go` — config-derived proxy detail, drop logging suffix.
  - `internal/docker/spec.go` — UNCHANGED (app-container plumbing stays).
  - Tests: `cmd/makeslop/main_test.go` (heavy: 25 gateway refs), `internal/docker/sidecar_test.go`.
  - `CLAUDE.md` — rewrite the gateway-proxy section.
- **Patterns found:** pure/impure split (spec.go pure), `newClientFn`/`newSidecarFn`/`newGatewayFn` seams, table-driven tests, fail-loud invariants, `net.SplitHostPort` validation for proxy address already in projectconfig.
- **Dependencies:** `internal/networks` is imported only by `cmd/makeslop` (main.go + main_test.go). Removing it has no other consumers.
- **Test command:** `go test ./...` (no Makefile test target).

## Development Approach
- **testing approach:** Regular (code first, then tests) — matches the existing
  table-driven test style in this repo.
- complete each task fully before moving to the next; small focused changes.
- **every task includes new/updated tests** for code changes in that task.
- **all tests must pass before starting the next task.**
- run `go test ./...` after each change.
- the change is intentionally breaking (flag/config surface changes); no
  backward-compat shim for `--no-proxy` (it becomes an unknown flag).

## Testing Strategy
- **unit tests:** required every task. Update sidecar tests for the new `Start`
  signature; rewrite main_test proxy cases for the inverted default; trim
  projectconfig tests for removed `network.log`.
- **no e2e tests** in this project (CLI; gated integration test for Build only).
- The `MAKESLOP_DOCKER_IT` integration test is unaffected (Build path).

## Progress Tracking
- mark completed items with `[x]` immediately when done.
- add newly discovered tasks with ➕ prefix; blockers with ⚠️ prefix.
- keep this plan in sync with actual work.

## Solution Overview
**Two-state model:**

| State | Trigger | Behavior |
|---|---|---|
| Direct (default) | no `--proxy`, no `network.proxy.address` | bridge networking; no socat, no volume, no `--network none`. |
| Proxy | `--proxy ip:port` OR `network.proxy.address` (flag wins) | `--network none`; egress = volume unix socket → socat → `TCP-CONNECT:<ip>:<port>` → remote HTTP proxy. |

**Data path (proxy mode):**
```
app (--network none, HTTP_PROXY=unix:///sockets/proxy.sock)
  → read-only volume → socat sidecar (bridge)
       UNIX-LISTEN:/sockets/proxy.sock,fork,mode=0666
       TCP-CONNECT:<remote-ip>:<remote-port>,reuseaddr
         → remote HTTP proxy
```

**Design decisions:**
- Host Gateway deleted: socat reaches the remote proxy directly, so the dumb
  TCP-splice host process is redundant.
- `--network none` + socat retained for proxy mode: preserves the airtight
  guarantee (apps that ignore `HTTP_PROXY` cannot leak).
- `--proxy` flag wins over `network.proxy.address` so a per-run override works.
- Effective-upstream resolution lives in `runRun`; validation (`net.SplitHostPort`)
  shared between flag and config.

## Technical Details
- **Sidecar.Start:** `Start(ctx context.Context, upstream string, volumeName string) error`.
  socat second arg: `fmt.Sprintf("TCP-CONNECT:%s,reuseaddr", upstream)`.
- **projectconfig.Network:** drop `LogPath` field and the `network.log` parse block;
  keep `ProxyAddress` + its `net.SplitHostPort` validation.
- **main.go proxy resolution:**
  ```go
  upstream := proxyFlag
  if upstream == "" {
      upstream = netCfg.ProxyAddress
  }
  if upstream != "" {
      if _, _, err := net.SplitHostPort(upstream); err != nil { /* fail loud */ }
      // derive volumeName, set opts.ProxySocketVolume, start sidecar
  }
  ```
- **--proxy flag:** `runCmd.Flags().StringVar(&proxyFlag, "proxy", "", "route container egress through a remote HTTP proxy (host:port)")`; scoped to `run` only.
- **status.go:** `netCfg.ProxyAddress != ""` → detail = address; else `"direct (bridge networking)"`. No `(logging → …)` suffix.

## What Goes Where
- **Implementation Steps** (checkboxes): all code, test, and doc changes — fully
  achievable in this repo.
- **Post-Completion**: manual smoke test against a real remote proxy + native-Linux
  daemon (validates the lifted Docker-Desktop limitation).

## Implementation Steps

### Task 1: Sidecar — Start takes an upstream address

**Files:**
- Modify: `internal/docker/sidecar.go`
- Modify: `internal/docker/sidecar_test.go`

- [x] change `Start(ctx, port int, volumeName string)` → `Start(ctx, upstream string, volumeName string)` in `sidecar.go`
- [x] update socat command to `fmt.Sprintf("TCP-CONNECT:%s,reuseaddr", upstream)` (was `host.docker.internal:<port>`)
- [x] update the package/Start doc comments (lines ~8-25): remove the `host.docker.internal` architecture diagram and the "Docker Desktop only" limitation note; describe direct remote-proxy connect over bridge networking
- [x] update the inline comment at the socat-args construction site (~line 110, "bridge networking so host.docker.internal resolves to the host") to describe reaching the remote upstream directly
- [x] update `sidecar_test.go` callers to pass an `upstream` string (e.g. `"10.0.0.5:3128"`) instead of a port; assert the socat cmd contains `TCP-CONNECT:10.0.0.5:3128,reuseaddr`
- [x] add a test case asserting the socat cmd no longer references `host.docker.internal`
- [x] run `go test ./internal/docker/...` — must pass before next task

### Task 2: projectconfig — drop network.log, keep proxy.address

**Files:**
- Modify: `internal/projectconfig/projectconfig.go`
- Modify: `internal/projectconfig/projectconfig_test.go`

- [x] remove the `LogPath` field from the `Network` struct and its doc comment
- [x] scrub the file-header / `Load` doc comments (lines ~20-25) of `networks.Gateway` and `network.log` references
- [x] remove the `schema.Network.Log` parse block (the `if logRel := ...` section)
- [x] remove the now-unused `Log` field from the YAML schema struct if it is only used here
- [x] keep `ProxyAddress` parsing + `net.SplitHostPort` validation unchanged
- [x] delete/adjust tests that asserted `network.log` parsing (`LogPath` population, invalid-log-path errors)
- [x] add/keep a test asserting `network.proxy.address` still parses and validates (valid + invalid host:port)
- [x] run `go test ./internal/projectconfig/...` — must pass before next task

### Task 3: main.go — delete gateway wiring, add --proxy, invert default

**Files:**
- Modify: `cmd/makeslop/main.go`

- [x] remove the `internal/networks` import, the `newGatewayFn` var, and the `*networks.Gateway` wiring in `runRun`
- [x] change `runRun` signature: replace `noProxy bool` with `proxyFlag string`
- [x] resolve effective upstream in `runRun`: `upstream := proxyFlag; if upstream == "" { upstream = netCfg.ProxyAddress }`
- [x] validate `upstream` with `net.SplitHostPort` when non-empty; on error print a clear `makeslop: ...`-prefixed message to stderr and return `errSilent` (fail loud, before pre-flight)
- [x] after removing the `networks` import, run `go build` to confirm no now-unused imports remain (e.g. `crypto/sha256`, `os` — both should still be referenced inside the `upstream != ""` branch)
- [x] gate proxy wiring on `upstream != ""`: derive `volumeName`, set `opts.ProxySocketVolume`, construct sidecar via `newSidecarFn`, call `sc.Start(ctx, upstream, volumeName)`, `defer sc.Close()`; when empty, skip all of it (direct/bridge)
- [x] add `--proxy` `StringVar` flag scoped to `runCmd` only; remove the `--no-proxy` `BoolVar` flag and the `noProxy` variable; update the `RunE` call to pass `proxyFlag`
- [x] update the `runRun` doc comments to describe the two-state model (delete the socket-by-default / `--no-proxy` narrative)
- [x] run `go build ./...` then `go test ./cmd/...` (expect test failures from Task 4 — build must succeed) — proceed to Task 4

### Task 4: main_test.go — rewrite proxy tests for inverted default

**Files:**
- Modify: `cmd/makeslop/main_test.go`

- [x] remove `capturedGateway`, `setGatewayFnForTest`, and all `newGatewayFn`/`cap.proxy`/`cap.logPath`/`networks.` references
- [x] update `fakeSidecar.Start` + `capturedSidecar` to record `(upstream string, volumeName string)` instead of `(port int, ...)` — already done in prior task
- [x] rewrite/replace the `--no-proxy` dry-run test: default `run --dry-run` now produces bridge networking (NO `--network`, NO `HTTP_PROXY`, NO proxy volume) and the sidecar is NOT constructed
- [x] add test: `run --proxy 10.0.0.5:3128 --dry-run` (or non-dry-run with fakes) enables proxy — `opts.ProxySocketVolume` set, `--network none`, `HTTP_PROXY`/`HTTPS_PROXY` present, sidecar `Start` receives `upstream="10.0.0.5:3128"`
- [x] add test: `network.proxy.address` in `.makeslop.yaml` enables proxy with no flag (sidecar `Start` receives the config address)
- [x] add test: `--proxy` overrides `network.proxy.address` (flag value reaches the sidecar)
- [x] add test: invalid `--proxy notaddr` fails loud (non-zero / error) before launch
- [x] add test: `run --no-proxy` now errors as an unknown flag
- [x] delete the existing `TestNoProxy_RejectedOnNonRunCommands` scope-guard test (the `--no-proxy` flag no longer exists)
- [x] add `TestProxy_RejectedOnNonRunCommands` mirroring the `--out-of-home` scope-guard pattern: assert `--proxy` is rejected as unknown on `init`, `version`, `config`, `migrate`, `build`, and `status`
- [x] run `go test ./cmd/...` — must pass before next task

### Task 5: status.go — config-derived proxy detail

**Files:**
- Modify: `cmd/makeslop/status.go`
- Modify: `cmd/makeslop/status_test.go` (or wherever status proxy detail is tested)

- [x] proxy check detail: `netCfg.ProxyAddress != ""` → detail = the address; else `"direct (bridge networking)"`
- [x] update the stale explanatory comment block (~lines 313-316: "gateway (direct egress) default", "upstream proxy is used", "no --no-proxy knowledge") to the two-state, config-derived model
- [x] remove the `(logging → <path>)` suffix logic and any `LogPath` reference
- [x] keep the socat-image check (`docker.ImageExists(docker.SocatImage)`) unchanged
- [x] update status tests: proxy-set case shows the address; unset case shows `"direct (bridge networking)"`; remove logging-suffix assertions
- [x] run `go test ./cmd/...` — must pass before next task

### Task 6: Delete internal/networks package

**Files:**
- Delete: `internal/networks/proxy.go`
- Delete: `internal/networks/proxy_test.go`

- [x] delete both files and the `internal/networks` directory
- [x] `grep -rn "internal/networks\|networks\." --include="*.go" .` — confirm zero remaining references
- [x] run `go build ./...` and `go vet ./...` — must pass before next task

### Task 7: Verify acceptance criteria
- [x] default `makeslop run --dry-run` → bridge networking, no proxy plumbing
- [x] `--proxy ip:port` and `network.proxy.address` both enable airtight proxy mode; flag wins
- [x] invalid `--proxy` value fails loud before container launch
- [x] `--no-proxy` is rejected as an unknown flag
- [x] socat command targets the remote upstream directly (no `host.docker.internal`)
- [x] `internal/networks` and `network.log`/`LogPath` are fully gone
- [x] run full suite: `go test ./...`
- [x] run `go vet ./...`

### Task 8: [Final] Update documentation
- [x] rewrite the CLAUDE.md "Gateway proxy — socat-volume transport" section to the two-state model; delete the three-state table, gateway/upstream split, probe-dial invariant, request-logging section, and the "Docker Desktop only" limitation
- [x] update the `--no-proxy flag scope` section (remove it); add a `--proxy flag scope` section (run only)
- [x] update the `newGatewayFn and newSidecarFn seams` section: remove `newGatewayFn`; note `Sidecar.Start` now takes `upstream string`
- [x] update the `status command` section: proxy check is config-derived (address vs `direct (bridge networking)`), no logging suffix
- [x] note the inverted default in CLAUDE.md; confirm CurrentVersion and MigrationVersion are NOT bumped (Settings struct + embedded Dockerfile unchanged)
- [x] move this plan to `docs/plans/completed/`

## Post-Completion
*Items requiring manual intervention or external systems — informational only*

**Manual verification:**
- Smoke-test `makeslop run --proxy <real-remote-proxy-ip:port>` against a live
  remote HTTP proxy and confirm container egress flows through it.
- Verify on a **native-Linux** (non-Docker-Desktop) daemon that proxy mode now
  works (the lifted `host.docker.internal` limitation) and that direct mode has
  normal internet via the bridge.
- Confirm orphaned per-run volumes are still prunable:
  `docker volume prune --filter label=managed-by=makeslop`.
