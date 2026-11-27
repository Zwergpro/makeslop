# Architecture review

## Summary

The architecture has a sound foundation. Package dependencies are mostly clear, container specification construction is separated from side effects, and tests exercise behavior without requiring a live Docker daemon. There are no critical architectural problems.

The main maintenance risk is concentration of complexity: several abstractions combine execution modes or policies, so a change can require understanding and updating multiple code paths. The highest-value improvements are small shared terminal lifecycle helpers and explicit background creation settings. The project does not need Clean Architecture, a dependency-injection framework, or more interfaces globally.

### Rating scale

Ratings describe maintenance severity, not code quality in general:

- **1–3:** minor; improve when touching the area.
- **4–6:** moderate; plan a focused refactor.
- **7–8:** high; prioritize because changes can easily introduce divergence.
- **9–10:** critical; the architecture blocks safe maintenance. No findings received this rating.

## Findings at a glance

| Rating | Weak spot | Primary consequence |
| ---: | --- | --- |
| **8** | Duplicated TTY streaming and cleanup | Concurrent lifecycle fixes can diverge |
| **7** | `Spec` implicitly represents foreground and background modes | Invalid states and scattered mode checks |
| **6** | Broad Docker dependencies passed to every command | Unnecessary coupling and oversized fakes |
| **6** | Application/cache planning concentrated in `internal/cli` | The Cobra package owns too many policies |
| **5** | Workspace persistence API has inconsistent ownership | Callers must understand loading and locking details |
| **5** | Stringly typed mount kinds with permissive fallback | Invalid values silently become bind mounts |
| **4** | `projectconfig.go` combines several configuration domains | Navigation and change isolation suffer |
| **3** | Architecture documentation trails implementation | Background architecture must be inferred from code |

## 1. TTY lifecycle is implemented twice — 8/10

### Evidence

- Foreground execution: `internal/docker/run.go:104-260`
- Background exec/attach: `internal/docker/bg_exec.go:76-216`

Both implementations manage:

- Raw terminal setup and restoration.
- `SIGWINCH` handling and resize goroutines.
- Bidirectional `io.Copy`.
- Input closure and goroutine joining.
- Context cancellation and stream teardown.

There is already visible divergence: background code uses injected `d.getTTYSize` at `internal/docker/bg_exec.go:117`, while foreground code directly calls `term.GetSize` at `internal/docker/run.go:184` and `internal/docker/run.go:199`.

Cleanup ordering and goroutine lifetime are subtle. A future cancellation, resource-leak, or terminal-restoration fix can easily be applied to only one path.

### Narrow improvement

Extract small helpers only for behavior with the same lifecycle in both paths:

1. Scoped raw-mode setup and restoration.
2. Resize subscription and shutdown.
3. Stdin copying and `CloseWrite`, using separate writer and EOF-function arguments.

Keep completion orchestration in `Run` and `runExecTTY`. Foreground must register and consume `ContainerWait` concurrently with output, including force-removal before draining on wait transport errors. Background exec must drain and close its stream before starting bounded exec inspection. Do not introduce a shared wait callback or a common output-drain policy.

Keep `pollableStdin` as the ownership record and leave conditional close/join decisions visible at each call site. Fallback `os.Stdin` must never be closed, and callers must not wait on a goroutine blocked in an unowned reader. Preserve the documented exception for an uncloseable stdin reader rather than claiming that every goroutine can always be joined.

## 2. `Spec` has implicit, runtime-checked execution modes — 7/10

### Evidence

- Mode-related fields: `internal/docker/spec.go:77-96`
- Foreground/background argument rendering: `internal/docker/spec.go:209-272`
- SDK configuration checks `bgUUID == ""`: `internal/docker/spec.go:360-395`
- Background conversion: `internal/docker/bg_lifecycle.go:22-40`

`ForBackground` changes hidden identity fields, exported `ZMXArgv`, environment variables, labels, attachment behavior, and auto-removal. Several methods independently infer the mode from whether `bgUUID` is empty.

Consequences:

