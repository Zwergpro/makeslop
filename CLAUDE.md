# CLAUDE.md — makeslop project notes

## Documentation layout
User-facing docs live in `docs/` (`reference.md`, `security.md`, `architecture.md`). CLAUDE.md is agent-facing notes only.

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

### newSidecarFn seam (cmd/makeslop/main.go)
One package-level seam in `cmd/makeslop/main.go` mirrors the `docker.newClientFn` pattern:

**`newSidecarFn`** defaults to a closure that calls `docker.NewSidecar(quiet, stderr)` and returns
a `sidecarRunner`. Tests swap it via `setSidecarFnForTest(t)`, which returns a `fakeSidecar` that
records `(upstream, volumeName)` passed to `Start`. To simulate a `Start` failure, set
`cap.startErr` before calling `runCmd`.

```go
cap := setSidecarFnForTest(t)
// ... run command ...
// cap.upstream, cap.volumeName, cap.called are available
```

The `sidecarRunner` interface (in `main.go`) has `Start(ctx context.Context, upstream string, volumeName string) error`
and `Close() error`; satisfied by `*docker.Sidecar`.

The seam is used in proxy-wiring tests in `main_test.go` where a real sidecar container would be
unnecessary or would race with test teardown.

### Signal-cancellable root context

`runWithExitCode` (in `cmd/makeslop/main.go`) creates a signal-cancellable context and calls
`cmd.ExecuteContext(ctx)`:

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()
// ... cmd.ExecuteContext(ctx) ...
```

This keeps `runWithExitCode`'s unexported function signature unchanged so the ~21 call sites in `main_test.go`
need no edits. Every subcommand's `RunE` receives a live, cancellable `cmd.Context()`. A
`Ctrl-C` / `SIGTERM` cancels that context, which propagates through `docker.Run`, `docker.Build`,
and the sidecar lifecycle via the standard Go context machinery.

`main()` itself stays thin — it only calls `runWithExitCode(baseDir, os.Stdout, os.Stderr, os.Args[1:])` and exits with the code.

### Shared preflight helpers (`internal/docker/preflight.go`)

`CheckDaemon(ctx context.Context) error` — pings the daemon via `newClientFn`; returns
`ErrDaemonUnreachable` on failure.

`ImageExists(ctx context.Context, image string) (bool, error)` — calls `ImageInspect`; returns
`(true, nil)` when found, `(false, nil)` only when `cerrdefs.IsNotFound(err)`, and `(false, err)`
for any other error (so a dead daemon is never misreported as "image absent").

Both helpers build and close their own client (two constructions per `status` run — accepted for
simplicity).

### Preflight timeouts

`const preflightTimeout = 10 * time.Second` (in `internal/docker/preflight.go`) is the maximum
time allowed for daemon-ping and image-inspect calls on the preflight paths (`runRun` and
`status`). A stalled or black-hole `DOCKER_HOST` is bounded by this timeout.

`WithPreflightTimeout(parent context.Context) (context.Context, context.CancelFunc)` wraps parent
with that deadline. Callers must `defer cancel()`.

Applied at: `CheckDaemon` and `ImageExists` call sites in `runRun` and `cmd/makeslop/status.go`.
**Not** applied inside `docker.Run` or `docker.Build` — those are long-lived interactive sessions
that the signal context (from `runWithExitCode`) cancels on Ctrl-C instead.

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
   `progressui.NewDisplay(...).UpdateFrom` for rendering via a **lazy-display** pattern (see below).
6. The session and client are closed when the build finishes.

`build.go` depends on `github.com/moby/buildkit` (direct dep, pinned in `go.mod`).

### renderBuildOutput lazy-display pattern
`renderBuildOutput` in `build.go` uses a lazy `progressui` display: the display and its goroutine
are created only on the **first BuildKit trace frame** (aux frame with `moby.buildkit.trace` key).
If no trace frame arrives (empty body, plain stream, or immediate error), no display goroutine is
ever spawned. This design lets the plain-fallback and error paths avoid touching `progressui`
entirely while still rendering build steps live (the channel is fed concurrently with decoding, not
after EOF). `decodeBuildKitAux` is a pure helper.

**Wire format for `decodeBuildKitAux`:** the daemon emits each BuildKit trace frame as a
top-level id-keyed `jsonstream.Message`:
```json
{ "id": "moby.buildkit.trace", "aux": "<base64-of-proto-StatusResponse>" }
```
`decodeBuildKitAux` gates on `msg.ID == "moby.buildkit.trace"`, unmarshals `*msg.Aux` into
`[]byte` (base64 JSON string → proto bytes), then `proto.Unmarshal`s into
`controlapi.StatusResponse` and returns `bkclient.NewSolveStatus(&sr)`. Any other `ID` (e.g.
`moby.image.id`), nil `Aux`, bad base64, or bad proto bytes returns `nil` (frame dropped
gracefully).

**Mixed-mode discard:** once the progressui display goroutine is active (after the first trace
frame), any subsequent `Stream`/`Status` plain messages in the same body are **silently discarded**
(not written to stdout). This prevents a concurrent-write race between the decode loop and the
progressui goroutine both writing to the same stdout handle. Pre-trace plain messages (before the
first trace frame) are written immediately to stdout as always.

**Context error suppression:** `dispErr` from `d.UpdateFrom(ctx, statusCh)` is filtered: if it is
`context.Canceled` or `context.DeadlineExceeded`, it is suppressed and `renderBuildOutput` returns
`nil` for the display error. Only the `decodeErr` (from the JSON decode loop) is returned in that
case. The rationale: a context cancellation is intentional and the caller already knows about it;
surfacing it as a display error would be noise. Non-context display errors (e.g. a write failure
to stdout) are still returned wrapped as `"render progress: …"`.

**Test fixture rule:** test fixtures for trace frames must be built via the `realDaemonFrame(t,
sr)` helper in `build_test.go` — proto.Marshal → json.Marshal → `jsonstream.Message{ID:
"moby.buildkit.trace", Aux: &raw}`. Never use the nested-map shape
(`{"moby.buildkit.trace": "<base64>"}` with no `ID` field) — that is the old buggy format that
caused the silent-build regression. Tests that send trace frames must capture stdout (not use
`io.Discard`) and assert that expected vertex names appear, so a future decoder regression
produces a test failure instead of silent empty output.

### Dockerfile staging (`stageDockerfile`)

`stageDockerfile(path string) (dir string, cleanup func(), err error)` (in `build.go`) isolates
the Dockerfile before handing it to the BuildKit filesync sync. It:

1. Reads the Dockerfile bytes via `os.ReadFile(path)`.
2. Creates a fresh `os.MkdirTemp("", "makeslop-dockerfile-*")` directory.
3. Writes `filepath.Join(tmp, "Dockerfile")`.
4. Returns `(tmp, cleanup, nil)` where `cleanup` calls `os.RemoveAll(tmp)`.

`build()` syncs the staged directory under `dockerui.DefaultLocalNameDockerfile` instead of the
original parent directory (`~/.makeslop/`). This means only the single Dockerfile is ever sent to
the build daemon — credentials (`.claude.json`) and the workspace cache tree (`workspaces/`) that
live alongside it in `~/.makeslop/` are never exposed.

`buildImageOptions` still requests `Dockerfile: filepath.Base(o.DockerfilePath)` == `"Dockerfile"`,
so the daemon-requested name is unchanged.

**`.dockerignore` siblings are intentionally not staged** — none are expected in `~/.makeslop/`
and the build context sent to the daemon is empty anyway.

### `build --refresh` semantics
`makeslop build --refresh` calls `config.WriteDockerfile(baseDir)` after `config.Bootstrap` and
before `config.Load`. It atomically overwrites `~/.makeslop/Dockerfile` from the embedded
`assets.Dockerfile` and prints a stderr notice (suppressed by `--quiet`). It does **not** touch
`MigratedVersion` or any migration state — `migrate` is the sole owner of version tracking. Use
`--refresh` to reset a hand-edited Dockerfile to the shipped version without a full `migrate` step.

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

### Per-workspace cache mount overlays (`cache:` block in `.makeslop.yaml`)
`internal/projectconfig/projectconfig.go` parses an optional `cache:` block:

```yaml
cache:
  content: true   # mount docs/ + CLAUDE.md from per-workspace cache (default: true)
  agent: true     # mount .claude/ + .codex/ from per-workspace cache (default: true)
