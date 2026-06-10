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

### docker package: struct-DI via `Docker` and `New`
`internal/docker` uses constructor dependency injection. There are no package-level mutable globals
and no test helpers in the production binary.

**`Docker` struct and construction:**
```go
type Docker struct { /* client, isTTYFn, makeRaw, stdin, stdout — all unexported */ }

func New(opts ...Option) (*Docker, error)          // real defaults from environment
func WithClient(c apiClient) Option                // same-package _test.go only
func WithTTYCheck(fn func() bool) Option
func WithRawMode(fn func(int) (*term.State, error)) Option
func WithStreams(in io.Reader, out io.Writer) Option // redirect container I/O (tests/CI)
```
`New` builds a moby client via `moby.New(moby.FromEnv)`, sets real stdin+stdout TTY detection,
sets real `term.MakeRaw`, and sets `stdin = os.Stdin` / `stdout = os.Stdout`. Options override
the defaults.

`WithStreams` redirects the data copies (`io.Copy` between container attach and host I/O) to
injected streams. Fd-based terminal operations (raw mode, `term.GetSize`, SIGWINCH resize) always
use the real `os.Stdin`; only the byte-stream copies are redirected. Used by tests via `bytes.Buffer`
and pipe ends to assert output content without a PTY.

**`apiClient` interface** (`client.go`) is the narrow unexported 11-method moby SDK adapter:
`ContainerCreate`, `ContainerAttach`, `ContainerStart`, `ContainerWait`, `ContainerResize`,
`ContainerRemove`, `ImageBuild`, `DialHijack`, `Ping`, `ImageInspect`, `Close`.
A compile-time assertion `var _ apiClient = (*moby.Client)(nil)` guards against signature drift.
The interface is not exported — `WithClient` is the only gateway, and only same-package
`_test.go` files call it.

**Methods on `*Docker`:** `Run`, `Build`, `CheckDaemon`, `ImageExists`, `Close`.
All four operation methods share a single `apiClient` for the struct's lifetime.
`cmd` callers must `defer d.Close()` once after construction to release the client connection.

