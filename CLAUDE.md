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
used by `Run`, `Build`, `Ping`, and `ImageInspect`. A package-level `newClientFn` (defaulting to
`newClient`, which calls `client.New(client.FromEnv)`) constructs the live client. A compile-time
assertion `var _ apiClient = (*moby.Client)(nil)` guards against signature drift.

The interface covers (raw SDK methods from `github.com/moby/moby/client`):
`ContainerCreate`, `ContainerAttach`, `ContainerStart`, `ContainerWait`, `ContainerResize`,
`ContainerRemove`, `ImageBuild`, `DialHijack`, `Ping`, `ImageInspect`, `Close`.

`SetClientForTest(c apiClient) (restore func())` (in `testing.go`) replaces `newClientFn` for the
duration of a test. Ready-made fakes live in `testing.go`:

- **`FakeRunClient`** — simulates the `Run` container lifecycle with a scripted exit code; also
  supports `PingErr` to simulate daemon-down, `ImageMissing` to simulate absent images,
  `BlockPing` to block `Ping` until the context is cancelled (used in preflight-timeout tests),
  and `BlockImageInspect` to similarly block `ImageInspect`.
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

### Shared preflight helpers (`internal/docker/preflight.go`)

`CheckDaemon(ctx context.Context) error` — pings the daemon via `newClientFn`; returns
`ErrDaemonUnreachable` on failure.

`ImageExists(ctx context.Context, image string) (bool, error)` — calls `ImageInspect`; returns
`(true, nil)` when found, `(false, nil)` only when `cerrdefs.IsNotFound(err)`, and `(false, err)`
for any other error (so a dead daemon is never misreported as "image absent").

`WithPreflightTimeout(ctx context.Context) (context.Context, context.CancelFunc)` — wraps the
given context with a short deadline (5 s) so that preflight checks never hang on a black-hole
`DOCKER_HOST`. Both `runRun` (main.go) and `runStatus` (status.go) use it around each preflight call.

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

### Per-workspace cache mount overlays (`cache:` block in `.makeslop.yaml`)
`internal/projectconfig/projectconfig.go` parses an optional `cache:` block:

```yaml
cache:
  content: true   # mount docs/ + CLAUDE.md from per-workspace cache (default: true)
  agent: true     # mount .claude/ + .codex/ from per-workspace cache (default: true)
```

`projectconfig.Load` returns a `Cache{Content bool, Agent bool}` (second return value, before the error). Absent
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
Whenever `internal/assets/files/Dockerfile` is modified (e.g. multi-arch support), bump
`MigrationVersion` in `internal/config/config.go` so that existing installs pick up the new
Dockerfile on the next `makeslop migrate`. `CurrentVersion` (settings schema version) is separate
and only changes when the `Settings` struct fields change. The two constants serve different
purposes: `CurrentVersion` gates JSON schema compatibility; `MigrationVersion` gates the one-shot
directory refresh.

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

### status command
`makeslop status` is an ordered dependency health check:
1. Daemon (`CheckDaemon`) — blocking
2. Base config (`BaseConfigExists`/`MigrationStatus`) — absent/corrupt = blocking (`✗`), stale = non-blocking (`!`)
3. Image (`ImageExists`) — blocking
4. Workspace (`ws.Lookup`) — blocking
5. Secret scan summary (`security.Scan` count) — non-blocking (`–`/`✓`)

Output: aligned lines with glyphs `✓/✗/–/!`; final verdict line + single next action. `--json`
emits `{checks:[{name,state,detail}], ready:bool}`. Exits non-zero when any blocking check fails.
CI/pipe-safe; exempt from the home guard and TTY requirement. Color/glyphs only when stderr is a
TTY and `NO_COLOR` is unset.

### --out-of-home flag scope
`--out-of-home` is registered only on `init` and `run` (not a persistent root flag). Commands
`version`, `config`, `migrate`, `build`, and `status` reject it as an unknown flag.

### --global-only flag scope
`--global-only` is registered only on `init` (not a persistent root flag). Commands `run`,
`version`, `config`, `migrate`, `build`, and `status` reject it as an unknown flag.
It only affects a **fresh** scaffold: `Scaffold` is idempotent (EEXIST = success, never clobbers
user edits), so on an already-init'd project `--global-only` is a no-op — documented, not silent.

### --quiet flag
`--quiet` is a persistent root flag. When set, stderr chrome (notices, nudges, progress lines such
as `masked N`) is suppressed. Error messages still print to stderr.

### Network model
The app container always uses Docker bridge networking with full internet access. The socat-sidecar
egress proxy feature (--proxy flag, network.proxy.address config, Sidecar type, SocatImage,
ProxySocketVolume) was removed entirely. There is no proxy mode, no --network none, and no
sidecar lifecycle.
