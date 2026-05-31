# Migrate internal/docker from docker CLI to moby/moby/client SDK

## Overview
- Replace shelling out to the `docker` binary with the moby/moby/client SDK for
  **both** code paths that exec docker today:
  - `makeslop go` → interactive `docker run --rm -it …` (in `docker.Run`)
  - `makeslop build` → `docker build` with BuildKit (in `docker.Build`)
- **Goal:** fully remove the `docker` binary dependency while preserving every
  existing behavior — `--rm`, `-it` (TTY + stdin + resize), cap-drop, security-opt,
  network mode, tmpfs, masked-file/dir overlays, the `--dry-run` printed command,
  and BuildKit `--mount=type=cache` in the Dockerfile.
- Selected architecture (**Option A**): keep the pure/impure split. `spec.go`
  stays pure and gains two pure SDK-struct projections; a narrow `apiClient`
  interface becomes the test seam, replacing the shell-shim machinery.

## Context (from discovery)
- Files/components involved:
  - `internal/docker/spec.go` — pure argv assembly (`BuildSpec`, `Spec.Args`,
    `Spec.ShellCommand`, `BuildArgv`). Keep + extend.
  - `internal/docker/run.go` — impure exec of `docker run` (`Run`) and
    `docker build` (`Build`). Rewrite to use the SDK.
  - `internal/docker/testing.go` — shim helpers compiled into the prod binary
    (`WriteShim`, `WriteBuildShim`, `SetDockerBinaryForTest`, `dockerBinary`).
    Retire shim machinery; keep `ttyCheck`/`SetTTYCheckForTest`/`SkipNonPOSIX`.
  - `internal/docker/run_test.go`, `internal/docker/spec_test.go` — rewrite/extend.
  - `cmd/makeslop/main.go` — `runGo` calls `docker.Run`; `buildCmd` calls
    `docker.Build`; `runWithExitCode` (lines 376-403) maps `*exec.ExitError` →
    host exit code.
  - `cmd/makeslop/main_test.go` — drives `makeslop go`/`build` end-to-end via
    `docker.WriteShim` + `docker.SetDockerBinaryForTest` (lines 214-215, 413);
    `TestGo_ExitCodePropagation`, `TestRunWithExitCode_SignalKilledMapsTo128PlusSignum`.
  - `CLAUDE.md`, `README.md` — document invariants; update.
- Related patterns found:
  - Pure functions never touch fs/exec; argv assembly is table-tested.
  - Package-global swap points (`dockerBinary`, `ttyCheck`) are the existing test seam.
  - POSIX-only invariant; `SkipNonPOSIX` guards shim/TTY tests.
  - TTY requirement is `go`-only; `build` is CI/pipe-safe (no `ttyCheck`).
- Dependencies identified (signatures **verified literally** against module cache):
  - `github.com/moby/moby/client v0.4.1` (indirect). **Every method is
    options-struct in / result-struct out** — re-derive exact signatures from
    `client_interfaces.go` and the per-method files. Confirmed shapes:
    - `ContainerCreate(ctx, ContainerCreateOptions{Config, HostConfig, NetworkingConfig, Platform, Name}) (ContainerCreateResult{ID, Warnings}, error)`
    - `ContainerAttach(ctx, id, ContainerAttachOptions{Stream,Stdin,Stdout,Stderr,DetachKeys,Logs}) (ContainerAttachResult, error)` — `ContainerAttachResult` **embeds** `HijackedResponse` (`.Conn`, `.Reader`).
    - `ContainerStart(ctx, id, ContainerStartOptions) (ContainerStartResult, error)`
    - `ContainerWait(ctx, id, ContainerWaitOptions) ContainerWaitResult` — returns a **single struct** `{Result <-chan container.WaitResponse; Error <-chan error}` and **no error return**.
    - `ContainerResize(ctx, id, ContainerResizeOptions{Height, Width uint}) (ContainerResizeResult, error)`
    - `ContainerRemove(ctx, id, ContainerRemoveOptions) (ContainerRemoveResult, error)`
    - `ImageBuild(ctx, io.Reader, ImageBuildOptions) (ImageBuildResult{Body io.ReadCloser}, error)`
    - `DialHijack(ctx, url, proto string, meta map[string][]string) (net.Conn, error)` — **note the leading `url` param** (matters for the buildkit session dialer, see Build orchestration).
    - `Close() error`. Constructor `client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())`.
  - `github.com/moby/moby/api v1.54.2` (indirect) — `container.Config`,
    `container.HostConfig` (`AutoRemove`, `CapDrop`, `SecurityOpt`, `NetworkMode`,
    `Tmpfs map[string]string`, `Mounts []mount.Mount`), `mount.{TypeBind,TypeTmpfs}`,
    `build.BuilderBuildKit = "2"`, `container.WaitResponse{StatusCode int64, Error *WaitExitError}`.
  - `github.com/moby/buildkit` (**NEW direct dep, pin v0.30.0** — the version
    `go get` resolves today) — `session`, `session/filesync`,
    `session/auth/authprovider`, `client` (for `*client.SolveStatus`),
    `util/progress/progressui`. Required because BuildKit through the daemon
    `/build` endpoint mandates a session. ⚠️ Pulls a large transitive set
    (containerd/v2, grpc ~1.80, otel ~1.43); must not force-upgrade the
    `moby/moby/client`/`api` pins (verify via `go mod tidy`, Task 4).
  - `golang.org/x/term v0.43.0` (already present) — `MakeRaw`/`Restore`/`GetSize`.

