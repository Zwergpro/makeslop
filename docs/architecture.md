# makeslop — Architecture and Internals

This document is for contributors and anyone who wants to understand how makeslop works under the
hood. It covers the key design patterns, module boundaries, and invariants. The authoritative
agent-facing notes live in `CLAUDE.md`; this file is a self-contained human-readable companion.

## Table of Contents

- [Package layout](#package-layout)
- [Pure/impure split](#pureimpure-split)
- [Mount groups and cache overlays](#mount-groups-and-cache-overlays)
- [apiClient seam and fake clients](#apiclient-seam-and-fake-clients)
- [cli dependency injection](#cli-dependency-injection)
- [Image resolution](#image-resolution)
- [Preflight helpers](#preflight-helpers)
- [Preflight timeouts](#preflight-timeouts)
- [Signal-cancellable root context](#signal-cancellable-root-context)
- [Settings RMW locking (config.WithLock)](#settings-rmw-locking-configwithlock)
- [Config-driven scan engine](#config-driven-scan-engine)
- [POSIX-only invariant](#posix-only-invariant)
- [Exit-code contract](#exit-code-contract)
- [Contributing / Build](#contributing--build)

---

## Package layout

- `cmd/makeslop/main.go` — ~15 lines: `var version = "dev"` (the ldflags landing pad) and
  `func main() { os.Exit(cli.Main(version, os.Args[1:])) }`.
- `internal/cli` — cobra commands (`init`, `run`, `status`, `config`, `ls`, `remove`,
  `version`). `root.go` holds `Main`, `runWithExitCode`, and the exit-code contract; `deps.go`
  holds the consumer-side docker interfaces.
- `internal/docker` — the `Docker` type wrapping the moby SDK: `spec.go` (pure), `run.go`,
  `preflight.go`, `client.go`, `docker.go`.
- `internal/config` — `~/.makeslop/settings.json`, config keys, settings locking.
- `internal/projectconfig` — parses and scaffolds the per-project `.makeslop.yaml`.
- `internal/workspace` — maps project roots to named workspaces and their cache dirs.
- `internal/security` — secret scan (`filepath.WalkDir` over basename globs).

---

## Pure/impure split

Argv assembly (`internal/docker/spec.go`) is **pure** and fully table-tested. Side-effecting SDK
calls live in `internal/docker/run.go`. Pure functions never touch the filesystem or exec
anything; filesystem checks that feed `BuildSpec` (e.g. `sandboxMountGates` in
`internal/cli/run.go`) run in the caller.

`spec.go` exposes two renderings of the same logical spec:

- `Args()` / `ShellCommand()` — argv slices used for `--dry-run` output.
- `ContainerConfig()` / `HostConfig()` — pure projections to SDK structs consumed by `Run`.

Drift-guard tests (`TestDriftGuard_*` in `spec_test.go`) keep both renderings honest. The
"printed == executed" invariant holds: what `--dry-run` prints is what `run` passes to the Docker
daemon.

---

## Mount groups and cache overlays

`BuildSpec` in `internal/docker/spec.go` emits mounts in a fixed order:

**Project root** (always, position 0):
- `<ProjectRoot>` → `/workspace/<name>`

**Global** (always present — not configurable):
- `~/.makeslop/.claude/` → `/home/user/.claude/`
- `~/.makeslop/.claude.json` → `/home/user/.claude.json`
- `~/.makeslop/.codex/` → `/home/user/.codex/`

**Sandbox policy** (gated by `Options.ProtectProjectConfig` / `Options.MaskGitHooks`):
- `<ProjectRoot>/.makeslop.yaml` → `/workspace/<name>/.makeslop.yaml` (read-only bind)
- tmpfs → `/workspace/<name>/.git/hooks`

**Agent-state cache overlay** (gated by `Options.MountAgentCache`):
- `workspaceHost/.claude/` → `/workspace/<name>/.claude/`
- `workspaceHost/.codex/` → `/workspace/<name>/.codex/`

**Content cache overlay** (gated by `Options.MountContentCache`):
- `workspaceHost/docs/` → `/workspace/<name>/docs/`
- `workspaceHost/CLAUDE.md` → `/workspace/<name>/CLAUDE.md`

**Secret masks** (last): `MaskedFiles` as `/dev/null` binds, then `MaskedDirs` as tmpfs mounts.

When a group is disabled (`false`), its mounts are **omitted** from the spec (never reordered).
Masks come after all group mounts, so a masked path under `docs/` still wins even when the content
group is disabled. One exception: when `ProtectProjectConfig` is set, a `/dev/null` mask for
`.makeslop.yaml` itself (e.g. from a broad `*.yaml` scan pattern) is filtered out, because it
would otherwise replace the read-only bind.

The sandbox-policy gates are computed by `sandboxMountGates` in `internal/cli/run.go`:
`ProtectProjectConfig` only when `.makeslop.yaml` is a regular file (a missing bind source would
fail container create), `MaskGitHooks` only when `.git` is a directory (gitfile worktrees and
submodules are skipped).

The cache booleans originate from the project `cache:` block in `.makeslop.yaml`, resolved by
`projectconfig.Load`. Absent block ⇒ both `true`. The `init --global-only` flag scaffolds the YAML
with both groups set to `false`.

**`Options.MountContentCache`** and **`Options.MountAgentCache`** both default to `false` in Go's
zero-value; callers that want the full-mount behavior must explicitly set them to `true`. `runRun`
(`internal/cli/run.go`) does this by reading the project config; tests that exercise full-mount
behavior must set them on their `sampleOptions()` or equivalent fixture.

---

## apiClient seam and fake clients

`internal/docker/client.go` declares a narrow unexported `apiClient` interface covering the SDK
methods used by `Run`, `CheckDaemon`, and `ImageExists`: `ContainerCreate`, `ContainerAttach`,
`ContainerStart`, `ContainerWait`, `ContainerResize`, `ContainerRemove`, `Ping`, `ImageInspect`,
`Close`. A compile-time assertion `var _ apiClient = (*moby.Client)(nil)` guards against signature
drift. Adding an SDK call means extending `apiClient` and the fakes.

`internal/docker` uses constructor dependency injection. `docker.New(opts ...Option)` builds a
real moby client from the environment (`moby.New(moby.FromEnv)`; the connection is lazy). Options:

- `WithClient(c apiClient)` — inject a fake (same-package `_test.go` only, since `apiClient` is
  unexported); suppresses the real client construction.
- `WithTTYCheck(fn)` — override the stdin+stdout TTY predicate used by `Run`.
- `WithRawMode(fn)` — override `term.MakeRaw`.
- `WithStreams(in, out)` — redirect container stdin/stdout data copies.

There is no package-level client factory variable and no test-helper code in the production
binary. Test fakes live in `_test.go` files:

- **`noopClient`** (`internal/docker/fakes_test.go`) — every method succeeds; embed it to
  override only what a test needs.
- **`fakeRunClient`** (`internal/docker/fakes_test.go`) — simulates the preflight/`Run` lifecycle
  with a scripted exit code. Supports `PingErr`, `ImageMissing`, `ImageErr`,
  `ContainerCreateErr`, `ContainerStartErr`, `BlockPing`, and `BlockImageInspect` fields.
- **`fakeClient`** (`internal/docker/run_test.go`) — the `Run`-lifecycle fake used by
  `run_test.go`; has `attachPayload` to script container output.

There are no shell shims, no `dockerBinary` global, no `executableTempDir`.

---

## cli dependency injection

Commands in `internal/cli` depend on consumer-side interfaces declared in `internal/cli/deps.go`,
not on `*docker.Docker` directly:

```go
type containerRunner interface { Run(ctx context.Context, s docker.Spec) error }
type daemonChecker   interface { CheckDaemon(ctx context.Context) error }
type imageChecker    interface { ImageExists(ctx context.Context, image string) (bool, error) }
```

`dockerDeps` bundles one of each. `newRootCmd` (in `root.go`) calls `docker.New()` once and uses
the same `*docker.Docker` for all three fields, closing it after the command finishes. If
`docker.New()` fails, every field is a `dockerNewErrStub` that returns the construction error, so
non-docker commands (`init`, `config`, `ls`, `remove`, `version`) still work while docker commands
fail clearly.

Tests build the command tree with `newRootCmdWithDeps(baseDir, deps)` and a `fakeDocker`
(`internal/cli/main_test.go`) that satisfies all three interfaces; helpers `runCmd` /
`runCmdWithDeps` execute it and capture stdout/stderr.

---

## Image resolution

makeslop does not build or pull images, and `image` in `settings.json` has no default
(`config.Load` leaves an empty value empty). `run` and `status` pick the image with a single
resolver in the cli package (`internal/cli/image.go`):

```go
func resolveImage(flagVal, settingsImage string) (string, error)
```

Order: the trimmed `-i/--image` flag, then the trimmed settings image, otherwise `errNoImage`
(`no image configured — run 'makeslop config set image <ref>' or pass -i/--image`). It takes
strings rather than `*config.Settings`, so callers need no nil-settings special case.

- `run` resolves right after loading settings, before workspace lookup and any daemon call, so a
  missing image fails fast (also on `--dry-run`). If the resolved image is absent locally, the
  image-exists preflight fails with `image "X" not found locally — build or pull it (e.g. 'docker pull X')`.
- `status` reports the image check in this order: an explicit `-i` skips the settings steps;
  corrupt settings → `cannot check — settings unreadable`; `errNoImage` → `no image configured`;
  daemon down → `cannot check — daemon unreachable`; otherwise inspect the image.
- `init` prints a non-blocking `note: no image configured …` when the setting is empty.

`examples/claudebox/Dockerfile` is an unembedded starting point that users build themselves.

---

## Preflight helpers

`internal/docker/preflight.go` provides two methods on `*Docker`, used by both `run` and `status`:

- **`CheckDaemon(ctx context.Context) error`** — pings the daemon via the shared `d.client`;
  returns `*ErrDaemonUnreachable` on failure.
- **`ImageExists(ctx context.Context, image string) (bool, error)`** — calls `ImageInspect` on
  `d.client`; returns `(true, nil)` when found, `(false, nil)` only when
  `cerrdefs.IsNotFound(err)`, and `(false, err)` for any other error (so a dead daemon is never
  misreported as "image absent").

Both methods share the `*Docker`'s single long-lived client — no per-call client construction or
close. The client is closed once by the cleanup func returned from `newRootCmd`.

---

## Preflight timeouts

Daemon-ping and image-inspect calls are bounded by `const preflightTimeout = 10 * time.Second`
(declared in `internal/docker/preflight.go`). `WithPreflightTimeout(parent context.Context)`
returns a derived context with this deadline and a cancel func.

Commands never call the checkers directly; they go through two `dockerDeps` wrappers in
`internal/cli/deps.go`, which own the deadline:

```go
func (d dockerDeps) checkDaemonPreflight(ctx context.Context) error {
	pfCtx, pfCancel := docker.WithPreflightTimeout(ctx)
	defer pfCancel()
	return d.daemon.CheckDaemon(pfCtx)
}
```

`imageExistsPreflight` has the same shape. Because each wrapper returns as soon as its single
blocking call completes, the deferred cancel releases the deadline immediately. `runRun`
(`internal/cli/run.go`) and `runStatus` (`internal/cli/status.go`) use these wrappers. The
long-running `Run` receives the original (signal-cancellable) `cmd.Context()` with no artificial
deadline.

---

## Signal-cancellable root context

`runWithExitCode` (in `internal/cli/root.go`) creates a signal-cancellable context and executes
the command tree with it:

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()
...
return exitCodeFromError(cmd.ExecuteContext(ctx), stderr)
```

Every subcommand receives this context via `cmd.Context()`. Ctrl-C or SIGTERM cancels the context,
which propagates to Docker SDK calls, preflight checks, and the secret scan.

`cli.Main` resolves `~/.makeslop` (through `filepath.EvalSymlinks` when it exists) and calls
`runWithExitCode`; `main()` just calls `os.Exit(cli.Main(version, os.Args[1:]))`.

---

## Settings RMW locking (config.WithLock)

`config.WithLock(baseDir string, fn func() error) error` (in `internal/config/lock.go`) prevents
lost updates when multiple `makeslop` processes write `settings.json` concurrently.

It uses a two-level lock:
- **Intra-process:** a package-level `sync.Mutex` (protects goroutines in the same process —
  necessary because Linux `flock(2)` does not serialize separate file descriptors within the same
  process).
- **Cross-process:** `syscall.Flock(fd, LOCK_EX)` on `<baseDir>/.settings.lock` (serializes
  distinct `makeslop` invocations).

`config.Update(baseDir, mutate)` wraps the common Load→mutate→Save sequence in `WithLock`; when
`mutate` returns an error, the save is skipped. `Save` itself writes via temp file + rename, so a
crash mid-write never leaves a half-written `settings.json`.

**No-nesting invariant:** `WithLock` MUST NOT be nested on the same goroutine — including inside
an `Update` mutate func — because same-process re-entry self-deadlocks on the `sync.Mutex`. Each
Load→mutate→Save site takes its own short-lived sequential lock; no caller wraps another
`WithLock`-protected call.

**Lock file:** `<baseDir>/.settings.lock` is created on first use and never deleted. It carries no
data — its role is purely advisory.

**Callers:**
- `workspace.Init` — registers a new workspace.
- `workspace.Remove` — unregisters a workspace.
- `config set` RunE (via `config.Update`) — writes a config key.

---

## Config-driven scan engine

`internal/security.Scan` uses a native Go `filepath.WalkDir` walk — there is no `fd`/`fdfind`
dependency. Patterns (basename globs) and skip-dirs are passed in at call time; the engine has no
hardcoded defaults. If `patterns` is empty, `Scan` returns `(nil, nil, nil)` immediately (no walk).
Symlinks whose basename matches a pattern are returned in the second slice (`symlinkMatches`)
rather than the first — WalkDir does not follow symlinks, so they are not masked; `run` warns the
user. These symlink warnings bypass `--quiet` (degraded protection is never treated as cosmetic).

Walk errors (e.g. unreadable subdirectories) are propagated immediately and abort `runRun` before
the container starts. This "fail-loud" invariant ensures makeslop never silently skips a directory
it cannot prove is secret-free — consistent with the no-`.env`-leak contract.

The defaults live as active values in the `Scaffold` stub seeded by `makeslop init`. Pre-existing
project `.makeslop.yaml` files are never auto-migrated; users with an old stub must manually add an
`exclude.scan` block.

`.makeslop.yaml` itself is decoded in strict mode (`KnownFields(true)`), so unknown keys — including
the `network:` block from older versions — are hard errors.

---

## POSIX-only invariant

makeslop targets POSIX systems only. Tests that rely on TTY/signal behavior call a
`skipNonPOSIX(t, why)` helper defined locally in each test package (unexported, not shared across
packages). Do not add Windows compatibility paths.

---

## Exit-code contract

`docker.ExitError{Code int}` (in `run.go`) is the only exit-code error. `Run` returns it when
`ContainerWait` reports a non-zero `StatusCode`. `exitCodeFromError` in `internal/cli/root.go`
maps the command's error to the process exit code:

```go
var de *docker.ExitError
if errors.As(err, &de) {
    return de.Code
}
if !errors.Is(err, errSilent) {
    fmt.Fprintf(stderr, "makeslop: %v\n", err)
}
return 1
```

`errSilent` means the command already printed a tailored message: exit 1 without reprinting. Any
other error is printed as `makeslop: <err>` and exits 1.

Signal-killed containers (e.g. SIGKILL) are reported by the daemon as `StatusCode=137`; that value
is passed through verbatim. There is no OS `WaitStatus` / `exec.ExitError` handling — makeslop
does not fork the docker binary.

---

## Contributing / Build

```
go build ./cmd/makeslop
go test -timeout=100s ./...
golangci-lint run
```

Tests do not use shell shims or a live Docker daemon, so there is no `noexec`/`GOTMPDIR`
constraint.

The version string is stamped at build time (`make build` does this and installs to
`~/.local/bin`):

```
go build -ldflags "-X main.version=$(git describe --tags --always --dirty)" ./cmd/makeslop
```

A plain `go build` without ldflags prints `dev` for the version.
