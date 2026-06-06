# makeslop — Architecture and Internals

This document is for contributors and anyone who wants to understand how makeslop works under the
hood. It covers the key design patterns, module boundaries, and invariants. The authoritative
agent-facing notes live in `CLAUDE.md`; this file is a self-contained human-readable companion.

## Table of Contents

- [Pure/impure split](#pureimpure-split)
- [Mount groups and cache overlays](#mount-groups-and-cache-overlays)
- [apiClient seam and fake clients](#apiclient-seam-and-fake-clients)
- [newSidecarFn seam](#newsidecarfn-seam)
- [Socat sidecar lifecycle](#socat-sidecar-lifecycle)
- [BuildKit session build flow](#buildkit-session-build-flow)
- [Preflight helpers](#preflight-helpers)
- [Config-driven scan engine](#config-driven-scan-engine)
- [Version constants](#version-constants)
- [POSIX-only invariant](#posix-only-invariant)
- [Exit-code contract](#exit-code-contract)
- [Contributing / Build](#contributing--build)

---

## Pure/impure split

Argv assembly (`internal/docker/spec.go`) is **pure** and fully table-tested. Side-effecting SDK
calls live in `internal/docker/run.go` and `internal/docker/build.go`. Pure functions never touch
the filesystem or exec anything.

`spec.go` exposes two renderings of the same logical spec:

- `Args()` / `ShellCommand()` — argv slices used for `--dry-run` output.
- `ContainerConfig()` / `HostConfig()` — pure projections to SDK structs consumed by `Run`.

A drift-guard test keeps both renderings honest. The "printed == executed" invariant holds: what
`--dry-run` prints is what `run` passes to the Docker daemon.

---

## Mount groups and cache overlays

`BuildSpec` in `internal/docker/spec.go` organises the 8 mounts into three logical groups:

**Global** (always present — not configurable):
- `~/.makeslop/.claude/` → `/home/user/.claude/`
- `~/.makeslop/.claude.json` → `/home/user/.claude.json`
- `~/.makeslop/.codex/` → `/home/user/.codex/`

**Agent-state cache overlay** (gated by `Options.MountAgentCache`):
- `workspaceHost/.claude/` → `/workspace/<name>/.claude/`
- `workspaceHost/.codex/` → `/workspace/<name>/.codex/`

**Content cache overlay** (gated by `Options.MountContentCache`):
- `workspaceHost/docs/` → `/workspace/<name>/docs/`
- `workspaceHost/CLAUDE.md` → `/workspace/<name>/CLAUDE.md`

When a group is disabled (`false`), its mounts are **omitted** from the spec (never reordered).
The project source root is always mounted at position 0. Secret masking (masked files `/dev/null`,
masked dirs tmpfs) appends after all group mounts, so a masked path under `docs/` still wins even
when the content group is disabled.

The two booleans originate from the project `cache:` block in `.makeslop.yaml`, resolved by
`projectconfig.Load`. Absent block ⇒ both `true` ⇒ identical to pre-feature behavior. The
`init --global-only` flag scaffolds the YAML with both groups set to `false`.

**`Options.MountContentCache`** and **`Options.MountAgentCache`** both default to `false` in Go's
zero-value; callers that want the traditional full-mount behavior must explicitly set them to
`true`. `runRun` does this by reading the project config; tests that exercise full-mount behavior
must set them on their `sampleOptions()` or equivalent fixture.

---

## apiClient seam and fake clients

`internal/docker/client.go` declares a narrow unexported `apiClient` interface covering all SDK
methods used by `Run`, `Build`, `Ping`, `ImageInspect`, and the socat sidecar lifecycle. A
package-level `newClientFn` (defaulting to `newClient`, which calls `client.New(client.FromEnv)`)
constructs the live client. A compile-time assertion `var _ apiClient = (*moby.Client)(nil)` guards
against signature drift.

`SetClientForTest(c apiClient) (restore func())` (in `internal/docker/testing.go`) replaces
`newClientFn` for the duration of a test.

**Note on testing.go in the production binary:** `internal/docker/testing.go` is compiled into the
production binary — it is not a `_test.go` file. This is intentional: `cmd/makeslop/main_test.go`
is in `package main`, not `package docker_test`, so it cannot reach unexported symbols via an
`export_test.go` bridge. Shipping the test helpers into the production binary is the accepted
trade-off for testability; the binary size impact is negligible.

Ready-made fakes live in `testing.go`:

- **`FakeRunClient`** — simulates the `Run` container lifecycle with a scripted exit code. Supports
  `PingErr` (daemon-down), `ImageMissing` (absent image), `SidecarExited`, and `SocatImageMissing`
  toggles. Records created/removed volumes and models the create→start→inspect exec handshake.

  ```go
  t.Cleanup(docker.SetClientForTest(docker.NewFakeRunClient(0))) // exit 0
  ```

- **`FakeBuildClient`** — simulates the `Build` SDK call and records `ImageBuildOptions`. Also
  supports `PingErr` and `ImageMissing` fields.

  ```go
  fbc := docker.NewFakeBuildClient(0)
  t.Cleanup(docker.SetClientForTest(fbc))
  // ... call Build ...
  opts := fbc.LastBuildOptions
  ```

This replaces the old shell-shim machinery. There are no shell shims, no `dockerBinary` global, no
`executableTempDir`.

---

## newSidecarFn seam

`cmd/makeslop/main.go` has a package-level seam that mirrors the `docker.newClientFn` pattern:

**`newSidecarFn`** defaults to a closure that calls `docker.NewSidecar(quiet, stderr)` and returns
a `sidecarRunner`. Tests swap it via `setSidecarFnForTest(t)`, which returns a `fakeSidecar` that
records `(upstream, volumeName)` passed to `Start`. To simulate a `Start` failure, set
`cap.startErr` before calling `runCmd`.

```go
cap := setSidecarFnForTest(t)
// ... run command ...
// cap.upstream, cap.volumeName, cap.called are available
```

The `sidecarRunner` interface has `Start(ctx context.Context, upstream string, volumeName string) error`
and `Close() error`, satisfied by `*docker.Sidecar`.

The seam is used in proxy-wiring tests where a real sidecar container would be unnecessary or would
race with test teardown.

---

## Socat sidecar lifecycle

`internal/docker/sidecar.go` manages an `alpine/socat` container that exposes a unix socket on a
Docker volume and connects it to a remote upstream address. This is used in proxy mode to give
`--network none` app containers a controlled egress path.

`Sidecar.Start(ctx, upstream, volumeName)`:

1. `ImageInspect` to check presence; if absent, `ImagePull` with a one-line notice (suppressed by
   `--quiet`); pull failure is fatal with a registry hint.
2. `VolumeCreate` (per-run name, `managed-by: makeslop` label).
3. `ContainerCreate` — detached socat container on bridge networking, volume mounted **read-write**
   at `/sockets`. Socat args:
   `UNIX-LISTEN:/sockets/proxy.sock,fork,mode=0666 TCP-CONNECT:<upstream>,reuseaddr`.
4. `ContainerStart` — starts the created container.
5. Readiness poll (~5 s, 100 ms intervals): `ContainerInspect` (early-exit detection) →
   `ExecCreate` / `ExecStart` / `ExecInspect` (`test -S /sockets/proxy.sock`, exit 0 = ready).

`Sidecar.Close()` — `ContainerRemove(Force:true)` then `VolumeRemove`; best-effort, idempotent.

**Orphan containers:** there is no proactive stale-sweep (`VolumeList`/`ContainerList`). Orphans
from killed runs are tolerated (unique per-run volume names) and prunable with:

```
docker volume prune --filter label=managed-by=makeslop
```

The socat image is pinned by digest (`const SocatImage = "alpine/socat@sha256:..."`); `status`
checks its presence via `docker.ImageExists(docker.SocatImage)`.

---

## BuildKit session build flow

`internal/docker/build.go` implements `Build` via the moby/moby SDK + a BuildKit session:

1. A `session.Session` is created, allowing `filesync` (context dir + dockerfile dir) and
   `authprovider.NewDockerAuthProvider` for registry pulls.
2. A dialer adapter wraps `cli.DialHijack(ctx, "/session", proto, meta)` to match
   `session.Dialer`'s `func(ctx, proto, meta)` signature.
3. `s.Run(ctx, dialer)` is started in a goroutine.
4. `ImageBuild` is called with `Version: build.BuilderBuildKit` (the `"2"` selector) and the
   session ID. No `DOCKER_BUILDKIT` environment variable is needed.
5. The response body is decoded as a BuildKit JSON trace stream; `aux` frames carrying
   `moby.buildkit.trace` payloads are decoded into `*client.SolveStatus` and fed to
   `progressui.NewDisplay(...).UpdateFrom` for rendering.
6. The session and client are closed when the build finishes.

`build.go` depends on `github.com/moby/buildkit` (direct dep, pinned in `go.mod`).

`build` passes an empty temporary directory as the docker build context — the Dockerfile downloads
everything, so no local files need shipping. This keeps the context transfer instant.

An integration test (gated by `MAKESLOP_DOCKER_IT=1`) exercises the full `Build` flow against a
live Docker daemon. See [Contributing / Build](#contributing--build) for the command.

---

## Preflight helpers

`internal/docker/preflight.go` provides two shared helpers used by both `run` and `status`:

- **`CheckDaemon(ctx context.Context) error`** — pings the daemon via `newClientFn`; returns
  `ErrDaemonUnreachable` on failure.
- **`ImageExists(ctx context.Context, image string) (bool, error)`** — calls `ImageInspect`;
  returns `(true, nil)` when found, `(false, nil)` only when `cerrdefs.IsNotFound(err)`, and
  `(false, err)` for any other error (so a dead daemon is never misreported as "image absent").

Both helpers build and close their own client (two constructions per `status` run — accepted for
simplicity).

---

## Config-driven scan engine

`internal/security.Scan` uses a native Go `filepath.WalkDir` walk — there is no `fd`/`fdfind`
dependency. Patterns (basename globs) and skip-dirs are passed in at call time; the engine has no
hardcoded defaults. If `patterns` is empty, `Scan` returns `nil` immediately (no walk).

Walk errors (e.g. unreadable subdirectories) are propagated immediately and abort `runRun` before
`docker.Run`. This "fail-loud" invariant ensures makeslop never silently skips a directory it
cannot prove is secret-free — consistent with the no-`.env`-leak contract.

The defaults live as active values in the `Scaffold` stub seeded by `makeslop init`. Pre-existing
project `.makeslop.yaml` files are never auto-migrated; users with an old stub must manually add an
`exclude.scan` block.

---

## Version constants

`internal/config/config.go` defines two distinct constants:

```go
CurrentVersion   = 1  // settings schema version — increment when Settings fields change
MigrationVersion = 2  // directory generation — increment when Dockerfile changes
```

The `Settings` struct records both:

```json
{
    "version": 1,
    "migrated_version": 2,
    ...
}
```

- **`version` / `CurrentVersion`** — gates JSON schema compatibility. Increment when the
  `Settings` struct fields change.
- **`migrated_version` / `MigrationVersion`** — gates the one-shot directory refresh. Increment
  whenever `internal/assets/files/Dockerfile` is modified so that existing installs pick up the
  new Dockerfile on the next `makeslop migrate`.

The two constants serve different purposes and are bumped independently.

**When to bump:** if `internal/assets/files/Dockerfile` changes, bump `MigrationVersion`. If
`Settings` struct fields change, bump `CurrentVersion`. The socat-volume proxy transport change and
the network-default inversion (direct-by-default) did not change either constant — `Settings`
fields and the embedded Dockerfile were unchanged.

---

## POSIX-only invariant

makeslop targets POSIX systems only. Tests that rely on TTY/signal behavior call `SkipNonPOSIX` at
the top. Do not add Windows compatibility paths.

---

## Exit-code contract

`docker.ExitError{Code int}` (in `run.go`) is the only exit-code error. `Run` returns it when
`ContainerWait` reports a non-zero `StatusCode`. `runWithExitCode` in `main.go` does:

```go
var ee *docker.ExitError
if errors.As(err, &ee) {
    return ee.Code
}
```

Signal-killed containers (e.g. SIGKILL) are reported by the daemon as `StatusCode=137`; that value
is passed through verbatim. There is no OS `WaitStatus` / `exec.ExitError` handling — makeslop
does not fork the docker binary.

---

## Contributing / Build

```
go build ./cmd/makeslop
go test ./...
```

Tests do not use shell shims, so there is no `noexec`/`GOTMPDIR` constraint. The `GOTMPDIR`
prefix (`GOTMPDIR=/home/user go test ./...`) remains harmless if you have it in muscle memory, but
is no longer required.

To run the Docker integration test against a live daemon:

```
MAKESLOP_DOCKER_IT=1 go test -tags integration ./internal/docker/
```

The version string is stamped at build time:

```
go build -ldflags "-X main.version=$(git describe --tags --always --dirty)" ./cmd/makeslop
```

A plain `go build` without ldflags prints `dev` for the version.