- `RunBG` must validate at runtime that `ForBackground` was called.
- A background `Spec` can accidentally be passed to foreground `Run`.
- Adding another execution mode would scatter more conditionals through renderers.
- Background/zmx concerns leak into an otherwise general container specification.

### Narrow improvement

Retain the shared base `Spec`, but introduce a distinct wrapper:

- `Run(ctx, Spec)` for foreground execution.
- `RunBG(ctx, BackgroundSpec)` for persistent execution.
- `BackgroundSpec` explicitly contains the base spec, UUID, and project.

Resolve every mode-dependent fact during `Background()` conversion: container name, stably ordered labels, final argv, an owned and freshly filtered environment, attachment behavior, and auto-removal. Store those final values rather than prefixes or fallback instructions. Use the same stored values for shell rendering and the complete SDK create request, including `Name`, `Config`, `HostConfig`, and `NetworkingConfig`. Share plain mount/network rendering, but do not independently reconstruct background policy in different projections.

Validate the background spec again immediately before creation because an exported zero value can bypass a validating constructor. Changing the foreground environment after conversion must not change the background’s mode-dependent values. This is deliberately not a promise to freeze every base-spec slice: networks, tmpfs, capabilities, security options, and presentation sections remain shared and read-only by convention. Keep SDK create-options projection private to `internal/docker` so callers cannot mutate the owned argv or environment through a returned config. Test `ShellCommand` as well as `Args` against options captured at `ContainerCreate`.

Also consider making the main project and joins explicit in `BuildSpec`. Currently `Options.Projects[0]`, ignored `ReadOnly` on the main project, and other invariants exist primarily as comments.

## 3. Small interfaces are composed, but broad dependencies are still consumed — 6/10

### Evidence

- Individual capability interfaces: `internal/cli/deps.go:11-26`
- `backgroundRunner` adds nine methods: `internal/cli/deps.go:28-39`
- `dockerAPI` aggregates fourteen methods: `internal/cli/deps.go:41-49`
- Every command receives the same `dockerDeps`.
- The construction-error stub implements every operation: `internal/cli/deps.go:113-143`
- The general CLI fake contains unrelated no-op background methods: `internal/cli/main_test.go:60-84`

The small interfaces do not provide their full benefit because constructors rarely declare which capabilities they actually consume. Adding a background operation can therefore affect foreground/status test scaffolding.

### Narrow improvement

Keep the composed interface at the application root, but inject narrower contracts into constructors:

- Status needs daemon/image/network checks, not container lifecycle operations.
- Background list needs only `ListBG`.
- Background target commands need one operation each.
- Both launch commands need the shared daemon/image/network preflight contract used by preparation.
- Background run additionally needs `CheckTTY`, `RunBG`, and `AttachBG`; its `--attach` path cannot be split away from the recovery-ID behavior.
- Foreground run additionally needs `Run`.

No dependency-injection library is needed; constructor parameters and small consumer-side interfaces are sufficient. Focused constructor tests can use narrow fakes, while full-command-tree integration tests keep the broader fake needed by root composition.

## 4. `internal/cli` owns application policy as well as command presentation — 6/10

### Evidence

- `prepareLaunch` performs settings loading, workspace lookup, joins, preflight, project loading, scanning, cache resolution, and spec construction: `internal/cli/run.go:156-251`.
- Cache planning, filesystem preparation, and destination policy occupy `internal/cli/cache.go`, `internal/cli/cache_dirs.go`, and `internal/cli/cache_targets.go`.
- `runStatus` is another large orchestration path: `internal/cli/status.go:137-258`.

This makes the Cobra package the place maintainers must visit for almost every behavior change. It also encourages large full-command tests—`internal/cli/run_test.go` is over 3,200 lines—because fewer application policies have independent APIs.

### Narrow improvement

Extract the cache subsystem first into `internal/cache`; it is a cohesive policy with a narrow boundary. Reject an empty project list explicitly because resolution and conflict checks require a main project. Place this guard at the CLI-facing extraction entry before computing paths or evaluating `projects[0].Name`; a guard inside the lower-level resolver alone would run too late. Keep the shared foreground/background launch phases visible in `internal/cli` for now:

