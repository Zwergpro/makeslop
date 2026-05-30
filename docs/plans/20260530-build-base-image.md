# Add `makeslop build` subcommand

## Overview
- Add a `makeslop build` subcommand that builds (or rebuilds) the base docker image
  from `~/.makeslop/Dockerfile` using `docker build`.
- Solves a current gap: `makeslop go` launches a container from the `claudebox`
  image (configurable via `settings.json`), and `init`/`migrate` seed the
  `Dockerfile`, but **nothing actually builds the image**. Users must run
  `docker build` by hand. `build` closes that loop.
- Integrates by reusing the established pieces: `config.Bootstrap` for self-heal
  seeding, `config.Load` for the image tag, and a new pure-argv + exec pair in the
  `internal/docker` package mirroring the existing `BuildSpec`/`Run` split.

## Context (from discovery)
- **Project**: `makeslop`, a Go CLI (cobra) that maps cwd → per-workspace cache
  under `~/.makeslop/` for docker-based workflows. POSIX-only invariant.
- **Files/components involved**:
  - `cmd/makeslop/main.go` — cobra command wiring (`init`, `go`, `migrate`); add `build`.
  - `internal/docker/spec.go` — pure argv assembly (`BuildSpec`, `Spec.Args`); add build-argv assembly.
  - `internal/docker/run.go` — exec wrapper (`Run`, swappable `dockerBinary`); add `Build`.
  - `internal/config/config.go` — `Bootstrap`, `Load`, `DefaultImage="claudebox"`, `DockerfileFile="Dockerfile"`.
  - `internal/assets/files/Dockerfile` — the embedded base image definition.
- **Patterns found**:
  - Pure/impure split: argv assembly is pure and table-tested; `exec` lives in `run.go`.
  - `dockerBinary` package var swapped in tests via `docker.SetDockerBinaryForTest`.
  - CLI tests use `runCmd(t, baseDir, args...)` and `docker.WriteShim` to record argv.
  - Errors that already printed to stderr return `errSilent`; `*exec.ExitError` propagates
    the child exit code through `runWithExitCode`.
- **Dependencies identified**: `spf13/cobra`, `golang.org/x/term`. No docker SDK — shells out to `docker`.

## Development Approach
- **Testing approach**: Regular (code first, then tests within the same task).
- Complete each task fully before moving to the next.
- Make small, focused changes.
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task.
- **CRITICAL: all tests must pass before starting the next task** (`go test ./...`).
- Run tests after each change. Maintain backward compatibility (`go`, `init`, `migrate` unchanged).

## Testing Strategy
- **unit tests**: required every task — table-driven argv tests for the pure builder;
  shim-based exec tests for `Build`; `runCmd`-based CLI tests for the subcommand.
- **e2e tests**: project has no UI/browser e2e suite. The shim-based docker tests
  (record argv, control exit code) are the integration boundary and play that role.

## Progress Tracking
- mark completed items with `[x]` immediately when done.
- add newly discovered tasks with ➕ prefix.
- document blockers with ⚠️ prefix.
- keep this plan in sync with actual work.

## Solution Overview
- **Command surface**: dedicated `makeslop build`. `go` is untouched (no auto-build).
  Flags:
  - `--no-cache` — forward `--no-cache` to `docker build` for a clean rebuild (covers
    the "rebuild" half of the request); default off uses docker's layer cache.
  - `--build-arg K=V` — repeatable (`StringArray`), each forwarded verbatim as
    `--build-arg K=V` (generic passthrough; covers proxy/version args).
- **Self-heal**: `build` calls `config.Bootstrap(baseDir)` first, seeding
  `~/.makeslop/` (incl. `Dockerfile`) when absent, then builds — so it works on a
  fresh machine with no prior `init`. Bootstrap is idempotent and never overwrites
  user edits.
- **Image tag**: from `config.Load(baseDir).Image` (default `claudebox`).
- **No TTY requirement**: unlike `go`, `build` streams output and works in CI/pipes;
  it does not consult `ttyCheck`.
- **Empty build context + BuildKit**: the embedded Dockerfile does no `COPY` (it
  downloads everything), and it uses BuildKit cache mounts (`--mount=type=cache`).
  So `Build` builds from an **empty temp dir** as context with `-f <baseDir>/Dockerfile`,
  and runs with `DOCKER_BUILDKIT=1` in the child env. This avoids shipping all of
  `~/.makeslop/` (workspace state) to the daemon and guarantees cache mounts work.