## Development Approach
- **Testing approach**: Regular (code-first within each task; pure layers are
  naturally test-after). Matches existing repo style (tests live alongside code).
- Complete each task fully before moving to the next.
- This is a breaking refactor of one package. Tasks are ordered so the tree
  **compiles and `go test ./...` stays green at every task boundary**. Where a
  behavior swap is intrinsically atomic (Run exec → SDK), the production change
  and its test rewrite live in the **same task**.
- **CRITICAL: every task includes new/updated tests; all tests pass before the next task.**
- Maintain backward-compatible *behavior* (exit codes, dry-run output, masking).
  Internal API signatures may change (`Run`/`Build` gain client injection seams).

## Testing Strategy
- **Unit tests** (required per task):
  - Pure layer (bulk, deterministic): `BuildSpec` (keep), new
    `ContainerConfig()`/`HostConfig()` field-by-field tests (incl. tmpfs `:` split
    and mount translation), a **drift-guard** test asserting `Args()`/`ShellCommand()`
    and the struct projections agree on image/cmd/workdir/env/mounts/caps/secopt/network,
    `BuildOptions → ImageBuildOptions` mapping, build-arg `[]string → map[string]*string`
    parse, and context/dockerfile sync-dir selection.
  - `Run` orchestration via a **fake `apiClient`**: clean exit (code 0, `AutoRemove`
    ⇒ no explicit remove), non-zero exit ⇒ `docker.ExitError{Code}`, `ttyCheck`
    false ⇒ `ErrNoTTY` before any client call, `ContainerStart` failure ⇒ deferred
    force-remove fires, ctx cancel ⇒ no container leak. `SkipNonPOSIX` where
    raw-mode/SIGWINCH are exercised.
- **Integration test** (gated, skipped in normal `go test`): full `Build` flow
  (session gRPC over `DialHijack` needs a live daemon). Gate with
  `//go:build integration` (excludes it from `go test ./...`) **and** a
  `MAKESLOP_DOCKER_IT` env guard (so even `-tags integration` runs skip rather
  than fail when no daemon is reachable). Document the command to run it.
- No project e2e/UI suite exists; not applicable.
- Test command: `GOTMPDIR=/home/user go test ./...` (the `GOTMPDIR` requirement
  becomes unnecessary once shims are retired, but remains harmless).

