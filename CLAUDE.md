# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

makeslop is a Go CLI that runs Claude Code / Codex inside a per-project Docker container
(user-supplied image; makeslop never builds or pulls) with controlled mounts and secret masking. It talks to the daemon through the
moby/moby Go SDK; the `docker` CLI binary is not used. User-facing docs live in `docs/`
(`reference.md`, `security.md`, `architecture.md`); this file is agent-facing notes only.

## Commands

```sh
go build ./cmd/makeslop                         # build (version prints "dev")
make build                                      # install to ~/.local/bin with git-describe version
go test -timeout=100s ./...                     # full suite (what CI runs)
go test ./internal/docker -run TestBuildSpec    # single package / test
golangci-lint run                               # lint (CI pins v2.12.2, config in .golangci.yml)
```

The version string is injected with `-ldflags "-X main.version=…"` in `cmd/makeslop/main.go`,
which just calls `cli.Main(version, args)`.

## Layout

- `internal/cli` — cobra commands (`init`, `run`, `status`, `config`, `ls`, `remove`, `version`). `root.go` holds `Main`, `runWithExitCode`, and the exit-code contract.
- `internal/docker` — `Docker` type wrapping the SDK: `spec.go` (pure), `run.go`,
  `preflight.go`, `client.go`.
- `internal/config` — `~/.makeslop/settings.json`, bootstrap of `~/.makeslop/`, settings locking.
- `internal/projectconfig` — parses the per-project `.makeslop.yaml`.
- `internal/workspace` — maps project roots to named workspaces and their cache dirs.
- `internal/security` — secret scan (`filepath.WalkDir` over basename globs).
- `examples/claudebox/Dockerfile` — unembedded example image; users build it themselves.

## Architecture and invariants

### Pure/impure split
`internal/docker/spec.go` is pure and heavily table-tested: `BuildSpec(Options) Spec`, then two
renderings of the same spec — `Args()`/`ShellCommand()` for `--dry-run`, and
`ContainerConfig()`/`HostConfig()` for the SDK call in `Run`. A drift-guard test keeps them in
sync ("printed == executed"). Pure functions never touch the filesystem or exec; side effects
belong in `run.go`.

Mount order in `BuildSpec`: project root first, then global mounts (`~/.makeslop/.claude/`,
`.claude.json`, `.codex/`), then per-workspace overlays gated by `MountAgentCache` /
`MountContentCache`, then secret masks last so a mask always wins. The two booleans default to
`false` in Go, so tests wanting full mounts must set them.

### Dependency injection (no global test hooks)
- **docker package:** `docker.New(opts ...Option)` with `WithClient`, `WithTTYCheck`,
  `WithRawMode`, `WithStreams`. `apiClient` in `client.go` is the narrow SDK subset in use; the
  `var _ apiClient = (*moby.Client)(nil)` assertion catches SDK drift. Adding an SDK call means
  extending `apiClient` and the fakes in `internal/docker/fakes_test.go`.
- **cli package:** commands depend on consumer-side interfaces in `internal/cli/deps.go`
  (`containerRunner`, `daemonChecker`, `imageChecker`). Tests build the tree with
  `newRootCmdWithDeps(baseDir, deps)` and a `fakeDocker` (see `internal/cli/main_test.go`). If
  `docker.New()` fails, `dockerNewErrStub` defers the error so non-docker commands still work.

### Context, timeouts, exit codes
- `runWithExitCode` wraps execution in `signal.NotifyContext(SIGINT, SIGTERM)`; every `RunE` must
  use `cmd.Context()`.
- Preflight calls (daemon ping, image inspect) go through `dockerDeps.checkDaemonPreflight` /
  `imageExistsPreflight`, bounded by `preflightTimeout` (10s). `Run` gets no deadline.
- `ImageExists` returns `(false, nil)` only on `cerrdefs.IsNotFound`; other errors propagate so a
  dead daemon is never reported as "image absent".
- Exit codes: `*docker.ExitError{Code}` passes the container's status through (137 for SIGKILL
  included); `errSilent` means exit 1 with nothing printed (the message was already printed);
  any other error prints `makeslop: <err>` and exits 1.

### Settings
- `image` is never defaulted; commands resolve it via `resolveImage` (flag > settings >
  `errNoImage`) in `internal/cli/image.go`. `-i/--image` exists on `run` and `status` only. `run`
  resolves before `ws.Lookup` so config errors fail fast; `init` prints a non-blocking note when
  the image is unset. A missing local image fails with a `docker pull` hint (no auto-pull).
- `Load` still defaults `Shell` and `TmpDirSize`. There is no version stamp or migration step:
  obsolete keys (`version`, `migrated_version`) are ignored and dropped on the next `Save`.
- Every `settings.json` read-modify-write goes through `config.Update` / `config.WithLock`: an
  in-process mutex plus `flock` on `<baseDir>/.settings.lock`. **Never nest `WithLock`**, including
  inside an `Update` mutate func: the nested call self-deadlocks.

### Project config (`.makeslop.yaml`)
- Decoded in strict mode (`KnownFields(true)`), so unknown keys are hard errors. That includes the
  `network:` block from older versions.
- The file must be a regular file; a symlink is rejected. When it exists, it is mounted read-only
  over itself in the container (`ProtectProjectConfig`), and `.git/hooks` is tmpfs-masked
  (`MaskGitHooks`).
- `Load` returns `(Excludes, Cache, env []string, error)`. A missing `cache:` block means
  `{Content:true, Agent:true}`. `init --global-only` scaffolds `{false,false}`. `Scaffold` is
  idempotent and never overwrites an existing file.
- Existing project files are never auto-migrated.

### Secret scan
`security.Scan` has no built-in defaults: patterns and skip-dirs come only from `.makeslop.yaml`,
and empty patterns skip the walk. Walk errors abort `run` before the container starts ("fail
loud": never skip a directory we can't prove is secret-free).

### Command-scope rules
- The TTY requirement applies to `run` only. All other commands must stay CI/pipe-safe.
- The home-directory guard (`internal/cli/guard.go`) applies to `run` and `init`. `--out-of-home`
  is registered only on those two; `--global-only` only on `init`.
- `--quiet` is a persistent flag. It suppresses stderr chrome (errors still print) and never
  touches stdout.
- `status` runs ordered checks with `✓/✗/–/!` glyphs, supports `--json`, and exits non-zero if a
  blocking check fails.

### POSIX only
Do not add Windows code paths. Tests that depend on TTY or signal behavior call the package-local
`skipNonPOSIX(t, why)` helper.