1. Resolve settings, image, workspace, and join paths.
2. On real runs, check the daemon before parsing project YAML or scanning.
3. Load projects, resolve cache mounts, and build the spec.
4. Check image and network.
5. Revalidate every managed cache directory before the first write, then create directories.

Dry runs continue to validate local sources while skipping Docker operations and directory creation. The `bg run --attach` TTY check remains between dry-run handling and execution preflight.

Keep diagnostics at their current reporting boundary. Do not return success-only notices that disappear when a later phase fails. Security warnings must retain their `--quiet` bypass, and tailored errors must still be printed exactly once.

Consider a broader `internal/launch` package only after these phases and its diagnostic contract can remain explicit. This does not require separate domain, service, repository, and transport layers.

## 5. Workspace/configuration ownership is inconsistent — 5/10

### Evidence

- Persistence types live in `config`: `internal/config/config.go:23-35`.
- `workspace.Lookup` and `List` accept mutable `*config.Settings`: `internal/workspace/workspace.go:26-76`.
- `Init` and `Remove` instead load and lock settings internally: `internal/workspace/workspace.go:79-134`.
- `runInit` registers, reloads settings, then looks up the registration again: `internal/cli/init.go:28-39`.

Callers must know which methods operate on a supplied snapshot and which perform locked storage operations. The nil-settings behavior adds another implicit contract.

### Narrow improvement

Preserve caller-supplied settings for `Lookup` and `List`. This is a consistency boundary, not merely a way to avoid reads: image, shell, tmpfs, and workspace registration must come from the same settings revision. `status` also uses that read to distinguish unreadable settings from an unregistered workspace while still checking an explicit image.

Change `Init` to return the matched ancestor, cache directory, and image value from the settings already loaded under its lock. This removes `runInit`’s second lookup and reload while preserving its no-image hint. Do not add a snapshot framework or make each workspace method reload settings independently.

## 6. Mount kinds are not closed or validated — 5/10

### Evidence

- `Mount.Type` is a string: `internal/docker/spec.go:70-74`.
- CLI rendering switches on it: `internal/docker/spec.go:249-268`.
- SDK rendering repeats the switch: `internal/docker/spec.go:440-468`.
- Both defaults treat every unrecognized value as bind.

A typo such as `"tmpfs "` silently changes behavior rather than producing an error. Adding a mount kind requires remembering both projections.

### Narrow improvement

Introduce a small `MountKind` type with bind/tmpfs/volume constants. Make the built mount slice private, own its storage, and return a defensive copy for inspection. Validate the completed mount list once at the `BuildSpec` construction boundary, reject missing projects, and return an error for unknown values. Direct literals and post-construction mutation must not bypass the mount-kind boundary. Keep `Args`, `ShellCommand`, and SDK projections free of additional error plumbing; after validation, they handle only the three declared cases.

The main-project requirement belongs to `BuildSpec`, not to every executable `Spec`. `Run`, `RunBG`, and `Background` remain lower-level APIs that can execute projectless specs in focused tests. Private mount storage guarantees valid installed kinds; it does not claim that every spec was assembled by `BuildSpec`.

## 7. `projectconfig.go` is a collection of concerns — 4/10

`internal/projectconfig/projectconfig.go` contains scaffolding, loading, excludes, networking, environment parsing, YAML helpers, stat filtering, and sorting. The central `load` function alone spans approximately `internal/projectconfig/projectconfig.go:185-297`.

The package boundary is reasonable; the problem is navigation within it.

### Low-risk improvement

Split by concern without introducing new packages or abstractions:

- `load.go`
- `scaffold.go`
- `exclude.go`
- `environment.go`
- `network.go`
- `yaml.go`

Apply the same treatment to very large test files when useful, such as separating join, environment, and dry-run tests from `internal/cli/run_test.go`.

## 8. Architecture documentation misses background execution — 3/10

`docs/architecture.md` documents foreground execution and the Docker boundary well, but does not explain:

