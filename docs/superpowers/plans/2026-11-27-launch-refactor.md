# Launch refactor implementation plan

> **For agentic workers:** Use `superpowers:executing-plans` for native execution or `superpowers:subagent-driven-development` if the operator chooses delegation. Complete the checkboxes in order; keep both launch modes working at each commit.

**Goal:** Validate and mount the same project roots, and give the dry run and Moby SDK the same final container create values.

**Architecture:** CLI orchestrates; cache plans and prepares; Docker builds mode-specific specs and runs them. No new launch service. **Tech stack:** Go (`go.mod`), Cobra, Moby Go SDK, standard `testing`, existing Docker fakes. **Spec:** [Launch refactor design](../specs/2026-11-27-launch-refactor-design.md).

## Global constraints

- No new flags, settings, preview format, Docker CLI calls, pulls or builds. Preserve error precedence, stdout/stderr, quiet handling and exit codes.
- Builders perform no filesystem, environment or SDK I/O. Cache resolution can read; only execution preparation can create. Joins keep flag order and contribute excludes, not launch configuration.
- Keep the 10-second Docker preflight bounds separate from the runner's context. Do not change stream, terminal, wait, or readiness ownership.
- Use real temporary directories for cache tests and the existing stateful SDK fakes for lifecycle tests. Use Uber GoMock if a generated mock is needed; do not add a generator or dependency without such a test.

## Review focus

| Failure that an ordinary happy path misses | Owned by |
| --- | --- |
| A quiet warning disappears when a later step fails, or a recovery ID appears only after attach | Task 1 |
| Validation uses the project name instead of the resolved root for a relative target, workdir, or mask | Task 2 |
| A missing declared exclude or symlink introduced after resolution bypasses the cache checks | Task 3 |
| A renderer silently treats an unknown mount kind as a bind or accepts a zero-value spec | Task 4 |
| A background preview looks correct but the actual SDK request attaches streams or inherits a reserved `ZMX_` value | Task 4 |

## File map

| Files | Change |
| --- | --- |
| `internal/cli/{run,bg,deps}.go` | Keep both preparation phases; resolve roots once; choose builder, TTY and output at existing command boundaries. |
| `internal/cli/{run_cache,run,bg,main}_test.go` | Pin observable order/output. Update `fakeDocker` and the BG probes as runner types change. |
| `internal/docker/spec.go`, new `internal/docker/create_spec.go` | Resolved layout, builders, final create data, shell and SDK projections. |
| `internal/cli/{cache,cache_targets,cache_dirs}.go` → `internal/cache/{resolve,targets,dirs}.go` | Move rules and filesystem effects. Keep source-location discovery in CLI. Move package policy tests with their owners. |
| `internal/docker/{run,bg_lifecycle}.go` and their tests | Accept distinct specs; leave everything after creation on its current path. |
| `docs/architecture.md`, `CLAUDE.md` | Update shipped architecture and contributor invariants when the implementation lands. |

## Task 1 — Characterize launch order before changing types

**Interface:** existing `newRootCmdWithDeps`, `bgPolicyProbe`, `bgCLIProbe.beforeAttach` and cache fixtures. This commit adds coverage, not production code. Characterization tests should pass against today's tree; an unexpected failure is a behavior discrepancy to investigate, not a reason to change the expectation silently.

- [ ] Run the baseline: `go test -timeout=100s ./...`. Keep `TestRun_DaemonCheckedBeforeYamlParse`, `TestRun_ImageMissing_AbortsWithRemedy`, `TestRun_ImageOtherError_PropagatesError`, and `TestRun_CacheFailureNoCreation` as existing coverage rather than duplicating them.
- [ ] In `internal/cli/run_cache_test.go`, add `TestRun_QuietWarningBeforeCacheError`. Use a symlink matching an exclude pattern and a missing static source. Exercise the exit-code/reporting path or capture its stderr, not just the raw Cobra error: expect nonzero exit, `NOT masked` before `cache.static[0]`, one occurrence of the reported cache error, and no call to `Run`.
- [ ] Extend `TestBGCommands` in `internal/cli/bg_test.go`. With `--attach -n` and a failing TTY probe, assert the example banner, no TTY or Docker calls, and no local cache creation. For a real `--attach --quiet` run with `networks: [devnet]`, record **one ordered trace** across the existing probes: `daemon, tty, image, network:devnet, run, attach`. Check the cache directory at entry to `RunBG` and stderr at entry to `AttachBG` (reuse `beforeAttach`). Fail image inspection in a second subtest: no network, cache creation, runner, or UUID. Assert the Docker preflight receives a deadline and the runner receives no new deadline.
- [ ] Run `go test ./internal/cli -run 'TestRun_QuietWarningBeforeCacheError|TestBGCommands|TestRun_CacheFailureNoCreation'`; expect PASS. Commit: `test(cli): pin launch order and diagnostics`.