## Progress Tracking
- Mark completed items `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix; blockers with ⚠️ prefix.
- Update this plan if scope changes.

## Solution Overview
- **Pure core preserved.** `spec.go` keeps argv assembly (for `--dry-run`) and
  gains `Spec.ContainerConfig()`/`Spec.HostConfig()` — pure projections to SDK
  structs. The two renderings (argv vs structs) are kept honest by a drift-guard test.
- **Narrow client seam.** A package-local `apiClient` interface declares only the
  methods we call; `*client.Client` satisfies it. A swappable `newClientFn`
  (default `newClient`) + `SetClientForTest` lets both the docker package tests
  and `cmd/makeslop` end-to-end tests inject a fake — replacing
  `SetDockerBinaryForTest`/shims.
- **Run** orchestrates create→attach→raw-mode→start→resize→wait→remove with full
  `-it` semantics; exit codes surface via a new exported `docker.ExitError`.
- **Build** drives BuildKit via a session (filesync + authprovider) over
  `DialHijack`, calls `ImageBuild` with `Version:"2"` + `SessionID`, and renders
  progress with `progressui`.

## Technical Details

### Pure projections (Task 2)
- `ContainerConfig()`: `Image=s.Image`, `Cmd=[]string{s.Command}`,
  `WorkingDir=s.Workdir`, `Env=s.Env`, `Tty=true`, `OpenStdin=true`,
  `AttachStdin/Stdout/Stderr=true`.
- `HostConfig()`: `AutoRemove=true` (the `--rm`), `CapDrop=s.CapDrop` (`["ALL"]`),
  `SecurityOpt=s.SecOpt` (`["no-new-privileges"]`),
  `NetworkMode=container.NetworkMode(s.NetworkMode)` (`""` ⇒ default),
  `Tmpfs` map from each `s.Tmpfs` entry split on the **first** `:`
  (`"/tmp:size=100m"` → key `"/tmp"`, value `"size=100m"`; an entry with no `:`
  → value `""`), `Mounts=mountsFor(s.Mounts)`.
- `mountsFor`: bind → `mount.Mount{Type:TypeBind, Source:Host, Target:Container,
  ReadOnly:m.ReadOnly}`; tmpfs → `mount.Mount{Type:TypeTmpfs, Target:Container}`
  (no size, matching today's `--mount type=tmpfs,target=…`); the `/dev/null`
  masked-file overlays stay binds with `Source:"/dev/null"`.
- **Note:** with `Tty:true` the attach stream is **not** stdcopy-multiplexed —
  the read side copies the hijacked conn straight to stdout (no demux).

### Run orchestration (Task 3) — replaces `docker run --rm -it`
(All calls use the options/result-struct forms from the Dependencies block.)
1. `if !ttyCheck() { return ErrNoTTY }` (unchanged contract).
2. `res, err := cli.ContainerCreate(ctx, ContainerCreateOptions{Config: s.ContainerConfig(), HostConfig: s.HostConfig()})`; `id := res.ID`.
3. `att, err := cli.ContainerAttach(ctx, id, ContainerAttachOptions{Stream:true, Stdin:true, Stdout:true, Stderr:true})`; use `att.Conn` / `att.Reader`.
4. `term.MakeRaw(int(os.Stdin.Fd()))`; `defer term.Restore`.
5. `cli.ContainerStart(ctx, id, ContainerStartOptions{})`; set `startedCleanly` on success.
6. Send initial `ContainerResize` (size from `term.GetSize`); install SIGWINCH
   handler → `ContainerResize{Height,Width}` (POSIX-only, consistent with the invariant).
7. Pump: `go io.Copy(att.Conn, os.Stdin)`; `go io.Copy(os.Stdout, att.Reader)` (no demux).
8. Wait: `wr := cli.ContainerWait(ctx, id, ContainerWaitOptions{})`; `select` on
   `wr.Result`, `wr.Error`, and `ctx.Done()`.
9. Exit translation from `container.WaitResponse`:
   - `wr.Error` channel fired ⇒ return that error (wait failed server-side).
   - `WaitResponse.Error != nil` (a `*WaitExitError`) ⇒ wrap as a plain error, **not** an `ExitError`.
   - else `StatusCode==0` ⇒ nil; non-zero ⇒ `&docker.ExitError{Code:int(StatusCode)}`.
- `--rm`: `AutoRemove` reaps on normal exit. A **deferred best-effort**
  `ContainerRemove(ctx, id, ContainerRemoveOptions{Force:true})` guarded by
  `!startedCleanly || ctx.Err() != nil` ensures a pre-start abort / start failure
  / ctx cancel does not leak a container.

### Exit-code contract (Task 3)
- New exported `docker.ExitError struct{ Code int }` implementing `error`
  (`Error() string` like `"container exited with code N"`).
- `runWithExitCode` repointed: `var ee *docker.ExitError; if errors.As(err, &ee)
  { return ee.Code }`. The `syscall.WaitStatus`/`exec.ExitError` branch and its
  imports are **removed**.
- ⚠️ **Semantics change, made explicit:** the old branch derived `128+signum`
  from the OS `WaitStatus` of the forked `docker` process. We no longer fork, so
  signal-killed semantics now depend on the **daemon** — which reports
  `128+signum` in `StatusCode` (e.g. SIGKILL ⇒ 137), passed through verbatim.
  `TestRunWithExitCode_SignalKilledMapsTo128PlusSignum` becomes a **pure mapping
  test** (fake `StatusCode 137` ⇒ exit 137); rename to reflect that it no longer
  exercises OS `WaitStatus.Signaled()`, and drop the `SkipNonPOSIX`/Unix-only
  rationale that justified the old branch.
- `errSilent`, the generic `"makeslop: %v"` path, and `ErrNoTTY` handling in
  `runGo` are unchanged.

### Build orchestration (Task 4) — BuildKit via session
1. `s, _ := session.NewSession(ctx, "makeslop")`.
2. `s.Allow(filesync.NewFSSyncProvider(...))` for the **context** dir
   (`opts.ContextDir`, still the empty temp dir — the Dockerfile does not `COPY`
   from context) and the **dockerfile** dir (`dir(opts.DockerfilePath)`);
   `s.Allow(authprovider.NewDockerAuthProvider(...))` for registry pulls.
3. `go s.Run(ctx, dialer)` where `dialer` is an **adapter closure** —
   `session.Dialer` is `func(ctx, proto string, meta) (net.Conn, error)` but
   `cli.DialHijack` takes an extra leading `url`, so:
   `dialer := func(ctx, proto, meta) { return cli.DialHijack(ctx, "/session", proto, meta) }`
   (`cli.DialHijack` is **not** directly assignable to `session.Dialer`).
4. `ImageBuild(emptyTar, ImageBuildOptions{ Version: build.BuilderBuildKit,
   SessionID: s.ID(), RemoteContext: "client-session",
   Dockerfile: base(opts.DockerfilePath), Tags: []string{opts.Image},
   BuildArgs: parseBuildArgs(opts.BuildArgs), NoCache: opts.NoCache })`.
5. **Render progress (two sub-steps — `resp.Body` is NOT a `SolveStatus` channel):**
   - a. Decode `resp.Body` as the daemon's JSON build-trace stream; for each
     `jsonmessage` frame, parse the buildkit `aux` payload
     (`moby.buildkit.trace`, a base64 `controlapi.StatusResponse`) and convert
     it into `*client.SolveStatus`, pushing onto a `chan *client.SolveStatus`.
   - b. Feed that channel to `progressui` (`progressui.NewDisplay(out, mode)` →
     `Display.UpdateFrom(ctx, ch)`), auto-selecting TTY vs plain mode.
   - Then close the session.
   - ⚠️ This decode is the non-trivial heart of the build migration; it gets its
     own checkbox. **Fallback if the decode proves disproportionate:** copy
     `resp.Body` through `jsonmessage.DisplayJSONMessagesStream` (no
     `progressui`, drops the `[+] Building` UI). The session + cache mounts work
     either way; only the rendering differs.
- `DOCKER_BUILDKIT=1` env disappears (BuildKit selected by `Version:"2"`).
- `Build` stays TTY-free / CI-pipe-safe (no `ttyCheck`).
- `BuildArgv` (pure argv builder) is no longer used by `Build`; keep it only if a
  test/dry-run references it, otherwise remove with its tests.

## What Goes Where
- **Implementation Steps** (checkboxes): all code, tests, and in-repo docs below.
- **Post-Completion** (no checkboxes): live-daemon manual verification of
  `makeslop go` and `makeslop build`, and running the gated integration test.

## Implementation Steps

### Task 1: Add the apiClient interface and default client constructor

**Files:**
- Create: `internal/docker/client.go`
- Create: `internal/docker/client_test.go`

- [x] define unexported `apiClient` interface in `client.go` with exactly the
      methods used: `ContainerCreate`, `ContainerAttach`, `ContainerStart`,
      `ContainerWait`, `ContainerResize`, `ContainerRemove`, `ImageBuild`,
      `DialHijack`, `Close`. **Copy the literal options/result-struct signatures
      from `moby/moby/client@v0.4.1` `client_interfaces.go`** (e.g. `ContainerWait`
      returns a single `ContainerWaitResult` and no error) — guessed positional
      signatures will fail the assertion below.
- [x] add `func newClient() (apiClient, error)` using
      `client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())`.
- [x] add `var newClientFn = newClient` (swap point for tests) and a compile-time
      assertion `var _ apiClient = (*client.Client)(nil)`.
- [x] write a test asserting `*client.Client` satisfies `apiClient` (compile-time
      assertion is the test; add a trivial test that `newClient` returns non-nil
      when `DOCKER_HOST` is unset or pointed at a dummy, without dialing).
- [x] run `go build ./...` and tests — package still compiles (run.go unchanged), must pass before Task 2.

### Task 2: Add pure Spec → SDK-struct projections

**Files:**
- Modify: `internal/docker/spec.go`
- Modify: `internal/docker/spec_test.go`

- [ ] add pure `func (s Spec) ContainerConfig() *container.Config` per Technical Details.
- [ ] add pure `func (s Spec) HostConfig() *container.HostConfig`, including
      `tmpfsMap` (split on first `:`) and `mountsFor` helpers.
- [ ] keep `Args()`/`ShellCommand()`/`BuildSpec` unchanged (used by `--dry-run`).
- [ ] refresh the `spec.go` package doc comment to note the pure SDK-struct
      projections (still pure — no fs/exec — so the CLAUDE.md contract holds).
- [ ] write table tests for `ContainerConfig()` (image/cmd/env/tty/stdin flags).
- [ ] write table tests for `HostConfig()` (AutoRemove, CapDrop, SecurityOpt,
      NetworkMode incl. empty default, tmpfs map split incl. no-`:` case, bind +
      tmpfs + `/dev/null` mount translation, ReadOnly propagation, proxy-socket case).
- [ ] write the **drift-guard** test: one representative `Spec` (proxy + masked
      files/dirs), assert `Args()`/`ShellCommand()` and the projections agree on
      image, cmd, workdir, env, mounts, caps, secopt, network.
- [ ] run tests — must pass before Task 3.

### Task 3: Migrate Run to the SDK + introduce docker.ExitError + client test seam

**Files:**
- Modify: `internal/docker/run.go`
- Modify: `internal/docker/testing.go`
- Modify: `internal/docker/run_test.go`
- Modify: `cmd/makeslop/main.go`
- Modify: `cmd/makeslop/main_test.go`

- [ ] add exported `type ExitError struct{ Code int }` with `Error()` in `run.go` (or `errors.go`).
- [ ] rewrite `Run(ctx, s Spec)`: keep `ErrNoTTY` guard; obtain client via
      `newClientFn()`; orchestrate create→attach→`term.MakeRaw`→start→initial
      resize + SIGWINCH→stdin/stdout pumps→`ContainerWait`; map non-zero
      `StatusCode` → `&ExitError{Code}`; deferred best-effort force-remove guarded
      by `startedCleanly`/ctx-cancel; `defer client.Close()`.
- [ ] add internal `run(ctx, cli apiClient, s Spec)` so tests inject a fake; the
      exported `Run` is a thin wrapper that builds the default client.
- [ ] in `testing.go`: add `SetClientForTest(c apiClient) (restore func())`
      swapping `newClientFn`. **Leave ALL build-side shim machinery untouched in
      this task** — `WriteShim`, `WriteBuildShim`, `SetDockerBinaryForTest`,
      `dockerBinary`, and `executableTempDir` all stay, because the exec-based
      `Build` and its `run_test.go`/`main_test.go` build tests still use them
      until Task 4. (Removing any of these now breaks the Task 3 green boundary.)
- [ ] add a fake `apiClient` test helper (records calls; scripted
      `ContainerAttach` stream + `ContainerWait` result/error; toggleable
      `ContainerStart` error).
- [ ] rewrite the **run** tests in `run_test.go` using the fake: clean exit (0,
      no explicit remove), non-zero ⇒ `*docker.ExitError` with `Code`, `ttyCheck`
      false ⇒ `ErrNoTTY` before any client call, start-failure ⇒ force-remove
      fires, ctx cancel ⇒ no leak; `SkipNonPOSIX` on raw-mode/SIGWINCH cases.
      Remove `executableTempDir` if now unused.
- [ ] repoint `runWithExitCode` in `main.go` to `errors.As(err, &*docker.ExitError)`
      → `return ee.Code`; delete the `exec.ExitError`/`syscall.WaitStatus` branch
      and now-unused imports; update the doc comment.
- [ ] update `cmd/makeslop/main_test.go` `go`-command tests to inject a fake via
      `docker.SetClientForTest` instead of `WriteShim`/`SetDockerBinaryForTest`;
      rework `TestGo_ExitCodePropagation` (fake returns StatusCode 42 ⇒ exit 42)
      and rename `TestRunWithExitCode_SignalKilledMapsTo128PlusSignum` to a pure
      mapping test (fake returns StatusCode 137 ⇒ exit 137; drop the
      `SkipNonPOSIX`/`WaitStatus` rationale — no OS wait status anymore).
- [ ] note: only the **`go`-command** main tests migrate here; build-command
      main tests stay on shims until Task 4.
- [ ] run `GOTMPDIR=/home/user go test ./...` — must pass before Task 4.

### Task 4: Migrate Build to BuildKit via session + progressui

**Files:**
- Create: `internal/docker/build.go`
- Create: `internal/docker/build_test.go`
- Create: `internal/docker/build_integration_test.go`
- Modify: `internal/docker/run.go` (remove old exec-based `Build`)
- Modify: `internal/docker/testing.go` (retire `WriteBuildShim`)
- Modify: `internal/docker/run_test.go` / build tests (remove old build shim tests)
- Modify: `go.mod` / `go.sum`

- [ ] add `github.com/moby/buildkit` dep, **pinned to v0.30.0** (`go get
      github.com/moby/buildkit@v0.30.0`); confirm `session`, `session/filesync`,
      `session/auth/authprovider`, `client`, `util/progress/progressui` resolve.
- [ ] run `go mod tidy` and **verify it does not force-upgrade**
      `github.com/moby/moby/client` (v0.4.1) or `.../api` (v1.54.2); if it does,
      add `replace`/version constraints and re-verify the build. (⚠️ blocker if unresolvable.)
- [ ] add pure helpers in `build.go`: `parseBuildArgs([]string) map[string]*string`
      and `buildImageOptions(o BuildOptions, sessionID string) ImageBuildOptions`
      (sets `Version`, `RemoteContext`, `Dockerfile=base(path)`, `Tags`, `NoCache`).
- [ ] add the **session dialer adapter** closure wrapping `cli.DialHijack` with
      the `"/session"` url so it matches `session.Dialer`'s
      `func(ctx, proto, meta)` shape.
- [ ] implement the **build-trace → `*client.SolveStatus` decoder**: read
      `resp.Body` as a jsonmessage stream, parse `aux` `moby.buildkit.trace`
      (base64 `controlapi.StatusResponse`) into `*client.SolveStatus` on a channel.
- [ ] implement `Build(ctx, o BuildOptions, stdout, stderr io.Writer)`: build
      default client; create session with filesync (context dir + dockerfile dir)
      and authprovider; `go s.Run(ctx, dialer)`; call `ImageBuild` with
      `buildImageOptions`; feed the decoded `SolveStatus` channel to
      `progressui.NewDisplay(...).UpdateFrom`; close session/client. Keep the
      empty-context-temp-dir creation/cleanup behavior. No `ttyCheck`.
      (Fallback: `jsonmessage.DisplayJSONMessagesStream` if the decoder proves disproportionate.)
- [ ] add internal `build(ctx, cli apiClient, ...)` seam mirroring Task 3 (for the
      pure-options assertions; full flow stays integration-gated).
- [ ] remove the old exec-based `Build` and `DOCKER_BUILDKIT` env handling from `run.go`.
- [ ] retire ALL remaining shim machinery now that nothing execs docker:
      `WriteShim`, `WriteBuildShim`, `SetDockerBinaryForTest`, the `dockerBinary`
      global, and `executableTempDir`; remove the now-dead shim-based build tests
      in `run_test.go` and migrate the build-command tests in `main_test.go` to
      the fake/seam (or mark integration-gated).
- [ ] write pure unit tests: `parseBuildArgs` (KEY=VAL, KEY-only ⇒ nil-ish,
      empty), `buildImageOptions` mapping (Version/SessionID injected/RemoteContext/
      Dockerfile basename/Tags/NoCache), and context/dockerfile sync-dir selection.
- [ ] write `build_integration_test.go` behind `//go:build integration` + a
      `MAKESLOP_DOCKER_IT` env guard (skip otherwise): real `Build` against the
      daemon produces the tagged image; document
      `MAKESLOP_DOCKER_IT=1 go test -tags integration ./internal/docker/`.
