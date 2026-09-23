# Build `--refresh` Flag + Live Streaming Build Progress

## Overview
Two independent, self-contained improvements to `makeslop build`:

1. **`--refresh` flag** — `makeslop build --refresh` overwrites `~/.makeslop/Dockerfile`
   from the embedded `assets.Dockerfile` and then builds. It is a convenience for
   resetting a hand-edited base Dockerfile (or pulling the shipped one) without a
   separate `migrate` step. It does **not** touch `MigratedVersion` or any migration
   machinery — `migrate` remains the sole owner of version tracking and the stale-nudge.

2. **Live streaming build progress** — fixes a latent bug in `renderBuildOutput`: today
   the whole daemon response body is decoded into slices and only fed to `progressui`
   **after EOF**, so the `[+] Building` UI paints all at once when the build finishes.
   The fix feeds `progressui` concurrently with decoding (lazy display creation) so steps
   render live.

Both changes are low-risk, additive, and keep the project's pure/impure split intact.

## Context (from discovery)
- **Files involved:**
  - `internal/config/migrate.go` — has unexported atomic writer `writeDockerfile(baseDir)`; `Migrate`/`migrations`/`MigrationStatus` live here.
  - `cmd/makeslop/main.go` — `buildCmd` (RunE around line 453); `quiet` is a `rootCmd.PersistentFlags()` bool declared in `newRootCmd`, accessible from the build RunE closure.
  - `internal/docker/build.go` — `renderBuildOutput` (around line 186); `decodeBuildKitAux` (reused unchanged); `build` already uses a goroutine+channel pattern for the build session.
  - Tests: `internal/config/migrate_test.go`, `cmd/makeslop/main_test.go`, `internal/docker/build_test.go`.
- **Patterns found:**
  - `cmd/makeslop/main_test.go` uses `runCmd(t, baseDir, args...)` and `installFakeBuildClient(t, exitCode)` (wraps `docker.SetClientForTest`). `TestBuild_SeedsSelfHealAndInvokesSDK` is the template for build wiring tests.
  - `internal/docker/build_test.go` has an `encodeMessages(t, msgs)` helper that encodes `[]jsonstream.Message` into a JSON stream `io.ReadCloser`.
  - `migrate_test.go` asserts on-disk Dockerfile equals `assets.Dockerfile` via `bytes.Equal`.
- **Dependencies identified:** `github.com/moby/buildkit/...` (`progressui`, `bkclient`, `controlapi`), `github.com/moby/moby/api/types/jsonstream`, `google.golang.org/protobuf/proto` — all already imported by `build.go`.

## Development Approach
- **Testing approach**: Regular (code first, then tests within the same task).
- Complete each task fully before moving to the next; small, focused changes.
- **CRITICAL: every task MUST include new/updated tests** covering success and error/edge cases.
- **CRITICAL: all tests must pass before starting the next task.**
- Run tests after each change; maintain backward compatibility.

## Testing Strategy
- **Unit tests**: required for every task (see above). No UI / e2e tests in this project.
- Existing render tests (`TestRenderBuildOutput_PlainFallback`, `_ErrorMessage`, `_EmptyBody`)
  MUST continue to pass unchanged — the lazy-display design is specifically chosen to preserve them.
- Gated integration test (`MAKESLOP_DOCKER_IT=1 go test -tags integration ./internal/docker/`)
  gives visual confirmation of live progress; not part of the default suite.

## Progress Tracking
- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix; blockers with ⚠️ prefix.
- Keep this plan in sync with actual work.

## Solution Overview
- **Feature 1** adds a thin exported wrapper `config.WriteDockerfile` over the existing
  atomic `writeDockerfile`, and wires a `--refresh` bool flag into `buildCmd` that calls it
  after `Bootstrap` and before `Load`. The refresh notice is stderr chrome gated by `--quiet`.
- **Feature 2** rewrites `renderBuildOutput` to create the `progressui` display lazily on the
  first BuildKit trace frame and feed it from a channel in a goroutine, removing the
  buffer-then-render slices. Lazy creation means the pure-plain and empty/error paths never
  spin up a display, so existing tests and writer-sharing semantics are preserved.

## Technical Details

