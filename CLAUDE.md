# CLAUDE.md — makeslop project notes

## Key architectural patterns

### Pure/impure split
Argv assembly (`internal/docker/spec.go`) is pure and fully table-tested.
Side-effecting SDK calls live in `internal/docker/run.go` and `internal/docker/build.go`.
Keep these separate: pure functions never touch the filesystem or exec anything.

`spec.go` exposes both:
- `Args()`/`ShellCommand()` — argv slices used for `--dry-run` output.
- `ContainerConfig()`/`HostConfig()` — pure projections to SDK structs consumed by `Run`.

These two renderings are kept honest by a drift-guard test.

### testing.go in the production binary (known trade-off)
`internal/docker/testing.go` is compiled into the production binary — it is **not** a `_test.go` file.
This is intentional: `cmd/makeslop/main_test.go` is in `package main`, not `package docker_test`,
so it cannot reach unexported symbols via an `export_test.go` bridge. Shipping the test helpers
(`SetClientForTest`, `SetTTYCheckForTest`, `SetTermMakeRawForTest`, `FakeRunClient`,
`FakeBuildClient`, `SkipNonPOSIX`) into the production binary is the accepted trade-off for
testability. The binary size impact is negligible.

### apiClient seam and SetClientForTest
`internal/docker/client.go` declares a narrow unexported `apiClient` interface with the methods
used by `Run`, `Build`, `Ping`, `ImageInspect`, and the socat sidecar lifecycle. A package-level
`newClientFn` (defaulting to `newClient`, which calls `client.New(client.FromEnv)`) constructs
the live client. A compile-time assertion `var _ apiClient = (*moby.Client)(nil)` guards against
signature drift.

The interface includes (all raw SDK methods from `github.com/moby/moby/client`):
- `ContainerCreate`, `ContainerAttach`, `ContainerStart`, `ContainerWait`, `ContainerResize`, `ContainerRemove` (existing, reused by sidecar using `Force: true`)
- `ContainerInspect(ctx, containerID string, options moby.ContainerInspectOptions) (moby.ContainerInspectResult, error)` — used by sidecar early-exit detection
- `ExecCreate(ctx, container string, options moby.ExecCreateOptions) (moby.ExecCreateResult, error)` — sidecar readiness handshake
- `ExecStart(ctx, execID string, options moby.ExecStartOptions) (moby.ExecStartResult, error)` — sidecar readiness handshake
- `ExecInspect(ctx, execID string, options moby.ExecInspectOptions) (moby.ExecInspectResult, error)` — sidecar readiness exit-code read
- `VolumeCreate(ctx, options moby.VolumeCreateOptions) (moby.VolumeCreateResult, error)` — sidecar volume creation
- `VolumeRemove(ctx, volumeID string, options moby.VolumeRemoveOptions) (moby.VolumeRemoveResult, error)` — sidecar cleanup
- `ImagePull(ctx, refStr string, options moby.ImagePullOptions) (moby.ImagePullResponse, error)` — socat image pull-on-demand
- `ImageBuild`, `DialHijack`, `Ping`, `ImageInspect`, `Close` (existing)

`SetClientForTest(c apiClient) (restore func())` (in `testing.go`) replaces `newClientFn` for the
duration of a test. Ready-made fakes live in `testing.go`:

- **`FakeRunClient`** — simulates the `Run` container lifecycle with a scripted exit code; also
  supports `PingErr` to simulate daemon-down and `ImageMissing` to simulate absent images.
  Extended with scriptable sidecar behavior: records created/removed volumes; models the
  **create→start→inspect** exec handshake; supports `SidecarExited` toggle and
  `SocatImageMissing` toggle.
  ```go
  t.Cleanup(docker.SetClientForTest(docker.NewFakeRunClient(0))) // exit 0
  ```
- **`FakeBuildClient`** — simulates the `Build` SDK call and records `ImageBuildOptions`; also
  supports `PingErr` and `ImageMissing` fields.
  ```go
  fbc := docker.NewFakeBuildClient(0)         // 0 = success
  t.Cleanup(docker.SetClientForTest(fbc))
  // ... call Build ...
  opts := fbc.LastBuildOptions               // inspect what was passed to ImageBuild
  ```