- [ ] update `cmd/makeslop/main_test.go` build-command tests (if any used shims)
      to the fake/seam or mark them integration-gated.
- [ ] run `go test ./...` (non-integration) — must pass before Task 5.

### Task 5: Final dead-symbol sweep and dependency verification

**Files:**
- Modify: `internal/docker/testing.go` (only if stragglers remain)
- Modify: `go.mod` / `go.sum`

- [ ] grep the whole tree for `WriteShim`, `WriteBuildShim`, `SetDockerBinaryForTest`,
      `dockerBinary`, `executableTempDir`, `GOTMPDIR`, `DOCKER_BUILDKIT` — confirm
      all are gone (retired in Tasks 3-4); keep `ttyCheck`, `SetTTYCheckForTest`,
      `SkipNonPOSIX`, and the new `SetClientForTest`/fake.
- [ ] re-run `go mod tidy`; verify `moby/moby/client` + `moby/moby/api` are now
      **direct** and the `moby/buildkit` pin (+ transitive) is recorded, with the
      client/api versions unchanged from Task 4.
- [ ] run `go build ./...`, `gofmt -l`, and `go vet ./...` — all clean.
- [ ] run `go test ./...` — must pass before Task 6.

### Task 6: Verify acceptance criteria
- [ ] verify each Overview behavior is preserved: `--rm`, `-it` (raw + resize),
      cap-drop ALL, no-new-privileges, network none w/ proxy, tmpfs `/tmp`,
      masked file (`/dev/null`) + masked dir (tmpfs) overlays, `--dry-run`
      output byte-identical to before, exit-code propagation incl. 137.