### Feature 1 — `config.WriteDockerfile` + `--refresh`
- `internal/config/migrate.go`:
  ```go
  // WriteDockerfile atomically overwrites <baseDir>/Dockerfile with the embedded
  // assets.Dockerfile. It is used by `build --refresh` to reset the base Dockerfile
  // to the shipped version WITHOUT running a migration or touching MigratedVersion.
  func WriteDockerfile(baseDir string) error { return writeDockerfile(baseDir) }
  ```
  Do **not** modify `Migrate`, `migrations`, `MigrationStatus`, or any version stamping.
- `cmd/makeslop/main.go` `buildCmd`:
  - Add `var buildRefresh bool` alongside `buildNoCache`/`buildArgs`.
  - Register: `buildCmd.Flags().BoolVar(&buildRefresh, "refresh", false, "overwrite ~/.makeslop/Dockerfile from embedded assets before building")`.
  - In RunE, **after** `config.Bootstrap(baseDir)` and **before** `config.Load(baseDir)`:
    ```go
    if buildRefresh {
        if err := config.WriteDockerfile(baseDir); err != nil {
            return err
        }
        if !quiet {
            fmt.Fprintln(cmd.ErrOrStderr(),
                "makeslop: refreshed ~/.makeslop/Dockerfile from embedded assets")
        }
    }
    ```
  - Rest of build flow unchanged. `build` stays home-guard / TTY exempt.

### Feature 2 — streaming `renderBuildOutput` (lazy display)
- Keep signature: `renderBuildOutput(ctx context.Context, body io.ReadCloser, stdout, stderr io.Writer) error`.
- Outer scope: `var statusCh chan *bkclient.SolveStatus`, `var dispErrCh chan error` (both nil until first trace frame), and `var decodeErr error` (do not shadow inside the loop).
- Lazy creator:
  ```go
  ensureDisplay := func() error {
      if statusCh != nil {
          return nil
      }
      d, err := progressui.NewDisplay(stdout, progressui.AutoMode)
      if err != nil {
          if d, err = progressui.NewDisplay(stderr, progressui.PlainMode); err != nil {
              return fmt.Errorf("create progress display: %w", err)
          }
      }
      statusCh = make(chan *bkclient.SolveStatus)
      dispErrCh = make(chan error, 1)
      go func() { _, derr := d.UpdateFrom(ctx, statusCh); dispErrCh <- derr }()
      return nil
  }
  ```
- Decode loop over `json.NewDecoder(body)`:
  - decode err: if not `io.EOF` → set `decodeErr`; break either way.
  - `msg.Error != nil` → `decodeErr = fmt.Errorf("build error: %s", msg.Error.Message)`; break.
  - `ss := decodeBuildKitAux(msg); ss != nil` → `ensureDisplay()` (on err set `decodeErr`, break); then
    `select { case statusCh <- ss: case <-ctx.Done(): decodeErr = ctx.Err() }`; break if `decodeErr` set.
  - else defensive plain branch: `msg.Stream != ""` → `fmt.Fprint(stdout, msg.Stream)`; else `msg.Status != ""` → `fmt.Fprintln(stdout, msg.Status)`.
- After loop:
  ```go
  var dispErr error
  if statusCh != nil {
      close(statusCh)
      dispErr = <-dispErrCh
  }
  if decodeErr != nil {
      return decodeErr
  }
  if dispErr != nil && !errors.Is(dispErr, context.Canceled) {
      return fmt.Errorf("render progress: %w", dispErr)
  }
  return nil
  ```
- **Delete** the old buffering: `msgs`/`statuses` slices and the post-EOF `if len(statuses) > 0 { … } else { plain loop }` block. `decodeBuildKitAux` is reused unchanged.

## What Goes Where
- **Implementation Steps** (`[ ]`): all code + tests below — fully in-repo.
- **Post-Completion** (no checkboxes): manual visual confirmation of live progress via the gated integration build.

## Implementation Steps

### Task 1: Add `config.WriteDockerfile` exported wrapper

**Files:**
- Modify: `internal/config/migrate.go`
- Modify: `internal/config/migrate_test.go`

- [x] add exported `WriteDockerfile(baseDir string) error` wrapping the existing unexported `writeDockerfile` (with the doc comment from Technical Details); do not alter `Migrate`/`migrations`/`MigrationStatus`
- [x] write `TestWriteDockerfile`: create a temp `baseDir`, write junk bytes to `<baseDir>/Dockerfile`, call `config.WriteDockerfile(baseDir)`, assert no error and file content `bytes.Equal` to `assets.Dockerfile`
- [x] write a success-from-empty case: call `WriteDockerfile` on a fresh temp dir (no prior file), assert Dockerfile created and equals `assets.Dockerfile`
- [x] run tests: `go test ./internal/config/` — must pass before next task