This replaces the old shell-shim machinery (`WriteShim`, `SetDockerBinaryForTest`). There are no
shell shims, no `dockerBinary` global, no `executableTempDir`.

### newGatewayFn and newSidecarFn seams (cmd/makeslop/main.go)
Two package-level seams in `cmd/makeslop/main.go` mirror the `docker.newClientFn` pattern:

**`newGatewayFn`** defaults to `networks.NewGateway`. Tests swap it via `setGatewayFnForTest(t)`,
which installs a replacement that records `(proxy, logPath)` arguments for inspection and
delegates to the real `networks.NewGateway`.

```go
cap := setGatewayFnForTest(t)
// ... run command ...
if !cap.called { t.Fatal("gateway was not constructed") }
// cap.proxy, cap.logPath are available
```

**`newSidecarFn`** defaults to a closure that calls `docker.NewSidecar(quiet, stderr)` and returns
a `sidecarRunner`. Tests swap it via `setSidecarFnForTest(t)`, which returns a `fakeSidecar` that
records `(port, volumeName)` passed to `Start`. To simulate a `Start` failure, set
`cap.startErr` before calling `runCmd`.

```go
cap := setSidecarFnForTest(t)
// ... run command ...
// cap.port, cap.volumeName, cap.called are available
```

The `sidecarRunner` interface (in `main.go`) has `Start(ctx, port int, volumeName string) error`
and `Close() error`; satisfied by `*docker.Sidecar`.

Both seams are used in dry-run and gateway-wiring tests in `main_test.go` where real socket
bind/sidecar creation would be unnecessary or would race with test teardown.

### Shared preflight helpers (`internal/docker/preflight.go`)

`CheckDaemon(ctx context.Context) error` — pings the daemon via `newClientFn`; returns
`ErrDaemonUnreachable` on failure.

`ImageExists(ctx context.Context, image string) (bool, error)` — calls `ImageInspect`; returns
`(true, nil)` when found, `(false, nil)` only when `cerrdefs.IsNotFound(err)`, and `(false, err)`
for any other error (so a dead daemon is never misreported as "image absent").

Both helpers build and close their own client (two constructions per `status` run — accepted for
simplicity).

### Integration test for Build
A gated integration test exercises the full `Build` flow against a live Docker daemon:
```
MAKESLOP_DOCKER_IT=1 go test -tags integration ./internal/docker/
```
This test is skipped during normal `go test ./...` (requires a reachable daemon socket).

### docker.ExitError and the exit-code contract
`docker.ExitError{Code int}` (in `run.go`) is the only exit-code error. `Run` returns it when
`ContainerWait` reports a non-zero `StatusCode`. `runWithExitCode` in `main.go` does:

```go
var ee *docker.ExitError
if errors.As(err, &ee) {
    return ee.Code
}
```

Signal-killed containers (e.g. SIGKILL) are reported by the daemon as `StatusCode=137`; that value
is passed through verbatim. There is no OS `WaitStatus`/`exec.ExitError` handling — makeslop no
longer forks the docker binary.

### BuildKit session build flow
`internal/docker/build.go` implements `Build` via the moby/moby SDK + a BuildKit session:

1. A `session.Session` is created, allowing `filesync` (context dir + dockerfile dir) and
   `authprovider.NewDockerAuthProvider` for registry pulls.
2. A dialer adapter wraps `cli.DialHijack(ctx, "/session", proto, meta)` to match `session.Dialer`'s
   `func(ctx, proto, meta)` signature.
3. `s.Run(ctx, dialer)` is started in a goroutine.
4. `ImageBuild` is called with `Version: build.BuilderBuildKit` (the `"2"` selector) and the
   session ID. No `DOCKER_BUILDKIT` env variable.
5. The response body is decoded as a BuildKit JSON trace stream; `aux` frames carrying
   `moby.buildkit.trace` payloads are decoded into `*client.SolveStatus` and fed to
   `progressui.NewDisplay(...).UpdateFrom` for rendering.
6. The session and client are closed when the build finishes.

`build.go` depends on `github.com/moby/buildkit` (direct dep, pinned in `go.mod`).