- `bg_lifecycle.go`, `bg_exec.go`, and `bg_identity.go`.
- UUID/label ownership.
- zmx readiness and attachment.
- The foreground/background `Spec` relationship.

These are major architectural concepts and should not be discoverable only through implementation.

### Improvement

Add a background-execution section to `docs/architecture.md` covering identity, lifecycle, readiness, terminal attachment, and the intended specification boundary. Update the module map when new execution modes or policy packages are introduced.

## Illustrative target architecture

The following sketches show how the recommendations fit together. They are API examples, not drop-in implementations: mechanical error handling and terminal cleanup details are intentionally omitted. Each sketch can be adopted independently.

### Dependency direction

Keep the architecture flat. Extract cache policy first, while leaving the ordered launch workflow visible in `internal/cli`. Do not add an application service merely to move the same complexity to another package.

```text
cmd/makeslop
    └── internal/cli                 Cobra, launch phases, output, exit codes
          ├── internal/cache         Cache resolution and preparation
          │     ├── internal/projectconfig
          │     └── internal/docker  Mount type
          ├── internal/config
          ├── internal/projectconfig
          ├── internal/security
          ├── internal/workspace
          └── internal/docker        Spec and foreground/background execution
```

Docker execution remains behind consumer-side interfaces; there is no global service container.

### Cache planning outside Cobra

`cache.Plan` keeps resolved directories private and returns a defensive copy of mounts. Preparation repeats the complete validation pass before any write.

```go
package cache

type Plan struct {
	mounts []docker.Mount
	dirs   []dir
}

func ResolveProject(
	baseDir string,
	projects []docker.Project,
	cfg projectconfig.Config,
) (Plan, error) {
	if len(projects) == 0 {
		return Plan{}, errors.New("cache: main project required")
	}

	paths, err := projectCachePaths(baseDir, projects[0].Name, cfg.Cache.Static)
	if err != nil {
		return Plan{}, err
	}

	return resolve(cfg.Cache, paths, projects, cfg.Excludes.DeclaredPaths)
}

func resolve(
	cfg projectconfig.CacheMounts,
	paths Paths,
	projects []docker.Project,
	declaredExcludes []string,
) (Plan, error) {
	// Resolve and validate every declaration without creating sources.
}

func (p Plan) Mounts() []docker.Mount {
	return slices.Clone(p.mounts)
}

func (p Plan) Prepare(ctx context.Context) error {
	// Revalidate all dirs first, then create them in configuration order.
}
```

The existing `prepareLaunch` and `prepareLaunchExecution` functions retain their tested ordering. Their responsibilities become clearer without introducing a success-only plan or changing diagnostic timing:

```go
func runRun(cmd *cobra.Command, deps foregroundRunDeps, opts launchOptions) error {
	// prepareLaunch emits diagnostics at the current boundary, including before an error.
	launch, err := prepareLaunch(cmd, opts, deps)
	if err != nil {
		return err
	}

	if opts.DryRun {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), launch.Spec.ShellCommand())

		return err
	}

	if err := prepareLaunchExecution(cmd, launch, deps); err != nil {
		return err
	}

	err = deps.Run(cmd.Context(), launch.Spec)
	if errors.Is(err, docker.ErrNoTTY) {
		fmt.Fprintln(cmd.ErrOrStderr(),
			"makeslop: stdin/stdout must be a TTY — run in an interactive terminal")

		return errSilent
	}

	return err
}
```

The real implementation can keep the current workspace and dependency parameters. The important design choice is that daemon checks, YAML parsing, scanning, network checks, and cache writes do not move across their established boundaries.

### Narrow dependencies at command boundaries

The concrete `*docker.Docker` can satisfy all these interfaces. Each command constructor declares the complete contract it actually uses—no more and no less.