- [ ] verify `docker` binary is no longer referenced anywhere (grep `"docker"`
      exec/literal usages; confirm only image/flag strings remain).
- [ ] run full suite: `GOTMPDIR=/home/user go test ./...`.
- [ ] run gated integration build: `MAKESLOP_DOCKER_IT=1 go test -tags integration ./internal/docker/` (requires a live daemon).
- [ ] confirm `make build` produces a working binary.

### Task 7: [Final] Update documentation
- [ ] update `CLAUDE.md`: remove/rewrite the obsolete shim / `GOTMPDIR` / noexec
      sections (docker package); document the `apiClient` seam +
      `SetClientForTest`, the `docker.ExitError` exit-code contract, and the
      BuildKit-session + `progressui` build flow + the `moby/buildkit` dep.
      Keep the MigrationVersion-on-Dockerfile-change rule and POSIX-only invariant
      (Dockerfile unchanged); keep TTY-is-`go`-only and home-dir guard exemptions.
- [ ] update `README.md` if it mentions a docker-CLI prerequisite (now: a docker
      **daemon**/socket, not the CLI binary).
- [ ] move this plan to `docs/plans/completed/`.

## Post-Completion
*Items requiring manual intervention or external systems — informational only*

**Manual verification** (needs a live docker daemon):
- `makeslop go` in a registered workspace: confirm interactive shell, terminal
  resize, Ctrl-C, and non-zero exit-code propagation behave as before.
- `makeslop go --dry-run`: confirm output is byte-identical to the pre-migration command.
- `makeslop build` (and `--no-cache`, `--build-arg`): confirm BuildKit cache
  mounts still hit and `progressui` output renders in both TTY and piped modes.
- Run the gated integration test against the daemon:
  `MAKESLOP_DOCKER_IT=1 go test -tags integration ./internal/docker/`.

**External system updates** (if applicable):
- CI images/runners must expose a docker **daemon socket** (`DOCKER_HOST`/
  `/var/run/docker.sock`); the `docker` CLI binary is no longer required but the
  daemon is. Update CI docs/provisioning accordingly.