### Scan filters are config-driven (no in-code denylist, no engine fallback)
`internal/security.Scan` uses a native Go `filepath.WalkDir` walk — there is no `fd`/`fdfind`
dependency. Patterns (basename globs) and skip-dirs are passed in at call time; the engine has no
hardcoded defaults. If `patterns` is empty, `Scan` returns `nil` immediately (no walk).

Walk errors (e.g. unreadable subdirectory) are **propagated immediately** and abort `runRun` before
`docker.Run`. This "fail-loud" invariant ensures we never silently skip a directory we cannot prove
is secret-free — consistent with the no-`.env`-leak contract.

The defaults live as active values in the `Scaffold` stub (seeded by `makeslop init`). Pre-existing
project `.makeslop.yaml` files are **never** auto-migrated — `MigrationVersion` only refreshes
`~/.makeslop/`, not project-local files. Users with an old stub must manually add an
`exclude.scan` block to restore masking.

### POSIX-only invariant
makeslop targets POSIX systems only. Tests that rely on TTY/signal behavior call `SkipNonPOSIX` at the top.
Do not add Windows compatibility paths.

### TTY requirement is `run`-only
`makeslop run` (formerly `go`) requires an interactive TTY (checked via `ttyCheck`).
`makeslop build`, `makeslop init`, `makeslop migrate`, `makeslop config`, `makeslop status`, and `makeslop version` are CI/pipe-safe and never consult `ttyCheck`.

### Home-directory guard exemptions
`makeslop run` and `makeslop init` enforce the home-directory guard.
`makeslop build`, `makeslop migrate`, `makeslop config`, `makeslop status`, and `makeslop version` are exempt — they operate on `~/.makeslop/` directly
and do not care about the current working directory.

### MigrationVersion-on-Dockerfile-change rule
Whenever `internal/assets/files/Dockerfile` is modified (e.g. multi-arch support), bump
`MigrationVersion` in `internal/config/config.go` so that existing installs pick up the new
Dockerfile on the next `makeslop migrate`. `CurrentVersion` (settings schema version) is separate
and only changes when the `Settings` struct fields change. The two constants serve different
purposes: `CurrentVersion` gates JSON schema compatibility; `MigrationVersion` gates the one-shot
directory refresh.

**Note:** the socat-volume proxy transport change (replacing the host-unix-socket transport) does
**not** bump either `CurrentVersion` or `MigrationVersion` — the `Settings` struct fields are
unchanged and the embedded Dockerfile is unchanged.

### init seed-at-latest and stale-nudge behavior
`makeslop init` detects whether `~/.makeslop/settings.json` already exists **before** calling
`Bootstrap`. On a **fresh seed** (no `settings.json`), after `Bootstrap`, it stamps
`s.MigratedVersion = MigrationVersion` and saves — so a freshly-initialised directory is never
reported as stale. On an **existing-but-stale** directory (`s.MigratedVersion < MigrationVersion`),
it prints a non-blocking nudge to stderr:

```
note: base config is v<latest>, yours is v<current> — run 'makeslop migrate'
```

and continues without failing. `init` does NOT stamp `MigratedVersion` for existing installs — that
would skip the actual migration. The success message changed to:

- stderr: `registered <name> — run 'makeslop build' then 'makeslop run'`
- stdout: bare cache path (unchanged)

Config helpers added in `internal/config/config.go`:
- `BaseConfigExists(baseDir string) (bool, error)` — reports presence of `settings.json`.
- `MigrationStatus(s *Settings) (current, latest int, stale bool)` — compares `s.MigratedVersion`
  to `MigrationVersion`.

### status command
`makeslop status` is an ordered dependency health check:
1. Daemon (`CheckDaemon`) — blocking
2. Base config (`BaseConfigExists`/`MigrationStatus`) — absent/corrupt = blocking (`✗`), stale = non-blocking (`!`)
3. Image (`ImageExists`) — blocking
4. Workspace (`ws.Lookup`) — blocking
5. Secret scan summary (`security.Scan` count) — non-blocking (`–`/`✓`)
6. Proxy (`projectconfig.Load`) — non-blocking; detail is always a string: `gateway (direct egress)` when `ProxyAddress` is empty, the upstream address when set, with an optional ` (logging → <path>)` suffix when `LogPath` is set
7. Socat image (`docker.ImageExists(docker.SocatImage)`) — non-blocking; `✓` when present, `!` with `"alpine/socat absent — will pull on first run"` when absent