```go
package cli

type statusDeps interface {
	CheckDaemon(context.Context) error
	ImageExists(context.Context, string) (bool, error)
	ContainerRunning(context.Context, string) (exists, running bool, err error)
	NetworkExists(context.Context, string) (bool, error)
}

type backgroundLister interface {
	ListBG(context.Context) ([]docker.BGContainer, error)
}

type launchPreflight interface {
	daemonChecker
	imageChecker
	networkChecker
}

type foregroundRunDeps interface {
	launchPreflight
	Run(context.Context, docker.Spec) error
}

type backgroundRunDeps interface {
	launchPreflight
	CheckTTY() error
	RunBG(context.Context, docker.BackgroundSpec) error
	AttachBG(context.Context, string) error
}

func newStatusCmd(deps statusDeps) *cobra.Command
func newBGListCmd(deps backgroundLister) *cobra.Command
func newRunCmd(deps foregroundRunDeps) *cobra.Command
func newBGRunCmd(deps backgroundRunDeps) *cobra.Command
```

`backgroundRunDeps` includes attachment because `bg run --attach` must publish the UUID on stderr before calling `AttachBG`, including under `--quiet`. Attachment failure must return without hiding that recovery ID.

Composition still happens once in `newRootCmdWithDeps`. Focused constructor tests can use narrow fakes, while full-command-tree integration tests retain their broader fake and continue to verify cross-command behavior.

### Explicit foreground and background specifications

The base `Spec` contains common container configuration. `BackgroundSpec` makes the execution contract type-safe, while a resolved `createMode` supplies the same mode-dependent values to dry-run output and the complete SDK create request.

```go
package docker

type Spec struct {
	Image   string
	Command string
	Workdir string
	Env     []string
	mounts  []Mount

	// Existing common tmpfs, security, network, and presentation fields remain.
}

func (s Spec) Mounts() []Mount {
	return slices.Clone(s.mounts)
}

type BackgroundSpec struct {
	base Spec
	uuid string
	mode createMode
}

type label struct {
	key   string
	value string
}

type createMode struct {
	name       string
	labels     []label // Final values in deterministic display order.
	argv       []string
	env        []string
	attached   bool
	autoRemove bool
}

func (s Spec) Background(uuid, project string) (BackgroundSpec, error) {
	if !validBGUUID(uuid) {
		return BackgroundSpec{}, errors.New("expected a full background UUID")
	}

	env := freshBackgroundEnv(s.Env) // Owned snapshot; no zmx overrides from the base.
	mode := createMode{
		name: bgPrefix + uuid,
		labels: []label{
			{key: bgUUIDLabel, value: uuid},
			{key: bgProjectLabel, value: project},
			{key: bgShellLabel, value: s.Command},
		},
		argv:       []string{"zmx", "attach", bgSession, s.Command},
		env:        env,
		attached:   false,
		autoRemove: false,
	}

	return BackgroundSpec{base: s, uuid: uuid, mode: mode}, nil
}

func foregroundMode(s Spec) createMode {
	return createMode{
		argv:       []string{s.Command},
		env:        s.Env,
		attached:   true,
		autoRemove: true,
	}
}
```

Both projections consume the same resolved mode. The private `createOptions` projection includes the background name, which is not part of `container.Config`, and `HostConfig.AutoRemove`.

```go
func (s Spec) Args() []string {
	return argsFor(s, foregroundMode(s))
}

func (s BackgroundSpec) Args() []string {
	return argsFor(s.base, s.mode)
}

func (s Spec) createOptions() moby.ContainerCreateOptions {
	return createOptionsFor(s, foregroundMode(s))
}

func (s BackgroundSpec) createOptions() moby.ContainerCreateOptions {
	return createOptionsFor(s.base, s.mode)
}

func (d *Docker) Run(ctx context.Context, spec Spec) error
func (d *Docker) RunBG(ctx context.Context, spec BackgroundSpec) error
```

`argsFor` and `createOptionsFor` share mount/network rendering and consume final values; they do not filter environments, prepend zmx arguments, or infer background policy. SDK conversion turns the ordered labels into its required map without changing CLI order. Only `Run` and `RunBG` call the private create-options projection, so its returned configs cannot become a public mutation path for mode-owned argv or environment. `RunBG` calls `spec.validate()` immediately before creation so even a manually constructed zero value cannot reach the daemon.