## Technical Details
- **Pure argv** (`internal/docker/spec.go`):
  ```
  type BuildOptions struct {
      Image          string   // -t tag (required)
      DockerfilePath string   // -f path (required)
      ContextDir     string   // positional build context (required, non-empty)
      NoCache        bool     // --no-cache when true
      BuildArgs      []string // each appended as: --build-arg <entry>
  }
  func BuildArgv(o BuildOptions) []string // starts with "build"
  ```
  Argv order (deterministic): `build` [`--no-cache`] `-f <dockerfile>` `-t <image>`
  (`--build-arg <e>`)* `<contextDir>`.
  - **Ownership/coupling note**: `BuildArgv` treats `ContextDir` as **required and
    non-empty** — it is a pure projection and never invents a path. The empty temp
    context dir is created and owned entirely by `Build` (below), which sets
    `o.ContextDir` before calling `BuildArgv`. Production CLI callers leave the field
    unset; `Build` fills it. Task 1 tests therefore always pass a real (non-empty)
    `ContextDir` — an empty `ContextDir` is out of contract and must not be asserted
    as a valid argv shape.
- **Exec** (`internal/docker/run.go`):
  ```
  func Build(ctx context.Context, o BuildOptions, stdout, stderr io.Writer) error
  ```
  - Creates an empty temp context dir via `os.MkdirTemp("", "makeslop-build-*")`
    when `o.ContextDir == ""`, sets it on `o`, and `defer os.RemoveAll`s it.
  - `exec.CommandContext(ctx, dockerBinary, BuildArgv(o)...)`.
  - `cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")` — load-bearing: the
    Dockerfile uses `--mount=type=cache`, which silently no-ops without BuildKit.
  - `cmd.Stdout = stdout; cmd.Stderr = stderr; cmd.Stdin = nil` (no TTY needed; `Build`
    never consults `ttyCheck` and can never return `ErrNoTTY`).
  - Returns `cmd.Run()` — `*exec.ExitError` propagates so the CLI maps the build
    failure to a non-zero host exit code.
- **Test shim extension**: `WriteShim` only records argv, so the BuildKit env and the
  generated temp-context path need a slightly richer shim for Task 2. Add a
  build-specific shim (or extend `WriteShim`) that additionally records
  `$DOCKER_BUILDKIT` to a sibling `env.txt`; the generated context dir is recovered as
  the **last token of the recorded argv** (no other API exposes it).
- **CLI** (`cmd/makeslop/main.go`): new `buildCmd` added in `newRootCmd`, registered
  via `rootCmd.AddCommand(initCmd, goCmd, migrateCmd, buildCmd)`. `RunE`:
  `config.Bootstrap` → `config.Load` → assemble `docker.BuildOptions{Image: s.Image,
  DockerfilePath: filepath.Join(baseDir, config.DockerfileFile), NoCache, BuildArgs}`
  → `docker.Build(cmd.Context(), o, cmd.OutOrStdout(), cmd.ErrOrStderr())`.
  Build-arg entries are forwarded verbatim (docker validates `K`/`K=V` itself).

## What Goes Where
- **Implementation Steps** (`[ ]`): pure builder, exec runner, CLI wiring, docs — all in-repo.
- **Post-Completion** (no checkboxes): manual `docker`-on-PATH verification of a real build.

## Implementation Steps

### Task 1: Pure `docker build` argv assembly

**Files:**
- Modify: `internal/docker/spec.go`
- Modify: `internal/docker/spec_test.go`

- [x] add `BuildOptions` struct (Image, DockerfilePath, ContextDir, NoCache, BuildArgs) to `spec.go`.
- [x] add pure `BuildArgv(o BuildOptions) []string` returning argv starting with `build`,
      in the documented order; `--no-cache` only when `NoCache`; one `--build-arg <entry>`
      pair per `BuildArgs` element; `ContextDir` last.
- [x] write table-driven tests in `spec_test.go`: minimal (no flags), with `--no-cache`,
      with multiple `--build-arg` entries, and order/positional-context assertions.
- [x] write test asserting empty `BuildArgs`/`NoCache=false` emit no extra tokens.
- [x] run `go test ./internal/docker/` — must pass before Task 2.

### Task 2: `Build` exec runner with empty context + BuildKit

**Files:**
- Modify: `internal/docker/run.go`
- Modify: `internal/docker/run_test.go`

- [x] add `Build(ctx, o BuildOptions, stdout, stderr io.Writer) error` to `run.go`:
      create+defer-remove an empty temp context dir when `o.ContextDir==""`, exec
      `dockerBinary` with `BuildArgv(o)`, set `DOCKER_BUILDKIT=1` in `cmd.Env`, wire
      stdout/stderr, no TTY check.