## Task 2 — Resolve the layout once

**Interface:** introduce the following values in `internal/docker/spec.go`. Keep `Options`/`BuildSpec` for this commit, replacing `Options.Projects` with `Options.Layout`. Put policy on the same record as its root so a join cannot acquire another project's masks by an index mismatch.

```go
type ProjectRoot struct{ Name, HostRoot, ContainerRoot string }
type Project struct {
    ProjectRoot
    // Existing Label, ReadOnly, MaskedFiles, MaskedDirs, ProtectConfig, MaskGitHooks.
}
type Layout struct {
    Main  Project
    Joins []Project
}
```

- [ ] Add `TestBuildSpec_UsesResolvedRoots` in `internal/docker/spec_test.go` and `TestResolveCache_UsesResolvedRoots` in `internal/cli/cache_test.go`. Give projects names `app`, `lib` but container roots `/src/app`, `/src/lib`. Assert main workdir and actual project bind targets match the supplied roots; assert policy mounts and a file mask land beneath the corresponding root. Accept relative `build` at `/src/app/build`, reject `/src/lib/build`; verify a relative static host source resolves under the main host root. Run `go test ./internal/docker ./internal/cli -run UsesResolvedRoots`; expect a missing-layout compile error or a `/workspace/...` mismatch before implementation.
- [ ] In `prepareLaunch`, construct the main root using `workspaceRoot` and `filepath.Base(workspaceDir)`; pair each resolved join host/name with `path.Join("/workspace", name)` in flag order. Pass through `loadProject`'s policy results without rebuilding the paths. Keep the same settings pointer for `ws.Lookup` and shell/image selection; keep `LoadExisting` for joins.
- [ ] In Docker, replace `Project.containerPath()` reads in workdir, binds, policy mounts and masks with `ContainerRoot`. In CLI cache resolution replace the corresponding main/local/static target and protected-path calculations in `cache.go` and `cache_targets.go`. Leave fixed `/workspace`, `/home/user`, `/tmp`, `/proc`, `/sys`, `/dev` protections intact. Update existing literal fixtures to populate roots explicitly; no compatibility fallback based on `Name`.
- [ ] Run `go test ./internal/docker ./internal/cli`. Check subdirectory launch, joined read-only behavior, join order, and cache/mask precedence still pass. Commit: `refactor: pass resolved project roots to cache and docker`.

## Task 3 — Move the cache plan out of CLI

**Interfaces in `internal/cache/resolve.go`:**

```go
type Sources struct{ BaseDir, WorkspaceDir, HostHome string }
func Resolve(cfg projectconfig.CacheMounts, src Sources, layout docker.Layout, declaredExcludes []string) (Plan, error)
func (p Plan) Mounts() []docker.Mount
func (p Plan) Prepare(ctx context.Context) error
```

- [ ] Write public-contract tests in `internal/cache/resolve_test.go`: mutate one result of `Mounts()` and assert a second is unchanged; resolve a local cache with a missing declared `build` exclude and expect a conflict. In `internal/cache/dirs_test.go`, resolve two missing managed dirs, insert a symlink in the second path, call `Prepare`, and assert error plus `os.IsNotExist` for the first. Cover an already-cancelled context and the empty plan. Run `go test ./internal/cache`; expect missing API before extraction.
- [ ] Move `resolveCache`, target rules and directory logic to the files in the map. Keep `Plan`'s mounts and directory list private. `Resolve` receives `cfg.Excludes.DeclaredPaths` from CLI, never the stat-filtered mask list. Preserve static source validation, declaration names in errors, two-pass preparation, and component checks during creation. Move pure policy cases from `internal/cli/{cache,cache_dirs}_test.go`; keep command fixtures and source-discovery tests with CLI.
- [ ] Keep `projectCachePaths` in CLI, returning `cache.Sources`: resolve the managed base, **not** its children; look up host home only for static sources that use `~`. In `prepareLaunch`, pass `plan.Mounts()` to the current builder. Store `cache.Plan` instead of `[]cacheDir` in `preparedLaunch`; in `prepareLaunchExecution` call `Prepare(cmd.Context())` only after successful image and network checks. Update the cache ownership paragraph in `docs/architecture.md`.
- [ ] Run `go test ./internal/cache ./internal/cli ./internal/docker`; expect PASS, including `TestRun_CacheMissingExcludeConflict`, `TestRun_CacheDryRunNoWrites`, and `TestRun_CacheScanMaskOrder`. Commit: `refactor(cache): separate cache resolution from directory preparation`.