```

`projectconfig.Load` returns a `Cache{Content bool, Agent bool}` (fourth return value). Absent
block ⇒ `{true, true}` ⇒ identical to pre-feature behavior (backward compatible).

The values flow into `docker.Options.MountContentCache` and `docker.Options.MountAgentCache` in
`runRun`. `BuildSpec` gates the two per-workspace mount groups on these booleans; global mounts
(`~/.makeslop/.claude/`, `.claude.json`, `.codex/`) are always present.

`Scaffold(root, Cache)` writes the stub with the `cache:` block; `Stub` is the default rendering
(`{true, true}`). The `init --global-only` flag calls `Scaffold(root, Cache{false, false})`,
scaffolding a file that disables both overlay groups. `Scaffold` is idempotent (EEXIST = success).

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
Whenever `internal/assets/files/Dockerfile` is modified, bump `MigrationVersion` in
`internal/config/config.go` so that existing installs pick up the new Dockerfile on the next
`makeslop migrate`. Also bump `MigrationVersion` when a migration step is added or changed.

`CurrentVersion` (settings schema version) is separate and only changes when the `Settings`
struct fields change. The two constants serve different purposes: `CurrentVersion` gates JSON
schema compatibility; `MigrationVersion` gates the one-shot directory refresh.

One constant per concern. No change to the `Settings` struct = no `CurrentVersion` bump.
No change to the Dockerfile or migration steps = no `MigrationVersion` bump.

**Note:** the socat-volume proxy transport change (replacing the host-unix-socket transport) does
**not** bump either `CurrentVersion` or `MigrationVersion` — the `Settings` struct fields are
unchanged and the embedded Dockerfile is unchanged.

**Note:** the network-default inversion (proxy off by default, opt-in via `--proxy`/`network.proxy.address`)
does **not** bump either `CurrentVersion` or `MigrationVersion` — the `Settings` struct fields are
unchanged and the embedded Dockerfile is unchanged.

**Note:** the configurable per-workspace cache mount overlays (`cache:` block in `.makeslop.yaml`,
`init --global-only` flag, `MountContentCache`/`MountAgentCache` in `docker.Options`) do **not**
bump either `CurrentVersion` or `MigrationVersion` — the `Settings` struct fields are unchanged
and the embedded Dockerfile is unchanged. The `cache:` block lives in the per-project
`.makeslop.yaml`, not in `~/.makeslop/settings.json`.

### init seed-at-latest and stale-nudge behavior
`makeslop init` detects whether `~/.makeslop/settings.json` already exists **before** calling
`Bootstrap`. On a **fresh seed** (no `settings.json`), after `Bootstrap`, it stamps
`s.MigratedVersion = MigrationVersion` and saves — so a freshly-initialised directory is never
reported as stale. On an **existing-but-stale** directory (`s.MigratedVersion < MigrationVersion`),
it prints a non-blocking nudge to stderr:

```
note: your base config is v<current>, latest is v<latest> — run 'makeslop migrate'
```

and continues without failing. `init` does NOT stamp `MigratedVersion` for existing installs — that
would skip the actual migration. The success message changed to:

- stderr: `registered <name> — run 'makeslop build' then 'makeslop run'`
- stdout: bare cache path (unchanged)

Config helpers added in `internal/config/config.go`:
- `BaseConfigExists(baseDir string) (bool, error)` — reports presence of `settings.json`.
- `MigrationStatus(s *Settings) (current, latest int, stale bool)` — compares `s.MigratedVersion`
  to `MigrationVersion`.

### `config.WithLock` — settings.json RMW invariant

`config.WithLock(baseDir string, fn func() error) error` (in `internal/config/lock.go`) provides
two-level mutual exclusion around every Load→mutate→Save sequence on `settings.json`:

1. **In-process `sync.Mutex`** — serializes goroutines within the same binary (on Linux, `flock(2)`
   does NOT block concurrent flock calls from different file descriptors in the same process, so a
   Go `sync.Mutex` is required to fill that gap).
2. **POSIX `flock(LOCK_EX)`** on `<baseDir>/.settings.lock` — guards against concurrent
   invocations from separate processes (e.g. two concurrent `makeslop init` shells).
   The lock file is created with mode `0o600` on first use and is never deleted — it is a
   permanent artifact in `~/.makeslop/` and is safe to ignore in directory listings.

Both layers are released before `WithLock` returns, whether `fn` succeeds or errors.

**NO-NESTING INVARIANT:** `WithLock` MUST NOT be nested. A nested call in the same goroutine
deadlocks on `inProcessMu`. The design relies on each Load→mutate→Save site taking its own
short-lived lock *sequentially*, never spanning multiple RMW sites in one outer lock.

Sites currently protected:
- `workspace.Init` — registers a new workspace entry in `settings.json`.
- `init` RunE (fresh-seed re-stamp) — stamps `MigratedVersion` after workspace registration.
- `configSetCmd` RunE (`config set` RMW) — Load→ConfigSet→Save for `makeslop config set`.
- `config.Migrate` — stamps `MigratedVersion` after running migration steps.

**Two sequential (non-nested) acquisitions in `init`:** `ws.Init` acquires and releases its lock,
then returns; the `init` RunE then takes a separate lock for the re-stamp. Back-to-back, never
nested. No deadlock. The re-stamp is idempotent (re-loads fresh state), so an interleaving
concurrent `init` between the two cannot cause a lost update.

### status command
`makeslop status` is an ordered dependency health check:
1. Daemon (`CheckDaemon`) — blocking
2. Base config (`BaseConfigExists`/`MigrationStatus`) — absent/corrupt = blocking (`✗`), stale = non-blocking (`!`)
3. Image (`ImageExists`) — blocking
4. Workspace (`ws.Lookup`) — blocking
5. Secret scan summary (`security.Scan` count) — non-blocking (`–`/`✓`)
6. Proxy (`projectconfig.Load`) — non-blocking; detail is config-derived: `"direct (bridge networking)"` when `ProxyAddress` is empty, the upstream address when set (e.g. `"10.0.0.5:3128"`)
7. Socat image (`docker.ImageExists(docker.SocatImage)`) — non-blocking; `✓` when present, `!` with `"alpine/socat absent — will pull on first --proxy run"` when absent

Output: aligned lines with glyphs `✓/✗/–/!`; final verdict line + single next action. `--json`
emits `{checks:[{name,state,detail}], ready:bool}`. Exits non-zero when any blocking check fails.
CI/pipe-safe; exempt from the home guard and TTY requirement. Color/glyphs only when stderr is a
TTY and `NO_COLOR` is unset.

### --out-of-home flag scope
`--out-of-home` is registered only on `init` and `run` (not a persistent root flag). Commands
`version`, `config`, `migrate`, `build`, and `status` reject it as an unknown flag.

### --proxy flag scope
`--proxy` is registered only on `run` (not a persistent root flag). Commands `version`,
`config`, `migrate`, `build`, `init`, and `status` reject it as an unknown flag.

### --global-only flag scope
`--global-only` is registered only on `init` (not a persistent root flag). Commands `run`,
`version`, `config`, `migrate`, `build`, and `status` reject it as an unknown flag.
It only affects a **fresh** scaffold: `Scaffold` is idempotent (EEXIST = success, never clobbers
user edits), so on an already-init'd project `--global-only` is a no-op — documented, not silent.

### --quiet flag
`--quiet` is a persistent root flag. When set, stderr chrome (notices, nudges, progress lines such
as `masked N`) is suppressed. Error messages still print to stderr.

`build` is the one command whose **stdout** the flag also affects: `--quiet` threads through
`BuildOptions.Quiet` → `renderBuildOutput`, which selects `progressui.QuietMode` (discards the
`[+] Building` UI) and skips the plain-fallback stream/status writes. Build failures still
propagate (the decode loop's `decodeErr`/`msg.Error` path is unaffected), so errors remain visible.

### Network model — direct default, opt-in proxy

**Direct bridge networking is the default.** The app container has normal Docker bridge networking
and full internet access. No socat sidecar, no volume, no `--network none`.

**Proxy mode is opt-in.** Enabled by `--proxy ip:port` (flag) or `network.proxy.address` in
`.makeslop.yaml` (config). The flag wins over config. When proxy mode is active, the app is
airtight (`--network none`; its sole egress is a unix socket → socat → `TCP-CONNECT:<ip>:<port>`
→ the remote HTTP proxy).

There is **no host-side Gateway process**, no `host.docker.internal`, no probe-dial, and no
request logging. The socat sidecar connects to the remote upstream directly over bridge networking,
so proxy mode works on native Linux as well as Docker Desktop.

#### Two-state model

| State | Trigger | Behavior |
|---|---|---|
| **Direct (default)** | no `--proxy`, no `network.proxy.address` | bridge networking; no socat, no volume, no `--network none` |
| **Proxy** | `--proxy ip:port` OR `network.proxy.address` (flag wins) | `--network none`; egress = volume unix socket → socat → `TCP-CONNECT:<ip>:<port>` → remote HTTP proxy |

#### Data path (proxy mode)
```
app (--network none, HTTP_PROXY=unix:///sockets/proxy.sock)
  → read-only volume → socat sidecar (bridge)
       UNIX-LISTEN:/sockets/proxy.sock,fork,mode=0666
       TCP-CONNECT:<remote-ip>:<remote-port>,reuseaddr
         → remote HTTP proxy
```

#### Sidecar (`internal/docker/sidecar.go`)
`Sidecar` manages an `alpine/socat` container that exposes a unix socket on a Docker **volume**
and connects it to a remote upstream address.

- `const SocatImage = "alpine/socat@sha256:<digest>"` — pinned by digest; exported so `status.go`
  can check its presence via `docker.ImageExists`.
- `NewSidecar(quiet bool, stderr io.Writer) *Sidecar`
- `Start(ctx context.Context, upstream string, volumeName string) error`
  1. `ImageInspect` to check presence; if absent, `ImagePull` with a one-line notice (suppressed by `--quiet`); pull failure is fatal with a registry hint.
  2. `VolumeCreate` (per-run name, `managed-by: makeslop` label).
  3. `ContainerCreate` + `ContainerStart` — detached socat container on bridge networking, volume
     mounted **read-write** at `/sockets`. Socat args:
     `UNIX-LISTEN:/sockets/proxy.sock,fork,mode=0666 TCP-CONNECT:<upstream>,reuseaddr`.
  4. Readiness poll (~5 s, 100 ms intervals): `ContainerInspect` (early-exit detection) →
     `ExecCreate`/`ExecStart`/`ExecInspect` (`test -S /sockets/proxy.sock`, exit code 0 = ready).
- `Close() error` — `ContainerRemove(Force:true)` then `VolumeRemove`; best-effort, idempotent.

**No proactive stale-sweep** (no `VolumeList`/`ContainerList`): orphans from killed runs are
tolerated (unique per-run volume name) and prunable via `docker volume prune --filter label=managed-by=makeslop`.

#### Volume mount in `spec.go`
`Options.ProxySocketVolume string` — when non-empty, `BuildSpec` emits:
- `--network none`
- A **read-only** volume mount at `proxySocketDir` (`/sockets`) for the app container.
  (`Args()` → `type=volume,source=<name>,target=/sockets,readonly`; `mountsFor` → `mount.TypeVolume`.)
- `HTTP_PROXY=unix:///sockets/proxy.sock` and `HTTPS_PROXY=unix:///sockets/proxy.sock`.

The socat sidecar mounts the same volume **read-write** (it must create the socket file); the app
container is read-only.

#### runRun wiring
`runRun` in `cmd/makeslop/main.go` resolves the effective upstream:

```go
upstream := proxyFlag
if upstream == "" {
    upstream = netCfg.ProxyAddress
}
```

When `upstream != ""`:
1. Validates with `net.SplitHostPort`; fails loud (prints `makeslop: …` to stderr, returns `errSilent`) on malformed address.
2. Derives the per-run volume name `makeslop-sock-<hash>-<pid>`.
3. Sets `opts.ProxySocketVolume = volumeName`.
4. Constructs the Sidecar via `newSidecarFn(quiet, stderr)` and calls `sc.Start(ctx, upstream, volumeName)`.
5. Calls `docker.Run(...)`.
6. Defers teardown: `sc.Close()`.

When `upstream == ""` (direct mode): skips all proxy wiring; `opts.ProxySocketVolume` is empty.

Dry-run: the app container spec shows the read-only volume mount + `--network none` when proxy is
active, or plain bridge networking when direct. "Printed == executed" holds for the app container spec.
