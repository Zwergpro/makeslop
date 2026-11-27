# Launch refactor: one project layout, one create request

**Scope:** An internal, behavior-preserving refactor of `makeslop run` and `makeslop bg run`. Based on the [architecture review](../../architecture-review.md). [Implementation plan](../plans/2026-11-27-launch-refactor.md).

## Why change it?

Today a cache rule checks a container path derived from the project name, and Docker independently derives the path it mounts. These paths must never disagree. Likewise, `Spec.ForBackground` decorates a foreground spec and the dry-run and SDK renderers separately infer what an empty UUID means. The change is successful when a maintainer can change a root or a background create value in **one place**, with a test that detects disagreement at the actual mount or create request.

## Decisions and alternatives

- **CLI owns the workflow.** Keep `prepareLaunch` and `prepareLaunchExecution`, already used by both commands. Do not add `internal/launch`: it would need to pass image errors, quietable notices, and mandatory warnings back to the very package that already handles them. Image selection remains shared with `status`; the home guard remains shared with `init`.
- **Cache owns validation and managed directory creation, but not their timing.** Keeping all cache code in CLI would save a package, but leave filesystem policy coupled to command tests. `internal/cache` takes resolved values, returns a plan, and imports neither Cobra nor CLI. Docker never imports cache.
- **Docker owns final create values, not another launch workflow.** Both builders use common mount/security/network construction and produce different spec types. Do not keep a foreground spec inside a background one or introduce a common runner: the creation data is shared; foreground wait and background readiness are not.

## Launch contract

| Phase | Required behavior |
| --- | --- |
| Resolve identity | Check the home guard, load settings once, resolve `--image` before looking up registration, and use the same settings revision for the shell and workspace lookup. Use the registered main root even when invoked from a subdirectory. Validate joined paths before checking the daemon; preserve flag order. |
| Plan locally | On real runs check the daemon **before** parsing project YAML or scanning. Use `LoadExisting` for joins; their excludes apply, not their cache, network, or environment settings. CLI resolves host environment, scan results, policy flags, and source locations. Build the selected spec without I/O. |
| Dry-run return | Still validate configuration and static sources. Print the selected mode to stdout; no daemon call, TTY check, cache write, or execution. Keep quoting, joined preview sections, and the background example-UUID banner. Sections are display-only. |
| Execution preflight | For `bg run --attach` check the TTY now; foreground keeps its terminal check in `Docker.Run`. Check image, then the configured network(s) or container target if inspection is required. Missing image and failed inspection retain distinct diagnostics. Keep the existing 10-second Docker preflight bounds; pass `cmd.Context()` to the runner, not a preflight context. |
| Write and run | Only after success may CLI call `Plan.Prepare`. A nonzero foreground exit status propagates. A detached background launch prints its UUID to stdout only after readiness. With `--attach`, print it to stderr **before** `AttachBG`, even under `--quiet`. An attach failure does not roll back a successful launch. |

Scan warnings are emitted when encountered, not buffered until a plan succeeds. If a matching symlink is followed by a cache error, stderr must contain both, in order, even under `--quiet`; report the error once. A failed `docker.New()` still leaves non-Docker commands usable. No changes to flags, configuration, defaults, image pulling/building, network inspection policy, or output formats.

## One layout for cache and Docker

CLI pairs each project's registered/resolved host root with its container root (`/workspace/<name>` in production). The layout has one writable main project and ordered joins. Keep the root and that project's read-only flag, masks, policy gates and display label on the same project record, not in parallel slices that could drift. The cache resolver reads root values only; it never re-derives them from the name. A relative static source is relative to the main **host** root; a relative static destination is relative to the main **container** root. Fixed protected locations such as `/workspace` and `/tmp` remain fixed.

`cache.Resolve` takes the cache declarations, source locations, layout, and the main project's **declared** excludes, including missing paths. It may stat sources but cannot create them. Absent cache configuration yields no mounts. Preserve shared/local/static order, modes, directory trailing slashes, static-file vs. directory behavior, and existing overlap checks. Protect joined roots, policy paths, and excludes; do not ban all children of the main root.

`Plan.Mounts()` returns a copy. `Plan.Prepare(ctx)` first rechecks **every** managed directory, then creates components with the existing symlink and containment checks. If the second declaration is invalid at recheck time, the first is not created. This is not a transaction: an operational failure during creation may leave an earlier empty directory. Static sources are never created. Use `filepath` for host paths and `path` for container paths.

## One create representation, two execution contracts

The pure builders produce `ForegroundSpec` or `BackgroundSpec`. Their private create data contains the final image, workdir, name, ordered labels, argv, environment, mounts, tmpfs, security, network, attachment settings and auto-removal choice. A runner cannot accept the other mode's type; a zero-value spec must fail before SDK creation. Use explicit bind, tmpfs and volume kinds; reject unknown kinds in construction. Preserve the order main bind → main policy → caches → main masks → each join's bind, applicable policy and masks. A mask must follow the mount it covers.

| Create value | Foreground | Background |
| --- | --- | --- |
| Name and labels | Unnamed; no background labels | `makeslop-<canonical-v4-uuid>`; `io.makeslop.bg.uuid`, `.project`, `.shell` (all with the `io.makeslop.bg` prefix), in that order |
| Command | `[shell]` | `["zmx", "attach", "agent", shell]` |
| Environment | Resolved project entries unchanged | Keep non-reserved entries in order; append exactly `ZMX_DIR=/tmp/makeslop-zmx`, `ZMX_DIR_MODE=0700`, `ZMX_SESSION=`, `ZMX_SESSION_PREFIX=` using separately owned storage |
| Terminal | `Tty` and `OpenStdin` true; all three `Attach*` true | `Tty` and `OpenStdin` true; all three `Attach*` false |
| Removal | `AutoRemove` true (`--rm -it`) | `AutoRemove` false (`-dit`) |

`ShellCommand()` and the SDK create request project those values; neither chooses mode based on a missing UUID, filters environment, or constructs the zmx command. The background shell label is the configured shell itself. Keep the foreground wait-before-start and remove-before-drain behavior; keep background readiness, failed-start cleanup, and stream-close-before-exit-inspection behavior. Terminal helper extraction and workspace registration changes are out of scope.

## Acceptance

1. In a fixture using `/src/app` and `/src/lib`, cache checks and Docker binds use those exact roots. No new CLI option exposes custom roots.
2. A missing declared exclude still blocks an overlapping cache; replacing a later managed component with a symlink between resolution and preparation leaves earlier directories absent.
3. A dry run of either mode with a failing Docker/TTY probe still previews without writes. Failed real preflight leaves managed directories absent. A warning survives a later error.
4. Both previews agree with captured SDK create options on mode-specific values. Mutating the input environment after background construction does not alter the result.
5. Existing CLI, cache, renderer and lifecycle regressions pass without a live daemon. Keep stateful SDK fakes for streams and waits. If an interface mock is generated, use **Uber GoMock**; this refactor does not require a mock migration.
