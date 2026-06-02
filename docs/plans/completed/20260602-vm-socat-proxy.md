# Socat-volume proxy transport (replace host-unix-socket gateway)

## Overview

`makeslop run` fails on Docker Desktop / VM-backed Docker (macOS) with:

```
container create: Error response from daemon: invalid mount config for type "bind":
bind source path does not exist: /socket_mnt/private/tmp/makeslop-<hash>-<pid>.sock
```

**Root cause:** the gateway's unix socket is created by a **host** process and bind-mounted into
the app container. On VM-backed Docker the daemon runs in a Linux VM; a host-created unix socket
cannot be represented through the file-sharing layer (`/socket_mnt/...` is the VM's view of the
macOS share), so the daemon cannot find it at mount time.

**Fix (approved, simplified):** **replace** the host-unix-socket transport with a single
**socat-volume** transport for *all* platforms. **No OS/VM detection, no transport selection
knob.** The host-side unix-socket bind-mount path is removed entirely.

How socat-volume keeps the app container airtight:

- The proxy **stays a host process**, but listens on **TCP `127.0.0.1:0`** instead of a unix socket.
- A small `alpine/socat` sidecar **inside the VM** re-exposes that TCP endpoint as a unix socket on
  a Docker **volume** (real VM filesystem): `UNIX-LISTEN:/sockets/proxy.sock,fork,mode=0666
  TCP-CONNECT:host.docker.internal:<port>,reuseaddr`.
- The app container mounts that volume and stays `--network none` with
  `HTTP_PROXY=unix:///sockets/proxy.sock` — its only egress is the volume unix socket, identical in
  spirit to the old design. The socket is created *inside the VM*, so it never crosses the
  file-sharing boundary that broke the original.

**Benefits:** restores `makeslop run` on Docker Desktop; one uniform code path (no detection);
reuses all existing host-side proxy logic (gateway/upstream/logging); no Linux gateway image.

> ⚠️ **Known limitation (deliberate):** with no platform detection, the host proxy binds
> `127.0.0.1` and the sidecar relies on `host.docker.internal` resolving to the host. This is
> correct and safe on **Docker Desktop** (the confirmed target). Native-Linux / non-Docker-Desktop
> daemons do **not** provide `host.docker.internal` by default and cannot reach a loopback-bound
> host service via the docker bridge — those setups are out of scope for this change. Revisit if
> native-Linux support is needed (would require `--add-host host.docker.internal:host-gateway` on
> the sidecar *and* a non-loopback host bind, i.e. the detection we are intentionally dropping).

## Context (from discovery)

- **Files/components involved:**
  - `internal/networks/proxy.go` — host-side `Gateway`: today binds a **unix** socket; ServeHTTP /
    upstream-splice / logging / upstream probe-dial / `Close()` teardown.
  - `internal/docker/spec.go` — pure `BuildSpec` + `Args()`/`HostConfig()` + drift-guard; today emits
    `--network none`, a **read-only socket bind-mount**, and `HTTP_PROXY=unix://…` when
    `ProxySocketHost` is set (`spec.go:128-140`).
  - `internal/docker/client.go` — narrow `apiClient` (raw SDK methods) + `var _ apiClient =
    (*moby.Client)(nil)` + `newClientFn` seam.
  - `internal/docker/testing.go` — `FakeRunClient`/`FakeBuildClient`, `SetClientForTest`.
  - `cmd/makeslop/main.go` — `runRun` (socket path computed at `main.go:170-174`), `newGatewayFn`
    seam (`main.go:35`), `--no-proxy` wiring (`main.go:364`).
  - `cmd/makeslop/main_test.go` — `setGatewayFnForTest`.
  - `cmd/makeslop/status.go` — ordered health-check; proxy line at `status.go:314`.
- **Related patterns found:**
  - Pure/impure split: `spec.go` pure; side-effects in `run.go`/`build.go` (and the new `sidecar.go`).
  - `apiClient` uses **raw** SDK methods (`ContainerCreate`, `ContainerStart`, `ContainerWait`,
    `ImageInspect`, `Ping`, `ImageBuild`, `DialHijack`, …), guarded by a compile-time assertion.
  - Seams as package vars (`newClientFn`, `newGatewayFn`); fakes via `SetClientForTest`.
  - Fail-loud invariants abort launch before `docker.Run`.