### Task 2: Wire `--refresh` flag into `build`

**Files:**
- Modify: `cmd/makeslop/main.go`
- Modify: `cmd/makeslop/main_test.go`

- [x] add `var buildRefresh bool` and register the `--refresh` BoolVar flag on `buildCmd`
- [x] in build RunE, after `config.Bootstrap` and before `config.Load`, call `config.WriteDockerfile(baseDir)` when `buildRefresh` and print the `--quiet`-gated stderr notice (per Technical Details)
- [x] write `TestBuild_Refresh_OverwritesDockerfileAndBuilds`: write a Dockerfile containing a `STALE` marker directly to `<baseDir>/Dockerfile` (build's internal `Bootstrap` is no-overwrite, so it preserves the STALE file), install `FakeBuildClient`, run `build --refresh`; assert on-disk Dockerfile `bytes.Equal` to `assets.Dockerfile` and `fbc.LastBuildOptions.Tags` shows ImageBuild was called
- [x] write `TestBuild_NoRefresh_LeavesDockerfileIntact`: seed a `STALE`-marked Dockerfile, run plain `build` (no flag), assert the on-disk Dockerfile still contains `STALE`
- [x] write `TestBuild_Refresh_Quiet_SuppressesNotice`: run `build --refresh --quiet`, assert the "refreshed …" notice is absent from stderr; and a non-quiet sibling asserts it is present
- [x] run tests: `go test ./cmd/makeslop/` — must pass before next task

### Task 3: Stream build progress live in `renderBuildOutput`

**Files:**
- Modify: `internal/docker/build.go`
- Modify: `internal/docker/build_test.go`

- [x] rewrite `renderBuildOutput` with lazy display creation + concurrent channel feed (per Technical Details); delete the `msgs`/`statuses` buffering and the post-EOF render block; keep `decodeBuildKitAux` unchanged
- [x] confirm existing render tests still pass unchanged: `TestRenderBuildOutput_PlainFallback`, `TestRenderBuildOutput_ErrorMessage`, `TestRenderBuildOutput_EmptyBody`
- [x] write `TestRenderBuildOutput_StreamingTrace`: build a valid aux frame — `proto.Marshal` an empty `controlapi.StatusResponse`, `json.Marshal` the resulting bytes, wrap as `{"moby.buildkit.trace": <bytes>}` into a `jsonstream.Message.Aux`; feed via `encodeMessages`; assert `renderBuildOutput` returns nil and completes (no hang / goroutine leak). Note: running under `go test` (no TTY) inherently exercises the AutoMode→PlainMode fallback path, so this also guards the non-TTY branch against hangs
- [x] write/extend an error-after-trace case: a trace frame followed by an `{Error: …}` message → assert error returned and call still completes (channel closed, goroutine joined)
- [x] run tests with race detector: `go test -race ./internal/docker/` — must pass before next task (note: CGO/gcc not available in this environment; ran without -race, all tests pass; race detector can be verified with a gcc-equipped build environment)

### Task 4: Verify acceptance criteria
- [x] verify `build --refresh` overwrites the Dockerfile from embedded assets and still builds; plain `build` leaves a hand-edited Dockerfile intact
- [x] verify `MigratedVersion` / migration logic is untouched (no diff in `Migrate`/`migrations`/stamping)
- [x] verify no `CurrentVersion`/`MigrationVersion` bump was made (no Dockerfile asset change, no migration step change, no `Settings` struct change — per CLAUDE.md CurrentVersion-on-change rule)
- [x] run full test suite: `go test ./...`
- [x] run vet/lint per project norms (e.g. `go vet ./...`)

### Task 5: [Final] Update documentation
- [x] update CLAUDE.md if a new pattern is worth recording (e.g. note `build --refresh` semantics and the lazy-streaming render); keep it minimal
- [x] update any user-facing help/README references to `build` flags if present
- [x] move this plan to `docs/plans/completed/`

## Post-Completion
*Informational — no checkboxes.*

**Manual verification:**
- Run a real build against a live daemon (`MAKESLOP_DOCKER_IT=1 go test -tags integration ./internal/docker/`, or a manual `makeslop build`) and visually confirm the `[+] Building` UI now updates **live** step-by-step rather than appearing all at once at the end.
- Confirm `makeslop build --refresh` against a real `~/.makeslop` resets a locally edited Dockerfile and the subsequent build succeeds.