Output: aligned lines with glyphs `✓/✗/–/!`; final verdict line + single next action. `--json`
emits `{checks:[{name,state,detail}], ready:bool}`. Exits non-zero when any blocking check fails.
CI/pipe-safe; exempt from the home guard and TTY requirement. Color/glyphs only when stderr is a
TTY and `NO_COLOR` is unset.

### --out-of-home flag scope
`--out-of-home` is registered only on `init` and `run` (not a persistent root flag). Commands
`version`, `config`, `migrate`, `build`, and `status` reject it as an unknown flag.

### --no-proxy flag scope
`--no-proxy` is registered only on `run` (not a persistent root flag). Commands `version`,
`config`, `migrate`, `build`, `init`, and `status` reject it as an unknown flag.

### --quiet flag
`--quiet` is a persistent root flag. When set, stderr chrome (notices, nudges, progress lines such
as `masked N`) is suppressed. Error messages still print to stderr.

### Gateway proxy — socat-volume transport

`makeslop run` wires the network through `internal/networks.Gateway` (host TCP) and
`internal/docker/sidecar.go` (`Sidecar`, the alpine/socat container). The **socat-volume** transport
replaces the previous host-unix-socket bind-mount; there is **no OS/VM detection, no transport
selection knob** — all platforms use the same path.

#### Three-state network model

| State | Trigger | Behavior |
|---|---|---|
| **Gateway (default)** | no `network.proxy.address`, no `--no-proxy` | host TCP gateway + socat sidecar; app gets `--network none` + volume socket |
| **Upstream** | `network.proxy.address` set | host TCP gateway splices to upstream; socat sidecar unchanged; probe-dial fail-loud invariant applies |
| **Off** | `--no-proxy` flag | skip the gateway AND sidecar → docker bridge networking (escape hatch for non-HTTP traffic) |

#### Gateway (TCP-only)
`internal/networks.Gateway` listens on **TCP `127.0.0.1:0`** (OS assigns the ephemeral port).

Signature: `NewGateway(proxy, logPath string) *Gateway` — the `socketPath` argument and
`SocketPath()` method have been **removed**. There are no unix-socket prologue steps
(`os.Remove`, `syscall.Umask`, `chmod 0666`).

New accessors after a successful `Start`:
- `Addr() net.Addr` — the bound TCP address
- `Port() int` — the TCP port (for passing to `Sidecar.Start`)

All ServeHTTP / upstream-splice / logging / upstream probe-dial / `Close()` logic is unchanged.

#### Sidecar (`internal/docker/sidecar.go`)
`Sidecar` manages an `alpine/socat` container that re-exposes the host TCP port as a unix socket
on a Docker **volume** inside the VM.

- `const SocatImage = "alpine/socat@sha256:<digest>"` — pinned by digest; exported so `status.go`
  can check its presence via `docker.ImageExists`.
- `NewSidecar(quiet bool, stderr io.Writer) *Sidecar`
- `Start(ctx context.Context, port int, volumeName string) error`
  1. `ImageInspect` to check presence; if absent, `ImagePull` with a one-line notice (suppressed by `--quiet`); pull failure is fatal with a registry hint.
  2. `VolumeCreate` (per-run name, `managed-by: makeslop` label).
  3. `ContainerCreate` + `ContainerStart` — detached socat container on bridge networking, volume
     mounted **read-write** at `/sockets`. Socat args:
     `UNIX-LISTEN:/sockets/proxy.sock,fork,mode=0666 TCP-CONNECT:host.docker.internal:<port>,reuseaddr`.
  4. Readiness poll (~5 s, 100 ms intervals): `ContainerInspect` (early-exit detection) →
     `ExecCreate`/`ExecStart`/`ExecInspect` (`test -S /sockets/proxy.sock`, exit code 0 = ready).
- `Close() error` — `ContainerRemove(Force:true)` then `VolumeRemove`; best-effort, idempotent.
- `VolumeName() string` — returns the volume name after `Start`; used to set `opts.ProxySocketVolume`.