The snapshot guarantee is deliberately limited to mode-dependent values. Base-spec slices such as `Networks`, `Tmpfs`, `CapDrop`, `SecOpt`, and `Sections` remain shared and read-only by convention; conversion does not claim to freeze the entire launch. Drift tests compare `ShellCommand` and `Args` with options captured at `ContainerCreate`, including name, labels, final argv, owned environment, attachment, and auto-removal. A mutation test proves that later foreground environment changes do not affect the background’s owned environment.

Migrate CLI tests before making the base and SDK projections private. Replace `internal/cli/bg_test.go` assertions over `spec.Command`, `spec.Mounts`, `spec.Networks`, and `spec.ContainerConfig()` with comparisons of public `Args()` mount/network flags, configured shell, and environment. In the same migration, replace the three foreground `internal/cli/run_test.go` uses of `HostConfig()` or `NetworkingConfig()` with assertions for ordered or absent `--network` flags in the executed spec’s `Args()`.

Keep SDK-specific assertions in `internal/docker` against the actual captured `ContainerCreate` request; `TestRun_PassesNetworkingConfig` already covers multiple networks, `container:proxy`, and unset networking there. Do not add `Base()` or public SDK getters just to preserve test access.

### Small terminal helpers, separate completion protocols

Share only terminal operations whose setup and teardown semantics match. There is deliberately no `waitFunc` and no common `Run` method.

```go
package docker

type terminalControl struct {
	fd         int
	makeRaw    func(int) (*term.State, error)
	getTTYSize func(int) (int, int, error)
}

func (t terminalControl) enterRaw() (restore func(), err error) {
	// Enter raw mode and return one idempotent restoration function.
}

func (t terminalControl) watchResize(ctx context.Context, resize resizeFunc) (stop func()) {
	// Subscribe to SIGWINCH, seed the initial size, and join the resize goroutine on stop.
}

func pumpStdin(dst io.Writer, closeWrite func() error, src io.Reader) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(dst, src)
		_ = closeWrite()
	}()

	return done
}
```

Foreground retains its wait registration, streaming, removal, and drain order:

```go
att, err := d.client.ContainerAttach(ctx, id, moby.ContainerAttachOptions{ /* existing options */ })
// Attach failure returns before raw-mode setup or any wait request.
restore, err := d.terminal.enterRaw()
// Raw-mode failure returns before wait. Pollable-stdin fallback is not fatal.
ps := newPollableStdin(d.stdin)
// Failure to obtain a pollable stdin falls back to the original reader and continues.

wr := d.client.ContainerWait(waitCtx, id, moby.ContainerWaitOptions{
	Condition: container.WaitConditionNextExit,
}) // After attach/raw setup, but before ContainerStart.

// Start only after wait registration, then stream output and pump stdin.
stdinDone := pumpStdin(att.Conn, att.CloseWrite, ps.handle)
// On wr.Error: force-remove first, drain output second.
// drainAndJoin closes and joins ps only when it owns a closer.
// On normal exit: drain tail output before returning its status.
```

Background exec retains raw-mode setup before attachment and starts exit inspection only after stream teardown:

```go
restore, err := d.terminal.enterRaw()
// ExecAttach follows successful raw-mode setup.

stdin := newPollableStdin(d.stdin)
inputDone := pumpStdin(stream.Conn, stream.CloseWrite, stdin.handle)
// Drain and close the exec stream, then stop resize handling.
// Close and join stdin only when stdin.closer is non-nil; never close fallback os.Stdin.
// Only after that point inspect the exec status with a bounded context.
return d.execExitStatus(ctx, c.UUID, created.ID)
```

This removes duplicated terminal mechanics without hiding ownership or pretending that container wait and exec inspection are interchangeable. The writer and `closeWrite` function are separate because both current paths write to `Conn` but call `CloseWrite` on the hijacked response.

### Consistent workspace snapshots

`Lookup` and `List` retain caller-supplied settings so related decisions use one settings revision. `Init` returns all values needed by `runInit`, including the image used for its hint.