- [x] add a build shim (or extend `WriteShim`) in `testing.go` that records argv **and**
      `$DOCKER_BUILDKIT` to a sibling `env.txt`.
- [x] write test (shim + `SetDockerBinaryForTest`) asserting recorded argv matches the
      expected build argv and that the last argv token is an existing directory.
- [x] write test asserting the child saw `DOCKER_BUILDKIT=1` (read `env.txt`) — covers
      the BuildKit invariant that argv assertions cannot see.
- [x] write test asserting a non-zero shim exit yields a wrapped `*exec.ExitError`
      with the right code (mirrors `TestRun_NonZeroExit_PropagatesCode`).
- [x] write test asserting a missing docker binary surfaces `*exec.Error`/`*os.PathError`
      (mirrors `TestRun_DockerNotFound_ReturnsError`); guard with `SkipNonPOSIX`.
      **Divergence**: do NOT stub `ttyCheck` and do NOT assert `ErrNoTTY` — `Build`
      ignores the TTY entirely.
- [x] write test asserting the temp context dir is removed after `Build` returns:
      recover the path as the last token of the recorded argv, then assert `os.Stat`
      returns `fs.ErrNotExist`.
- [x] run `go test ./internal/docker/` — must pass before Task 3.

### Task 3: Wire the `build` subcommand

**Files:**
- Modify: `cmd/makeslop/main.go`
- Modify: `cmd/makeslop/main_test.go`

- [x] add `buildCmd` in `newRootCmd` with `--no-cache` (`BoolVar`) and `--build-arg`
      (`StringArrayVar`) flags; `Args: cobra.NoArgs`; `SilenceUsage: true`.
- [x] implement `RunE`: `config.Bootstrap(baseDir)` → `config.Load(baseDir)` → build
      `docker.BuildOptions{Image: s.Image, DockerfilePath: filepath.Join(baseDir,
      config.DockerfileFile), NoCache, BuildArgs}` → `docker.Build(cmd.Context(), o,
      cmd.OutOrStdout(), cmd.ErrOrStderr())`.
- [x] register via `rootCmd.AddCommand(initCmd, goCmd, migrateCmd, buildCmd)`.
- [x] write CLI test (`runCmd` + shim): `build` on an empty `baseDir` seeds the
      Dockerfile (self-heal) and invokes docker with `-t claudebox` and a `-f` pointing
      at `<baseDir>/Dockerfile`.
- [x] write CLI test: `build --no-cache --build-arg GO_VERSION=1.26.3` forwards both
      flags into the recorded argv.
- [x] write CLI test: a non-zero shim exit propagates a non-zero code via `runWithExitCode`
      (no TTY stub needed — build skips `ttyCheck`).
- [x] write CLI test: a custom `settings.json` `image` value is used as the `-t` tag.
      Note: `config.Bootstrap` does NOT create `settings.json`, so this test must write
      one itself (e.g. via `config.Save`) before invoking `build`.
- [x] run `go test ./...` — must pass before Task 4.

### Task 4: Verify acceptance criteria
- [x] verify `makeslop build` builds the image from `~/.makeslop/Dockerfile` with the
      configured tag (Overview requirement).
- [x] verify `--no-cache` and repeatable `--build-arg` reach docker (rebuild + passthrough).
- [x] verify self-heal: `build` with no prior `init` seeds `~/.makeslop/` then builds.
- [x] verify `go`/`init`/`migrate` behavior is unchanged (no regressions).
- [x] run full suite: `go test ./...`
- [x] run `go vet ./...` and `go build ./cmd/makeslop`.

### Task 5: [Final] Update documentation
- [ ] add a `makeslop build` entry to the README Usage list and document `--no-cache`
      and `--build-arg`, the empty-context/BuildKit note, and the self-heal behavior;
      update the first-run flow to mention `build`.
- [ ] update CLAUDE.md if any new pattern was discovered (likely none).
- [ ] move this plan to `docs/plans/completed/`.

## Post-Completion
*Items requiring manual intervention or external systems — informational only*

**Manual verification**:
- With a real `docker` on PATH, run `makeslop build` end-to-end and confirm the
  `claudebox` image is produced (`docker images claudebox`), then `makeslop go` launches it.
- Confirm cache mounts work (a second `build` is fast; `build --no-cache` rebuilds fully).
- Confirm `--build-arg HTTP_PROXY=...`/`HTTPS_PROXY=...` works for a proxied environment.