**No proactive stale-sweep** (no `VolumeList`/`ContainerList`): orphans from killed runs are
tolerated (unique per-run volume name) and prunable via `docker volume prune --filter label=managed-by=makeslop`.

#### Volume mount in `spec.go`
`Options.ProxySocketVolume string` — when non-empty, `BuildSpec` emits:
- `--network none`
- A **read-only** volume mount at `proxySocketDir` (`/sockets`) for the app container.
  (`Args()` → `type=volume,source=<name>,target=/sockets,readonly`; `mountsFor` → `mount.TypeVolume`.)
- `HTTP_PROXY=unix:///sockets/proxy.sock` and `HTTPS_PROXY=unix:///sockets/proxy.sock`.

The socat sidecar mounts the same volume **read-write** (it must create the socket file); the app
container is read-only. `Options.ProxySocketHost` and `Options.ProxySocketContainer` (the old
host-bind-mount fields) have been **removed**.

#### runRun wiring
When `!noProxy`, `runRun` in `cmd/makeslop/main.go`:
1. Derives the per-run volume name `makeslop-sock-<hash>-<pid>`.
2. Constructs the TCP Gateway via `newGatewayFn(proxyAddr, logPath)` and calls `gw.Start`.
3. Sets `opts.ProxySocketVolume = volumeName`.
4. Constructs the Sidecar via `newSidecarFn(quiet, stderr)` and calls `sc.Start(ctx, port, volumeName)`.
5. Calls `docker.Run(...)`.
6. Defers teardown: `sc.Close()` then `gw.Close()`.

Dry-run: the app container spec shows the read-only volume mount + `--network none`. Gateway and
sidecar creation are runtime side-effects invisible to dry-run (consistent with the previous design).
"Printed == executed" holds for the app container spec.

#### Probe-dial invariant (upstream mode only)
In **upstream mode** (`network.proxy.address` non-empty), `Gateway.Start` performs a single TCP
probe-dial of the upstream address before accepting any connections. If the upstream is unreachable,
`Start` tears down the listener and returns the error — the caller (`runRun`) must abort the
container launch. This "fail loud" invariant prevents silent black-holing of container traffic when
the upstream proxy is misconfigured. The probe checks TCP reachability only; it does not validate
HTTP CONNECT protocol.

In **gateway mode** (no upstream address), no probe-dial is performed; the gateway dials targets
on demand via `ServeHTTP`.

Tests that call `Start` in upstream mode must use a live fake upstream (a
`net.Listen("tcp", "127.0.0.1:0")` helper) — passing a dead address such as `127.0.0.1:1` will
cause `Start` to return an error.

#### Request logging (`network.log`, both modes)
When `network.log` is set in `.makeslop.yaml` (a relative path resolved under the project root),
`Gateway.Start` opens the file (`O_CREATE|O_APPEND|O_WRONLY`, 0644) **before** branching into
gateway or upstream mode. If the file cannot be opened, `Start` tears down the listener and returns
the error (fail-loud).

Log line format: `<METHOD> <target>` e.g. `CONNECT api.example.com:443`, `GET http://host/path`.

**Limitation:** plain-HTTP keep-alive connections in upstream mode log only the **first** request
line per connection (the upstream peek reads one `bufio.ReadString('\n')` line). CONNECT (HTTPS,
the dominant case) is exact. Gateway-mode plain HTTP is logged per-request by `ServeHTTP`.

When `network.log` is absent (`logPath == ""`), the upstream code path is byte-for-byte identical
to the pre-logging implementation — no buffering, no parse overhead.

#### Known limitation: Docker Desktop only
> ⚠️ The host proxy binds `127.0.0.1`; the socat sidecar reaches it via
> `host.docker.internal`. This hostname is provided by **Docker Desktop** (macOS and Windows).
> **Native-Linux / non-Docker-Desktop** daemons do **not** supply `host.docker.internal` by
> default and cannot reach a loopback-bound host service via the Docker bridge — those setups
> are out of scope. Revisit if native-Linux support is needed (would require
> `--add-host host.docker.internal:host-gateway` on the sidecar *and* a non-loopback host bind).