```go
package workspace

type InitResult struct {
	Root     string
	CacheDir string
	Image    string
}

func (w *Workspaces) Init(pwd string) (InitResult, error)

// These signatures intentionally keep the existing consistency boundary.
func (w *Workspaces) Lookup(s *config.Settings, pwd string) (root, cacheDir string, err error)
func (w *Workspaces) List(s *config.Settings) []Info
func (w *Workspaces) Remove(name string) (cacheDir string, err error)
```

`runInit` no longer reloads settings or repeats ancestor lookup. Other callers do not independently reload between image resolution and workspace lookup, and `status` can still distinguish unreadable settings from an unregistered workspace while checking an explicit image.

### Closed mount kinds

The zero value remains a bind mount. Validation happens once when the spec is constructed, not independently in every renderer.

```go
package docker

type MountKind uint8

const (
	MountBind MountKind = iota
	MountTmpfs
	MountVolume
)

type Mount struct {
	Kind      MountKind
	Host      string
	Container string
	ReadOnly  bool
}

func (m Mount) Validate() error {
	switch m.Kind {
	case MountBind, MountTmpfs, MountVolume:
		return nil
	default:
		return fmt.Errorf("unknown mount kind %d", m.Kind)
	}
}

func BuildSpec(o Options) (Spec, error) {
	if len(o.Projects) == 0 {
		return Spec{}, errors.New("docker: main project required")
	}

	spec := assembleSpec(o) // Pure construction, including generated mounts.
	if err := validateMounts(spec.mounts); err != nil {
		return Spec{}, err
	}

	return spec, nil
}
```

`BuildSpec` owns the completed mount list and validates it before returning. Callers receive copies through `Mounts()`, while renderers use the private slice directly. Outside `internal/docker`, direct literals and post-construction mutation cannot install an invalid mount kind. `Args`, `ShellCommand`, and SDK rendering retain their current signatures and handle only valid kinds. Construction tests cover missing projects and rejected kinds; drift tests continue to compare valid CLI and SDK projections.

Requiring a main project is a builder precondition, not an execution invariant. Lower-level `Run`, `RunBG`, and `Background` APIs continue to accept projectless specs for focused execution tests; no fallible renderers or additional project checks are introduced at those boundaries.

## Existing strengths to preserve

- **Pure `BuildSpec` with resolved inputs** is a strong boundary.
- **Shared dry-run and SDK projections** prevent drift; do not replace them with separate builders.
- `security.Scan` is focused and understandable.
- Configuration defaults, atomic saves, and locked updates are explicit.
- Tests use fakes rather than requiring a live daemon.
- Package names and the overall dependency graph are generally straightforward.

## Recommended order of work

1. Pin terminal lifecycle behavior with tests: attach/raw failure performs no wait request, wait remains before start, no exec inspection occurs before stream EOF, foreground wait-error removal/drain ordering is preserved, and a genuinely blocked unowned stdin reader is released by the test after return.
2. Extract only scoped raw-mode, resize, and stdin copy/EOF helpers; leave reader ownership, joining, and completion orchestration in place.
3. Pin background `ShellCommand`/`Args` parity against captured SDK create options and mode-dependent mutation isolation. While the old API still compiles, migrate background policy assertions and foreground network assertions to `Args()`; keep SDK assertions in `internal/docker`. Then introduce `BackgroundSpec`, its private base/projection, and the changed runner/stub/fake signatures together.
4. Before cache extraction, add the CLI regression where a matching symlink warning survives a later cache failure under `--quiet` and the error is printed once.
5. Extract cache resolution/preparation with an empty-project guard at the CLI-facing entry, before the first project index. Preserve launch phases, diagnostic timing, the batch precheck, and per-component execution-time rechecks.
6. Independently change private mount storage/fallible `BuildSpec`, complete command dependencies, `InitResult`, and file/documentation splits. Preserve the foreground `ErrNoTTY` branch.

Each step requires its existing behavior tests to pass before the next extraction. Do not review the fallible-builder API change alongside workspace locking or file moves. Update `docs/architecture.md` as each boundary changes.