**`Run` lifecycle (fixed in Tasks 1–2):**
```
Create → Attach → raw mode → ContainerWait(next-exit) → ContainerStart →
  stdout goroutine + stdin goroutine →
  on wait result: drain stdout (select outputDone/ctx.Done) →
  close stdin handle → join stdin goroutine (select stdinDone/ctx.Done) →
  map StatusCode → ExitError
```
Registering `ContainerWait` before `ContainerStart` guarantees the daemon delivers the exit status
even when the container auto-removes within milliseconds (fixes finding #2). The `outputDone`
channel prevents tail-output truncation (fixes finding #1). The inline stdin-goroutine join (not a
defer) ensures the goroutine finishes before the deferred `att.Conn.Close` fires (fixes finding #3).

Stdin join uses a pollable dup of fd 0 (`newPollableStdin`): `SetNonblock(0, true)` + `Dup` +
`os.NewFile` → Go runtime poller manages the fd → `Close()` unblocks the pending `Read`. Restore
(`SetNonblock(0, false)`) runs in the same deferred cleanup as `term.Restore`. Injected test
readers implementing `io.Closer` (e.g. `os.Pipe` read-end) take the same join path. If
dup/fcntl fails, the goroutine is leaked — this is the documented fallback, matching what the
docker CLI does.

**Test fakes** live in two files (both `_test.go` — never compiled into the production binary):

`internal/docker/fakes_test.go` — preflight and build fakes (package `docker`):
- `fakeRunClient` / `newFakeRunClient(exitCode)` — scripts preflight lifecycle; fields
  `PingErr`, `ImageMissing`, `BlockPing`, `BlockImageInspect` cover error paths. Used by
  `preflight_test.go` and related tests. (CLAUDE.md previously used capitalized `FakeRunClient`;
  actual code uses unexported `fakeRunClient`.)
- `fakeBuildClient` / `newFakeBuildClient(exitCode)` — scripts `Build`; records
  the last build options in its unexported `lastBuildOptions` field.
- `newDockerWithClient(t, c, opts...)` — constructs a `*Docker` with a fake client via
  `WithClient`; registers `d.Close` via `t.Cleanup`.
- `noopMakeRaw`, `alwaysTTY`, `neverTTY` — stubs for option injection.

`internal/docker/run_test.go` — Run-lifecycle fake (package `docker`):
- `fakeClient` — the Run-lifecycle fake used by `run_test.go`; distinct from `fakeRunClient`.
  Has an `attachPayload` field to script delayed attach output, records call order (for the
  wait-before-start test), and scripts attach reader EOF after scripted output delivery.
- `noopMakeRaw`, `alwaysTTY`, `neverTTY` — defined in `fakes_test.go` (same package `docker`) and used by `run_test.go`.

**Consumer-side interfaces in `cmd/makeslop`** (package `main`):
```go
type containerRunner interface { Run(ctx context.Context, s docker.Spec) error }
type imageBuilder    interface { Build(ctx context.Context, o docker.BuildOptions, out, errw io.Writer) error }
type daemonChecker   interface { CheckDaemon(ctx context.Context) error }
type imageChecker    interface { ImageExists(ctx context.Context, image string) (bool, error) }
```
These are bundled in `dockerDeps`. `newRootCmd` constructs a `*docker.Docker`, calls `defer closeDocker()` via
the returned cleanup func, and wraps the instance in `dockerDeps`; `newRootCmdWithDeps` accepts injected deps
for tests (boundary fakes in `main_test.go`).

`dockerNewErrStub` (in `main.go`) is a fallback used when `docker.New()` fails: it implements all four
interfaces and returns the construction error from every method call, so non-docker commands still work while
docker-touching commands get a clear error instead of a panic.

`runWithExitCode` accepts an optional `contextObserver func(context.Context)` parameter (nil in production)
that is called with the signal-cancellable context immediately after it is created. Tests pass a non-nil
observer to verify that `ExecuteContext` receives a cancellable (non-Background) context.

There are no shell shims, no `dockerBinary` global, no `executableTempDir`, no
`SetClientForTest`/`SetTTYCheckForTest`/`SetTermMakeRawForTest` globals.

**Note:** `CurrentVersion` and `MigrationVersion` are NOT bumped for this struct-DI refactor —
no `Settings` struct fields changed and the embedded Dockerfile is unchanged.

### Preflight methods (`internal/docker/preflight.go`)

`(*Docker).CheckDaemon(ctx context.Context) error` — pings the daemon via the shared `d.client`;
returns `*ErrDaemonUnreachable` on failure.

`(*Docker).ImageExists(ctx context.Context, image string) (bool, error)` — calls `ImageInspect`
on `d.client`; returns `(true, nil)` when found, `(false, nil)` only when
`cerrdefs.IsNotFound(err)`, and `(false, err)` for any other error (so a dead daemon is never
misreported as "image absent").

`WithPreflightTimeout(ctx context.Context) (context.Context, context.CancelFunc)` — wraps the
given context with a `preflightTimeout` deadline (10 s) so that preflight checks never hang on a
black-hole `DOCKER_HOST`. Both `runRun` (main.go) and `runStatus` (status.go) use it around each
preflight call.

Both methods share the `*Docker`'s single long-lived client — no per-call client construction
or close. `cmd` callers must `defer d.Close()` once after construction to release the connection.

**`runRun` preflight execution order:**
```
pwd → home guard → config.Load → ws.Lookup →
  [if !dryRun] CheckDaemon (with WithPreflightTimeout) →  ← skipped on --dry-run
  projectconfig.Load → security.Scan → BuildSpec →
  [if dryRun] print + return →
  ImageExists (with WithPreflightTimeout) → Run
```
The daemon check is intentionally before the secret scan so a down daemon is reported
immediately without waiting through a potentially long walk. It is skipped on `--dry-run`
so `--dry-run` continues to work without a live daemon.

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
hardcoded defaults. If `patterns` is empty, `Scan` returns `(nil, nil, nil)` immediately (no walk).

**`Scan` signature**: `Scan(ctx, root, patterns, skipDirs) (paths, symlinkMatches []string, err error)`.
Regular files matching a pattern go into `paths`; symlinks matching a pattern go into `symlinkMatches`
(not masked — WalkDir doesn't follow symlinks). Both slices are sorted. Callers: `runRun` (prints
symlink warnings bypassing `--quiet`) and `runStatus` in `status.go` (ignores the slice).

Walk errors (e.g. unreadable subdirectory) are **propagated immediately** and abort `runRun` before
`docker.Run`. This "fail-loud" invariant ensures we never silently skip a directory we cannot prove
is secret-free — consistent with the no-`.env`-leak contract for **unskipped** paths.

**Trust assumption for skip-dirs:** directories listed in `exclude.scan.skip-dirs` are bind-mounted
into the container unscanned. The no-`.env`-leak guarantee applies only to walked (non-skipped)
paths. Secrets inside `.git/`, `node_modules/`, etc. are the user's responsibility. Shrinking
skip-dirs widens the guarantee at the cost of a longer pre-launch walk.

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

`projectconfig.Load` returns `(Excludes, Cache, []string, error)` — a 4-value return. The `Cache`
value is the second return value (before the env slice and error). Absent block ⇒ `{true, true}` ⇒
identical to pre-feature behavior (backward compatible).

`Excludes` carries a `Warnings []string` field: human-readable notices for symlinked entries in
`exclude.files` or `exclude.dirs` that are dropped from masking. The warning message is
`"path %q is a symlink and is NOT masked"`. Missing entries and non-symlink wrong-type drops (e.g. a
directory listed in `exclude.files`) stay silent. `runRun` prints these warnings to `cmd.ErrOrStderr()`
directly — NOT through `quietWriter` — so `--quiet` does not suppress them.

The values flow into `docker.Options.MountContentCache` and `docker.Options.MountAgentCache` in
`runRun`. `BuildSpec` gates the two per-workspace mount groups on these booleans; global mounts
(`~/.makeslop/.claude/`, `.claude.json`, `.codex/`) are always present.

`Scaffold(root, Cache)` writes the stub with the `cache:` block; `Stub` is the default rendering
(`{true, true}`). The `init --global-only` flag calls `Scaffold(root, Cache{false, false})`,
scaffolding a file that disables both overlay groups. `Scaffold` is idempotent (EEXIST = success).

### Static environment variables (`environments:` block in `.makeslop.yaml`)
`internal/projectconfig/projectconfig.go` also parses an optional `environments:` block:

```yaml
environments:
  NODE_ENV: production
  PORT: 8080
```

`projectconfig.Load` returns `(Excludes, Cache, []string, error)`. The `[]string` is the third
return value: a sorted slice of `"KEY=VALUE"` pairs from the `environments:` block, or `nil` when
the block is absent (backward-compatible — nil means no `-e` flags emitted).

`validateEnvironments(map[string]yaml.Node) ([]string, error)` validates the block:
- Empty keys rejected.
- Values must be `yaml.ScalarNode` (strings, numbers, booleans coerced to string).
- Null scalars (`KEY:` / `KEY: null`) rejected fail-loud.
- Explicit empty string (`KEY: ""`) accepted → `"KEY="`.
- Keys must not contain `=` or newline/tab characters.
- Returns a `sort.Strings`-sorted `[]string` for deterministic output.

The env slice flows into `docker.Options.Env []string` in `runRun`, then into `Spec.Env` via
`BuildSpec`, then:
- `Args()` emits `-e KEY=VALUE` per entry, after the `--security-opt` loop and before the mounts loop.
- `ContainerConfig()` sets `Env: s.Env`.
- `ShellCommand()` already handles `-e` (value-taking flag already listed in its switch).

`MigrationVersion` and `CurrentVersion` are NOT bumped — `environments:` lives in per-project
`.makeslop.yaml`, not in `~/.makeslop/settings.json` or the embedded Dockerfile.

### POSIX-only invariant
makeslop targets POSIX systems only. Tests that rely on TTY/signal behavior call an inline `skipNonPOSIX`
helper defined locally in each test package (no shared export). Do not add Windows compatibility paths.

### TTY requirement is `run`-only
`makeslop run` requires an interactive TTY (checked via `(*Docker).Run`'s `isTTYFn` predicate,
injected at construction time via `WithTTYCheck`; defaults to real stdin+stdout detection).
`makeslop build`, `makeslop init`, `makeslop migrate`, `makeslop config`, `makeslop status`, and `makeslop version` are CI/pipe-safe and never consult the TTY predicate.

**Two distinct TTY notions** — do not conflate:
- `docker.Docker`'s `isTTYFn` checks stdin+stdout and gates `Run` (returns `ErrNoTTY` when false).
- `cmd`'s `isTTYFunc` / `defaultIsTTY` is writer-based and gates status color/glyph output
  (`status.go`, `main.go`). These stay separate.

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

**Current values:** `MigrationVersion = 3` (bumped for Dockerfile hardening: sudo removal, base
digest pin, Go/Node tarball sha256 verification, zsh-in-docker checksum). `CurrentVersion = 1`.

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

Symlink warnings (from `security.Scan` and `Excludes.Warnings`) bypass `--quiet` — degraded
protection is never treated as cosmetic chrome.

### Sandbox-policy mounts (`ProtectProjectConfig`, `MaskGitHooks`)
Two new `docker.Options` booleans gate additional security mounts in `BuildSpec`:

- `ProtectProjectConfig bool` — when true, mounts `<ProjectRoot>/.makeslop.yaml` read-only over
  itself inside the container (prevents the agent from weakening its own sandbox policy). Set in
  `runRun` iff `Lstat(<ProjectRoot>/.makeslop.yaml)` reports a regular file (missing file → skip;
  a missing bind source fails container create and there is nothing to protect).
- `MaskGitHooks bool` — when true, overlays `<workspacePath>/.git/hooks` with an empty tmpfs
  (prevents the agent from planting hooks that execute on the host). Set in `runRun` iff
  `Lstat(<ProjectRoot>/.git)` reports a directory (gitfile/worktree/submodule → leave off; the
  real hooks dir is outside the workspace; documented residual).

Both mounts are inserted in `BuildSpec` at a fixed point: after the 4 base mounts and before the
cache overlays. The drift-guard test covers both. `--dry-run` reflects both automatically.

**`filterOut` interaction for `ProtectProjectConfig`:** when `ProtectProjectConfig` is true,
`runRun` calls `filterOut(maskedFiles, configPath)` to remove `.makeslop.yaml` from the
`/dev/null` masked-file list *before* building `opts`. This is necessary because Docker applies
mounts in argument order (last-write-wins): if a `/dev/null` bind for `.makeslop.yaml` appears
after the read-only self-bind, it silently overrides the protection (e.g. when the user adds a
broad pattern like `"*.yaml"` to `exclude.scan.patterns`). `filterOut` removes only the first
occurrence and returns the input unmodified when the path is absent.

### Decoupling: `workspace.Lookup` signature and `docker.Options.WorkspaceHost`

`workspace.Lookup` does **not** call `config.Load` internally. Its signature is:

```go
func (w *Workspaces) Lookup(s *config.Settings, pwd string) (matchedRoot, cacheDir string, err error)
```

The caller is responsible for loading settings once and passing the result. A nil `s` is treated
as empty settings (returns `ErrNotRegistered`). This means:
- `runRun` loads settings once near the top of the function and passes the same `*config.Settings`
  to both `ws.Lookup` and any subsequent settings-dependent logic (no double-load).
- `status.go` reuses `loadedSettings` from check 2 for the workspace check. When settings are
  absent, `loadedSettings` is nil → `ErrNotRegistered` → detail "not registered — run 'makeslop
  init'". When settings are corrupt (parse failed), `loadedSettings` is also nil → same
  `ErrNotRegistered` path → detail changed to `"cannot check — settings unreadable"` (the real
  cause was already surfaced by the base-config check; a misleading "run makeslop init" would be
  wrong). This behavioral distinction was introduced in Task 3.

`docker.Options.WorkspaceHost string` replaces the old pattern of computing the workspace host path
inside `spec.go` from `BaseDir + config.WorkspacesDir + WorkspaceName`. The caller (`runRun`)
computes `workspaceHost` from the `cacheDir` returned by `ws.Lookup` and sets `Options.WorkspaceHost`
directly. This removes the `internal/config` import from `spec.go` (keeping it pure).

`--network` was removed from `ShellCommand`'s flag switch — `Args()` no longer emits it.

### Reserved paths in projectconfig
`reservedPaths` in `internal/projectconfig/projectconfig.go` lists paths that `docker.BuildSpec`
may mount over the project root. User overlays in `exclude.dirs`/`exclude.files` that collide with
these paths are rejected with a hard error:

```
projectconfig: path %q collides with a reserved agent path
```

Reserved paths: `.claude`, `.codex`, `docs`, `CLAUDE.md`, `.makeslop.yaml`.

### Dockerfile hardening: "pin infra, float agents" convention
The embedded `internal/assets/files/Dockerfile` follows a deliberate split:

- **Pinned with sha256:** base image digest (`debian:trixie-slim@sha256:…`), Go tarball (per-arch
  sha256 in the `RUN` command, not an overridable `ARG`), Node.js tarball (per-arch sha256 in the
  `RUN` command), zsh-in-docker script (version `v1.2.1` hardcoded in the URL — no `ARG`, so
  `--build-arg` cannot substitute an attacker-controlled version or bypass the sha256 check).
- **Intentionally floating:** `claude.ai/install.sh` and `@openai/codex` — these are active agent
  code; users benefit from the latest version on each build. Pinning agent versions is an accepted
  residual risk.

**Maintenance rule:** whenever `GO_VERSION` or `NODE_VERSION` is bumped, update the per-arch
sha256 values in the corresponding `RUN` commands AND bump `MigrationVersion` in
`internal/config/config.go` so existing installs pick up the new Dockerfile via
`makeslop migrate` + `makeslop build`. `CurrentVersion` is NOT bumped (no `Settings` struct change).

### Network model
The app container always uses Docker bridge networking with full internet access. The socat-sidecar
egress proxy feature (--proxy flag, network.proxy.address config, Sidecar type, SocatImage,
ProxySocketVolume) was removed entirely. There is no proxy mode, no --network none, and no
sidecar lifecycle.