## Task 4 — Build each mode directly and switch both runners

**Interfaces:** replace `Options` with `BuildInputs{Layout, Image, Shell, TmpDirSize, Env, Network, CacheMounts}` (`Network` is the resolved `projectconfig.Network` value). Return `(ForegroundSpec, error)` from `BuildForeground(in BuildInputs)` and `(BackgroundSpec, error)` from `BuildBackground(in BuildInputs, uuid string)`. Each type has `ShellCommand() string` and private create data. Change `Docker.Run(context.Context, ForegroundSpec) error` and `Docker.RunBG(context.Context, BackgroundSpec) error` in the same commit as their callers.

- [ ] Add `TestBuildModes_CreateParity` and `TestBuildModes_InvalidInput` in `internal/docker/spec_test.go`. Use `/bin/sh`, `KEEP=1`, conflicting values for all four reserved zmx keys, and UUID `00000000-0000-4000-8000-000000000000`. Check **each row** of the spec's create table against both renderings; compare label order in the preview and values in the SDK map. Change the original env slice after building and re-check the background values. Both builders must reject an unknown mount kind and missing main root without panic; background must also reject a noncanonical UUID. Run `go test ./internal/docker -run TestBuildModes`; expect the new builders to be missing before implementation.
- [ ] Factor common create data into `createSpec` (`internal/docker/create_spec.go`). Set explicit mount kinds at construction sites, including cache binds; preserve volume and tmpfs projections. `BuildForeground` and `BuildBackground` finalize name, labels, argv, env, attachment and auto-removal. Reuse `validBGUUID`, label constants and `bgZMXEnv`; do not call one public builder from the other. Use `createSpec.createOptions() moby.ContainerCreateOptions` in **both** runners. Render flags from those same final values; keep shell quoting, display sections and flag-shaped image handling independent of SDK conversion.
- [ ] In `internal/cli/run.go`, return `preparedLaunch{Inputs docker.BuildInputs, Cache cache.Plan}` instead of a built foreground spec. Read `Inputs.Image` and `Inputs.Network` for execution preflight. Select `BuildForeground` in `runRun`, `BuildBackground` in `runBG`; use `dryRunBGUUID` only for previews. Propagate builder errors before any cache write. Keep the mode-specific dry-run exits, BG attach TTY placement, and UUID-before-attach output.
- [ ] Update `internal/cli/deps.go` (including `dockerNewErrStub`), `fakeDocker` and `bgCLIProbe` with the new types. Replace CLI tests of private spec fields with preview/output assertions or move create-field assertions to Docker tests; do not export getters just to keep tests compiling. In `internal/docker/{run,bg_lifecycle}_test.go`, assert on `LastContainerCreateOpts` and `bgLifecycleClient.createOptions` from the actual runners. Pass a zero-value spec of each type with a valid test TTY and assert error with zero create calls. Preserve the existing wait-before-start, cleanup, readiness and exec tests.
- [ ] Remove `Spec.ForBackground`, `BuildSpec`, old `Spec`/`Options` and UUID-based renderer branches after the last caller migrates. Update `docs/architecture.md` and the `BuildSpec`/`Options` examples and invariants in `CLAUDE.md`. Run `go test ./internal/docker ./internal/cli ./internal/cache`, then `go test -timeout=100s ./...` and `golangci-lint run` (CI version v2.12.2). Expect passing tests and no lint findings. Commit: `refactor(docker): build typed launch specs from final create values`.

Stop here. Terminal helper sharing, workspace-init changes, replacing fakes en masse and reorganizing unrelated files are not part of this plan.
