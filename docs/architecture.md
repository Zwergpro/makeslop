# makeslop — Architecture and Internals

This document is for contributors and anyone who wants to understand how makeslop works under the
hood. It covers the key design patterns, module boundaries, and invariants. The authoritative
agent-facing notes live in `CLAUDE.md`; this file is a self-contained human-readable companion.

## Table of Contents

- [Pure/impure split](#pureimpure-split)
- [Mount groups and cache overlays](#mount-groups-and-cache-overlays)
- [apiClient seam and fake clients](#apiclient-seam-and-fake-clients)
- [Preflight helpers](#preflight-helpers)
- [Image resolution](#image-resolution)
- [Config-driven scan engine](#config-driven-scan-engine)
- [Settings schema](#settings-schema)
- [POSIX-only invariant](#posix-only-invariant)
- [Exit-code contract](#exit-code-contract)
- [Contributing / Build](#contributing--build)

---

## Pure/impure split

Argv assembly (`internal/docker/spec.go`) is **pure** and fully table-tested. Side-effecting SDK
calls live in `internal/docker/run.go` and `internal/docker/preflight.go`. Pure functions never touch
the filesystem or exec anything.

`spec.go` exposes two renderings of the same logical spec:

- `Args()` / `ShellCommand()` — argv slices used for `--dry-run` output.
- `ContainerConfig()` / `HostConfig()` — pure projections to SDK structs consumed by `Run`.

A drift-guard test keeps both renderings honest. The "printed == executed" invariant holds: what
`--dry-run` prints is what `run` passes to the Docker daemon.

---

## Mount groups and cache overlays

`BuildSpec` in `internal/docker/spec.go` organises mounts into logical groups. With both sandbox
policy mounts active (`ProtectProjectConfig` and `MaskGitHooks`) and both cache overlays enabled,
the spec can include up to 10 mounts. The three logical groups are:

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
methods used by `Run`, `CheckDaemon`, and `ImageExists`. A compile-time assertion
`var _ apiClient = (*moby.Client)(nil)` guards against signature drift. Adding an SDK call means
extending `apiClient` and the fakes below.

The interface covers: `ContainerCreate`, `ContainerAttach`, `ContainerStart`, `ContainerWait`,
`ContainerResize`, `ContainerRemove`, `Ping`, `ImageInspect`, `Close`.

`internal/docker` uses constructor dependency injection. `docker.New(opts ...Option)` builds a
real moby client from the environment; `WithClient(c apiClient)` (same-package `_test.go` only)
injects a fake. There is no `testing.go` production file, no `SetClientForTest`, no
`newClientFn` package-level variable, and no exported `FakeRunClient` type.

Test fakes live in `_test.go` files (compiled only during `go test`):

- **`noopClient`** (`internal/docker/fakes_test.go`) — zero-behavior implementation of every
  `apiClient` method; embedded by the fakes so each only overrides what it scripts.
- **`fakeRunClient`** (`internal/docker/fakes_test.go`) — simulates the preflight/`Run` lifecycle
  with a scripted exit code. Supports `PingErr`, `ImageMissing`, `ImageErr`, `BlockPing`,
  `BlockImageInspect` fields.
- **`fakeClient`** (`internal/docker/run_test.go`) — the `Run`-lifecycle fake used by
  `run_test.go`; distinct from `fakeRunClient`. Has `attachPayload` to script delayed output.

`internal/cli` depends on consumer-side interfaces in `internal/cli/deps.go` (`containerRunner`,
`daemonChecker`, `imageChecker`), bundled in `dockerDeps`. Tests build the command tree with
`newRootCmdWithDeps(baseDir, deps)` and the `fakeDocker` boundary fake in
`internal/cli/main_test.go`. If `docker.New()` fails, `dockerNewErrStub` defers the error to the
first Docker call so non-Docker commands (`config`, `ls`, `version`, …) still work.

There are no shell shims, no `dockerBinary` global, no `executableTempDir`.

---

## Preflight helpers

`internal/docker/preflight.go` provides two shared helpers used by both `run` and `status`:

- **`CheckDaemon(ctx context.Context) error`** — pings the daemon via the shared `d.client`;
  returns `*ErrDaemonUnreachable` on failure.
- **`ImageExists(ctx context.Context, image string) (bool, error)`** — calls `ImageInspect` on
  `d.client`; returns `(true, nil)` when found, `(false, nil)` only when
  `cerrdefs.IsNotFound(err)`, and `(false, err)` for any other error (so a dead daemon is never
  misreported as "image absent").

Both methods share the `*Docker`'s single long-lived client — no per-call client construction or
close. `cmd` callers must `defer d.Close()` once after construction to release the connection.

In `internal/cli`, both calls go through `dockerDeps.checkDaemonPreflight` /
`imageExistsPreflight`, which bound them with `preflightTimeout` (10s) so a black-hole
`DOCKER_HOST` cannot hang `run` or `status`. `Run` itself gets no deadline.

---

## Image resolution

makeslop has no embedded image, no build step, and no default image. `resolveImage` in
`internal/cli/image.go` picks the image for `run` and `status`: the `-i/--image` flag wins, then
the `image` setting; both empty (after trimming) yields `errNoImage`. The chosen value goes through
`config.NormalizeImage` (also used by `config set image`), which rejects anything that isn't a
valid docker reference, including a leading `-` that `--dry-run` would otherwise print as a flag.
The `-i/--image` flag is
registered on those two commands only.

- `run` resolves the image **before** `ws.Lookup`, so a config error fails fast (and `--dry-run`
  also needs an image, since the printed command must equal the executed one).
- `status` resolves it in its image check. An explicit `-i` bypasses the settings, so the check
  works even when `settings.json` is absent or corrupt.
- `init` never fails on a missing image; it prints a non-blocking note built from the same
  `noImageHint` text.
- A resolved image missing from the local daemon fails with `imageNotFoundHint` ("build or pull
  it"). There is no auto-pull.

---

## Config-driven scan engine

`internal/security.Scan` uses a native Go `filepath.WalkDir` walk — there is no `fd`/`fdfind`
dependency. Patterns (basename globs) and skip-dirs are passed in at call time; the engine has no
hardcoded defaults. If `patterns` is empty, `Scan` returns `(nil, nil, nil)` immediately (no walk). Symlinks whose
basename matches a pattern are returned in the second slice (`symlinkMatches`) rather than the
first — WalkDir does not follow symlinks, so they are not masked; callers should warn the user.
These symlink warnings bypass `--quiet` (degraded protection is never treated as cosmetic).

Walk errors (e.g. unreadable subdirectories) are propagated immediately and abort `runRun` before
`docker.Run`. This "fail-loud" invariant ensures makeslop never silently skips a directory it
cannot prove is secret-free — consistent with the no-`.env`-leak contract.

The defaults live as active values in the `Scaffold` stub seeded by `makeslop init`. Pre-existing
project `.makeslop.yaml` files are never auto-migrated; users with an old stub must manually add an
`exclude.scan` block.

---

## Settings schema

`~/.makeslop/settings.json` has no version stamp and no migration step. `config.Load` defaults
`Shell` and `TmpDirSize` when they are empty; `Image` is never defaulted (empty means unset).
Obsolete keys from older versions (`version`, `migrated_version`) are ignored on load and dropped
on the next `Save`, because `Settings` no longer has fields for them.

Schema changes must therefore stay backward compatible: add fields with `omitempty` plus
load-time defaulting rather than renaming or repurposing existing keys.

`config.Bootstrap` seeds only the agent directories (`.claude/`, `.codex/`, `workspaces/`) and an
empty `.claude.json`, never overwriting existing files. There are no embedded assets.

Every read-modify-write of `settings.json` goes through `config.Update` / `config.WithLock`
(in-process mutex plus `flock` on `<baseDir>/.settings.lock`). Never nest `WithLock`, including
inside an `Update` mutate func: the nested call self-deadlocks.

---

## POSIX-only invariant

makeslop targets POSIX systems only. Tests that rely on TTY/signal behavior call an inline
`skipNonPOSIX` helper defined locally in each test package (unexported, not shared across
packages). Do not add Windows compatibility paths.

---

## Exit-code contract

`docker.ExitError{Code int}` (in `run.go`) is the only exit-code error. `Run` returns it when
`ContainerWait` reports a non-zero `StatusCode`. `runWithExitCode` in `internal/cli/root.go` does:

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

The CLI lives in `internal/cli` (package `cli`). `cli.Main(version string, args []string) int` is
the single exported entry point. `cmd/makeslop/main.go` is ~10 lines: `var version = "dev"` (the
ldflags landing pad) and `func main() { os.Exit(cli.Main(version, os.Args[1:])) }`.

```
go build ./cmd/makeslop
go test -timeout=100s ./...
golangci-lint run
```

Tests do not use shell shims, so there is no `noexec`/`GOTMPDIR` constraint, and none of them
need a live Docker daemon.

The version string is stamped at build time:

```
go build -ldflags "-X main.version=$(git describe --tags --always --dirty)" ./cmd/makeslop
```

A plain `go build` without ldflags prints `dev` for the version.