- **Dependencies:** `github.com/moby/moby/client`, `github.com/moby/moby/api/types/*`,
  `containerd/errdefs`. New runtime dep: the `alpine/socat` image (pulled on demand, pinned by digest).

## Development Approach

- **Testing approach:** **TDD** where pure (spec rendering); impure pieces (`Gateway` TCP, `Sidecar`)
  use the existing fake-client seam and `net.Listen` fixtures.
- Complete each task fully (incl. tests) before the next; all tests pass before moving on.
- Small, focused changes. This **removes** the old socket transport — update (don't preserve) its tests.
- **Every task includes new/updated tests** as separate checklist items (success + error/edge cases).
- Run `go test ./...` after each change.

## Testing Strategy

- **Unit tests:** required per task. Pure spec rendering is table-driven; impure logic uses
  `SetClientForTest` fakes and local `net.Listen` upstream fixtures.
- **No UI/e2e tests** in this project.
- **Integration:** extend the gated `MAKESLOP_DOCKER_IT=1 go test -tags integration
  ./internal/docker/` suite with a socat-path smoke test (skipped in normal `go test ./...`).
- **Drift-guard:** keep the `Args()`-vs-`HostConfig()` test green after swapping the bind-mount for
  a `volume` mount.

## Progress Tracking

- Mark completed items `[x]` immediately when done.
- New tasks get a `➕` prefix; blockers a `⚠️` prefix.
- Keep this file in sync with actual work; update if scope changes.

## Solution Overview

```
host Gateway (TCP 127.0.0.1:<port>)
      ▲  host.docker.internal:<port>
      │
┌─────┴──────────────┐   makeslop-sock-<hash>-<pid>  (Docker volume, in-VM)
│ alpine/socat        │   UNIX-LISTEN:/sockets/proxy.sock,fork,mode=0666
│ sidecar (bridge)    │◄──────────── volume ────────────► app container
└─────────────────────┘                                   (--network none,
                                                            HTTP_PROXY=unix:///sockets/proxy.sock)
```

**Key design decisions & rationale:**
- **No detection.** Single transport for everyone; removes `resolveTransport`, GOOS/`Info` checks,
  and any `network.transport` knob from the design.
- **Proxy stays a host process** → reuse all of `proxy.go`, keep `network.log` as a direct host-side
  file write, keep the upstream probe-dial fail-loud on the host. No Linux gateway image.
- **Socket created in the VM** by socat → sidesteps the file-sharing boundary that broke the original.
- **Host proxy binds `127.0.0.1`**; sidecar reaches it via `host.docker.internal` (built into Docker
  Desktop). See the ⚠️ limitation above.

**Security note (preserve in code comments):** the app container stays airtight (`--network none`,
sole egress = the volume unix socket). The one exposure is the host proxy listening on a TCP port
reachable via `host.docker.internal` (any local process / bridge container could use it). Mitigated
by binding `127.0.0.1` + ephemeral port. In gateway mode this grants nothing new (local processes
already have direct internet — negligible additional exposure for a single-user dev host); in
upstream mode other local processes could borrow the upstream / pollute `network.log` — acceptable
for a single-user dev tool.

## Technical Details

- **`Gateway` (TCP-only):** `NewGateway(proxy, logPath)` — the `socketPath` argument and
  `SocketPath()` are **removed**. `Start` does `net.Listen("tcp", "127.0.0.1:0")`, with no unix
  prologue (`os.Remove`, umask, `chmod 0666`) and no 108-byte path concern. Adds `Addr() net.Addr` /
  `Port() int`. All ServeHTTP / upstream-splice / logging / probe-dial / `Close()` logic is reused.
- **`apiClient` additions** (raw SDK methods, guarded by the assertion; add only those used):
  `VolumeCreate`, `VolumeRemove`, `ContainerInspect`, `ContainerExecCreate`, `ContainerExecStart`,
  `ContainerExecInspect`, `ImagePull`. (`ContainerCreate`/`ContainerStart`/`ContainerRemove` already
  exist on the interface — `client.go:15-20`; the sidecar reuses them, using `ContainerRemove{Force:
  true}` instead of a separate stop.) **No `Info`** (no detection). **No `VolumeList`/`ContainerList`**
  — proactive stale-sweep-on-start is deliberately dropped (see Task 4 / YAGNI note); cleanup relies
  on idempotent `Close()` teardown + a per-run volume name + a `makeslop` label for manual prune.
  > ⚠️ **Signature risk:** this project pins `github.com/moby/moby/client v0.4.1` (a restructured
  > client whose method set/signatures differ from the classic `docker/docker/client`). Each added
  > method's exact signature MUST be verified against the pinned SDK (`go doc`) before coding, or the
  > compile-time assertion `var _ apiClient = (*moby.Client)(nil)` (`client.go:29`) breaks the build.
  > The `noopClient`/fake stubs in `testing.go` must gain matching methods to keep the assertion green.
  Fakes gain scriptable volume/exec/inspect/pull behavior, modelling the **create→start→inspect** exec
  handshake (not a single call) so readiness tests match production call order.
- **`spec.go`:** **remove** `Options.ProxySocketHost`/`ProxySocketContainer` and the bind-mount
  branch. Add `Mount.Type == "volume"` (`Args()` → `type=volume,source=,target=`; `mountsFor` →
  `mount.TypeVolume`). Add `Options.ProxySocketVolume` (volume name); when set, emit `--network none`,
  the volume mount at a fixed container path (`const proxySocketDir = "/sockets"`,
  socket `/sockets/proxy.sock`), and `HTTP_PROXY/HTTPS_PROXY=unix:///sockets/proxy.sock`. Stays pure.
- **`Sidecar`** (`internal/docker/sidecar.go`): `Start(ctx, port) error` — sweep stale
  `makeslop-sock-*` volumes/containers; ensure `alpine/socat` present (`ImageInspect`; else
  `ImagePull` + `--quiet`-gated notice; fail-loud on pull error); `VolumeCreate`; create+start the
  detached socat container (bridge net, volume at `/sockets`, args as above); readiness poll
  (`ContainerExec test -S /sockets/proxy.sock`, bounded ~5s, abort on sidecar early-exit). `Close()` —
  stop+rm sidecar then `VolumeRemove` (best-effort, reverse order, idempotent). `socatImage` pinned by
  digest.
- **`runRun`:** unchanged unless `!noProxy`; then construct the TCP Gateway, `gw.Start` → port, derive
  `makeslop-sock-<hash>-<pid>`, set `opts.ProxySocketVolume`, start the `Sidecar` (via a new
  `setSidecarFnForTest` seam) after the gateway and before `docker.Run`, defer `Close()` (sidecar then
  gateway). `--no-proxy` and `--quiet` behavior preserved; upstream (`network.proxy.address`) and
  `network.log` still feed the Gateway.
- **Versioning:** no `config.Settings` change → **no `CurrentVersion` bump**; embedded Dockerfile
  unchanged → **no `MigrationVersion` bump**.

## What Goes Where

- **Implementation Steps** (`[ ]`): all code, tests, and in-repo docs below.
- **Post-Completion** (no checkboxes): live-daemon manual verification on Docker Desktop; offline
  `alpine/socat` provisioning; whether native-Linux support is ever needed.

## Implementation Steps

### Task 1: Convert `Gateway` to TCP-only

**Files:**
- Modify: `internal/networks/proxy.go`
- Modify: `internal/networks/proxy_test.go`

- [x] change `NewGateway` to `NewGateway(proxy, logPath string)`; remove the `socketPath` field, `SocketPath()`, and the unix-only prologue (`os.Remove`, `syscall.Umask`, `chmod 0666`) from `Start`.
- [x] make `Start` bind `net.Listen("tcp", "127.0.0.1:0")`; add `Addr() net.Addr` and `Port() int`; update the package/Start doc comments (drop the 108-byte socket note).
- [x] keep ServeHTTP / upstream-splice / logging / upstream probe-dial / `Close()` working over TCP.
- [x] rewrite `internal/networks/proxy_test.go` fixtures: migrate **every** `NewGateway(sock, proxy, log)` call (~30 sites, e.g. `proxy_test.go:75,108,139,200,…,1060`) to the 2-arg `NewGateway(proxy, log)`; delete `TestSocketPath_Accessor` (`proxy_test.go:90-97`); drop the temp-`sock`-path helpers and replace with a "what port did it bind" helper using `g.Port()`.
- [x] write tests: `Start` binds and reports a non-zero `Port()`; gateway-mode CONNECT round-trip over TCP; upstream-mode probe-dial fail-loud still tears down on a dead upstream; logging records `CONNECT host:443`.
- [x] run tests — must pass before next task.

### Task 2: Extend `apiClient` (volume/exec/list/pull) + fakes

**Files:**
- Modify: `internal/docker/client.go`
- Modify: `internal/docker/testing.go`
- Modify: `internal/docker/testing_test.go` (or wherever fakes are exercised; create if absent)

- [x] **first, verify signatures:** `go doc` each method against the pinned `moby/moby/client v0.4.1`; record the exact options/result struct types and the `ContainerExecInspect` result field that carries the exit code. Prune to only methods actually called.
- [x] add to `apiClient`: `VolumeCreate`, `VolumeRemove`, `ContainerInspect`, `ExecCreate`, `ExecStart`, `ExecInspect`, `ImagePull` (reuse existing `ContainerCreate`/`ContainerStart`/`ContainerRemove`); keep `var _ apiClient = (*moby.Client)(nil)` (`client.go:29`) compiling.
- [x] update the `noopClient` and `FakeBuildClient`/`FakeRunClient` types in `testing.go` with matching method stubs so the assertion and the build stay green.
- [x] extend `FakeRunClient` with scriptable behavior: record created/removed volumes; model the **create→start→inspect** exec handshake with a scripted exit code (for readiness); a "sidecar exited early" toggle (drives `ContainerInspect`); an "image missing then pulled" toggle.
- [x] write tests asserting the fake records volume create/remove and returns scripted exec/inspect/pull results in the production call order.
- [x] run tests — must pass before next task.

### Task 3: Replace bind-mount with `volume` mount in the pure spec

**Files:**
- Modify: `internal/docker/spec.go`
- Modify: `internal/docker/spec_test.go`

- [x] remove `Options.ProxySocketHost` and `Options.ProxySocketContainer` and the read-only bind-mount branch in `BuildSpec`.
- [x] add `Mount.Type == "volume"` rendering: `Args()` → `type=volume,` + csv `source=`/`target=`; `mountsFor` → `mount.TypeVolume`.
- [x] add `Options.ProxySocketVolume`; when set, emit `--network none`, a **read-only** volume mount at `proxySocketDir` (`/sockets`) for the app container (it only connects to the socket, never writes), and `HTTP_PROXY/HTTPS_PROXY=unix:///sockets/proxy.sock`. Keep `BuildSpec` pure and mount ordering deterministic. (The socat sidecar — Task 4 — mounts the same volume **read-write** so it can create the socket; document the asymmetry.)
- [x] write table tests for `Args()` and `HostConfig()` volume-mount rendering (incl. volume names needing CSV quoting), the read-only flag on the app mount, and the env/`--network none` emission.
- [x] update the `Args()`-vs-`HostConfig()` drift-guard to cover the volume mount; remove obsolete bind-mount socket assertions.
- [x] run tests — must pass before next task.

### Task 4: `Sidecar` — volume + socat lifecycle (in-VM socket)

**Files:**
- Create: `internal/docker/sidecar.go`
- Create: `internal/docker/sidecar_test.go`

- [x] obtain and record a concrete `alpine/socat` digest; define `socatImage = "alpine/socat@sha256:<digest>"`, `proxySocketDir`/socket constants, and a `Sidecar` type (volume name, host port, quiet flag, stderr writer). Confirm `ImagePull` accepts a by-digest reference on the pinned SDK.
- [x] implement `Start(ctx, port)`: ensure `socatImage` present (`ImageInspect`; else `ImagePull` + `--quiet`-gated one-line notice; fail-loud with remedy on pull failure); `VolumeCreate` (per-run name + a `makeslop` label for manual prune); create+start detached socat container **read-write** on the volume at `/sockets` (bridge net), args `UNIX-LISTEN:/sockets/proxy.sock,fork,mode=0666 TCP-CONNECT:host.docker.internal:<port>,reuseaddr`.
- [x] implement readiness using the **create→start→inspect** exec handshake: `ContainerExecCreate` `test -S /sockets/proxy.sock` → `ContainerExecStart` → `ContainerExecInspect` reading the exit-code field; bounded retry (~5s). On each iteration, if the sidecar exited early (`ContainerInspect`), abort loudly with its exit info.
- [x] implement `Close()`: `ContainerRemove{Force:true}` the sidecar, then `VolumeRemove` (best-effort, reverse order, idempotent).
- [x] **YAGNI note:** no proactive stale-sweep-on-start (no `VolumeList`/`ContainerList`); orphans from killed runs are tolerated (unique per-run name) and prunable via the `makeslop` label. Reintroduce a sweep only if orphan accumulation proves a problem.
- [x] write tests (via `SetClientForTest`): happy path Start→ready→Close; image absent → pull invoked; pull failure → fail-loud; sidecar early-exit → Start errors and tears down; readiness timeout → error; `Close()` is idempotent and removes volume + container.
- [x] run tests — must pass before next task.

### Task 5: Wire socat transport into `runRun`

**Files:**
- Modify: `cmd/makeslop/main.go`
- Modify: `cmd/makeslop/main_test.go`

- [x] update the `newGatewayFn` seam to `NewGateway(proxy, logPath)`; update `setGatewayFnForTest` (`main_test.go:41-51`) — drop `sockPath` from the `capturedGateway` struct (`main_test.go:33`) and the closure (`main_test.go:45-47`); add captured `port`/`volume` as needed. Fix the hand-rolled override at `main_test.go:3763-3767`.
- [x] in `runRun`, replace the socket-path block (`main.go:170-174`): when `!noProxy`, build the TCP Gateway, `gw.Start` → `port`, derive `makeslop-sock-<hash>-<pid>`, set `opts.ProxySocketVolume`.
- [x] add a `setSidecarFnForTest` seam (mirror `newGatewayFn`); start the `Sidecar(port)` after `gw.Start` and before `docker.Run`; defer teardown (sidecar `Close()` then `gw.Close()`).
- [x] preserve `--no-proxy` (no gateway, no sidecar, bridge networking) and `--quiet` (gate the pull notice); keep upstream (`network.proxy.address`) and `network.log` feeding the Gateway.
- [x] dry-run: app container shows the read-only `/sockets` volume mount + `--network none` (sidecar/volume creation are runtime side-effects, invisible to dry-run, as the gateway was); preserve "printed == executed" for the app container.
- [x] update the breaking expected-spec builders that reference removed fields: `main_test.go:1198-1199`, `1762-1763`, `2088-2089`, `3656-3697` (all use `ProxySocketHost: cap.sockPath` / `ProxySocketContainer:`); switch them to `ProxySocketVolume` + captured volume name.
- [x] **delete** `TestRun_SocketPathLength_AtMost108Bytes` (`main_test.go:2163-2177`) and any `computeSocketPath`/`sockaddrUnLimit` helpers it uses — the 108-byte sockaddr_un concern no longer exists.
- [x] **rework** the dry-run "must NOT create the socket file" test (`main_test.go:2014-2018`): there is no host socket now — assert instead that dry-run starts neither the gateway nor the sidecar and creates no volume.
- [x] write tests: dry-run shows the read-only volume mount + `--network none`; `setSidecarFnForTest`/`setGatewayFnForTest` capture the volume name + port; `--no-proxy` wires neither (adapt `main_test.go:2126-2127`); `gw.Start` failure aborts before `docker.Run`; `Sidecar.Start` failure aborts before `docker.Run`.
- [x] run tests — must pass before next task.

### Task 6: `status` — socat image check

**Files:**
- Modify: `cmd/makeslop/status.go`
- Modify: `cmd/makeslop/status_test.go`

- [x] add a non-blocking check that `alpine/socat` is present (`ImageExists`): `✓` when present, `!`/`–` with a "run will pull it" hint when absent (not a hard `✗` — pull-on-demand covers it). Keep glyphs / `--json` / `NO_COLOR` / TTY rules per existing conventions.
- [x] **preserve** the existing upstream-vs-gateway-vs-logging detail logic on the proxy line (`status.go:314-325`) — the upstream/direct/`network.log` distinction is still meaningful; only optionally annotate that egress routes via the sidecar. Do not regress the strings `status_test.go` asserts.
- [x] write tests: socat image present → ok glyph; absent → non-blocking hint; existing upstream/gateway/log detail cases stay green; `--json` includes the new check/detail.
- [x] run tests — must pass before next task.

### Task 7: Verify acceptance criteria

**Files:** (none — verification only)

- [x] verify: the host-unix-socket transport is fully removed (no `ProxySocketHost`, no `SocketPath`, no unix `net.Listen`); the app container is always `--network none` + volume socket; logging/upstream/probe-dial preserved.
- [x] verify edge cases: `--no-proxy` short-circuits the gateway+sidecar; stale sweep; readiness timeout/early-exit fail-loud; socat image pull-on-demand and pull-failure remedy.
- [x] run full suite: `go test ./...`.
- [x] run linter if configured (`golangci-lint run` if present).
- [x] (optional, manual) gated integration: `MAKESLOP_DOCKER_IT=1 go test -tags integration ./internal/docker/` incl. a socat-path smoke test on a live Docker Desktop daemon. [skipped - requires live daemon]

### Task 8: Documentation + close out

**Files:**
- Modify: `CLAUDE.md`
- Modify: `docs/plans/20260602-vm-socat-proxy.md` (this file)

- [x] rewrite the CLAUDE.md gateway sections: the transport is now **socat-volume only** (no host unix socket, no detection); document the host TCP bind + `host.docker.internal` reliance, the `alpine/socat` digest pin + pull-on-demand, the new `Sidecar` type + `setSidecarFnForTest` seam, the `Gateway` TCP-only change + new `NewGateway` signature, and the volume `Mount` type.
- [x] record the ⚠️ native-Linux / non-Docker-Desktop limitation in CLAUDE.md, and note that neither `CurrentVersion` nor `MigrationVersion` changes.
- [x] remove now-stale CLAUDE.md text (108-byte socket limit, `ProxySocketHost` bind-mount, "container is locked to --network none via the host socket").
- [x] reconcile with the **already-uncommitted** CLAUDE.md edits (it shows `M` in `git status`) — rebase the rewrite onto the in-flight changes rather than clobbering them.
- [x] mark all checkboxes; move this plan to `docs/plans/completed/` (`mkdir -p docs/plans/completed`).

## Post-Completion

*Items requiring manual intervention or external systems — informational only.*

**Manual verification:**
- Live run on **macOS Docker Desktop**: `makeslop run` succeeds; container has no network interface;
  egress works via the proxy; `network.log` records requests; teardown removes the sidecar + volume;
  stale leftovers from a killed run are swept on the next run.
- Upstream mode: probe-dial fail-loud aborts on a dead upstream; logging records the first CONNECT line.
- Confirm `host.docker.internal` reaches the loopback-bound host proxy on the target Docker Desktop version.

**External / release considerations:**
- Pre-pull / mirror the pinned `alpine/socat` digest for offline installs; document the new dependency.
- ⚠️ Native-Linux / non-Docker-Desktop support is intentionally out of scope (see the Overview
  limitation). If needed later, reintroduce platform-aware bind/`--add-host` handling.
- Revisit the host-TCP exposure for multi-user hosts (optional per-run `Proxy-Authorization`).
