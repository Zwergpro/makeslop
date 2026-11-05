# Configurable Cache Mounts Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace fixed agent/content cache mounts with opt-in shared directories, workspace-local directories, and existing-file-or-directory static binds.

**Architecture:** Projectconfig validates declarations; CLI helpers resolve destinations, check filesystem boundaries, and prepare managed directories; Docker receives explicit mounts and performs no filesystem work in BuildSpec. Develop the helpers independently, then switch all existing consumers together so no compatibility implementation remains.

**Tech Stack:** Go 1.26.2, Cobra, yaml.v3, existing moby SDK, standard-library testing/filesystem helpers; no new dependencies.

**Spec:** `docs/superpowers/specs/2026-11-05-configurable-cache-design.md` (approved by the user's request to write this plan).

## Global Constraints

- "Missing config, a missing cache block, or empty groups mean no cache mounts."
- "Create missing shared and local directories; reuse existing directories without modifying their contents."
- "Mount regular files as files and directories as directories; never create missing static sources."
- "Container `~`/`~/...` uses `/home/user`, matching existing makeslop conventions."
- "All cache binds are writable."
- "Static container destinations must be absolute or start with `~`; no implicit relative destinations."
- "Only the main project's cache configuration applies."
- "Keep BuildSpec pure and preserve SDK/dry-run projection parity."
- "No migration, copying, or automatic cleanup."
- "Keep using the Docker SDK; no new dependencies or Docker CLI calls are needed."
- Preserve signal contexts, preflight deadlines, settings locking, image resolution, quiet semantics, and non-Docker command usability. This is a POSIX project; use package-local `skipNonPOSIX` where needed.
- Work in the execution workspace selected through using-git-worktrees. Preserve unrelated changes: `.gitignore` is already staged. Stage only named task files; never use `git add .` or commit somebody else's staged changes. With unrelated index entries present, use `git commit --only -m "<message>" -- <task paths>` for every task commit.
- Tasks 1–3 introduce individually tested helpers, not a second public config format. Task 4 is the atomic public cutover; do not ship the branch before it is complete.

## Review Focus

1. A declared exclude whose host path is missing must still conflict with a cache mount; stat filtering must not erase the declaration (Task 4).
2. Segment boundaries matter: `/opt/tool` and `/opt/toolbox` may coexist; `/opt/tool` and `/opt/tool/data` may not (Task 3).
3. Symlinks at the managed base itself, intermediate components, and dangling final components must not redirect creation outside the workspace (Task 2).
4. A later invalid static entry or a failed image/network preflight must not create earlier managed entries (Tasks 3–4).
5. Spaces, commas, and quotes in valid paths must survive shell display and SDK projections identically; paths are not shell-evaluated (Tasks 3–4).

---

## File structure and interfaces

| File | Responsibility |
| --- | --- |
| New `internal/projectconfig/cache.go`, `cache_test.go` | Cache declaration types, strict raw-node decoder, syntax validation |
| Existing `internal/projectconfig/projectconfig.go`, `projectconfig_test.go` | Wire decoder into Load; preserve declared exclude paths; replace scaffold and defaults |
| New `internal/cli/cache_dirs.go`, `cache_dirs_test.go` | Validate managed-directory containment and prepare directories without overwriting data |
| New `internal/cli/cache.go`, `cache_test.go` | Resolve cache paths, classify static sources, reject destination and exclude conflicts |
| Existing `internal/cli/run.go`, `run_test.go`, `join_test.go` | Resolve cache plan, prepare only on real launch, notice ignored join caches |
| Existing `internal/docker/spec.go`, `spec_test.go` | Consume explicit mounts; remove fixed mounts and boolean options; projection parity |
| Existing `internal/config/config.go`, `config_test.go` | Structural-only bootstrap; remove obsolete seed-file helper |
| Existing `internal/workspace/workspace.go`, `workspace_test.go` | Stop fixed cache scaffolding; preserve registry/removal behavior |
| Existing `internal/cli/init.go`, `init_test.go`, `status_test.go`, `remove_test.go` | Empty scaffold, removed flag, lifecycle regressions |
| Existing `internal/security/security_test.go` | Adapt Scaffold call and fixed-path assumptions |
| `docs/reference.md`, `docs/security.md`, `docs/architecture.md`, `README.md`, `CLAUDE.md` | New behavior, examples, security boundaries, replacement invariants |

Read these existing seams before execution: `projectconfig.load`, `validateEntries`, `statFilter`; `cli.loadProject`, `runRun`, `runInit`; `config.Bootstrap`; `workspace.Init`/`scaffoldTemplate`; `docker.BuildSpec`. Existing CLI tests supply `runCmdWithDeps`, `depsFrom`, `newFakeDocker`, `initWithImage`, `findMount`, and filesystem snapshot helpers. Use those rather than adding a second command harness. Existing Docker tests supply `parseMountArg` and drift guards.

## Task 1: Define and validate cache declarations

**Files:** Create `internal/projectconfig/cache.go` and `internal/projectconfig/cache_test.go`. Do not wire into `Load` yet.

**Interfaces:**
```go
type CacheMounts struct {
    Shared []string
    Local  []string
    Static []CacheBind
}
type CacheBind struct { Host, Container string }
func decodeCache(node *yaml.Node) (CacheMounts, error)
```
Consumes yaml.v3 nodes. Produces normalized managed names and split static declarations without environment reads, source stat calls, or directory creation. These names remain final; Task 4 changes `Config.Cache` to `CacheMounts` and deletes the old `Cache` type.

- [x] **Step 1: Add `TestDecodeCache_Valid` and `TestDecodeCache_Empty`.** Parse test snippets into `yaml.Node`; pass the mapping node directly. Assert with `reflect.DeepEqual`:
  ```go
  // shared: [.claude, .config//tool/]
  // local: [docs]
  // static: ["./settings.json:~/.settings.json"]
  want := CacheMounts{
      Shared: []string{".claude", ".config/tool"}, Local: []string{"docs"},
      Static: []CacheBind{{Host: "./settings.json", Container: "~/.settings.json"}},
  }
  ```
  A zero node, `null`, and `{}` return the zero value; empty lists have zero entries. Preserve list order. Scalar `cache` and null/non-sequence groups fail rather than being coerced into lists.
- [x] **Step 2: Run `go test -timeout=100s ./internal/projectconfig -run '^TestDecodeCache'`.** Expect failure because the new types/decoder do not exist.
- [x] **Step 3: Implement the interfaces in `cache.go`.** Walk raw nodes directly; require string-tagged scalar keys and entries. Reject aliases at every cache level, merge keys, duplicate/unknown group keys, and old `content`/`agent` keys. Avoid the existing `deref` helper where it would accept aliases. Permit anchored ordinary nodes only when no alias is used.
- [x] **Step 4: Add table-driven `TestDecodeCache_Invalid` and rerun it red before extending validation.** Cases: numbers/booleans/null/maps as entries; aliases of whole cache/group/entry; merge keys; duplicate keys; names `""`, `/abs`, `~`, `../x`, `a/../b`, `./x`, `a/./b`, `a:b`, newline/NUL; static missing/extra colons or empty sides; `~otheruser`; relative container target `docs`. Assert `err != nil` and errors identify the cache group/index where applicable. Preserve spaces, commas, and quotes in valid path strings; do not trim them away. Reject control characters in static paths too. Reject normalized duplicate managed names within a group. Shared and local may both contain `.claude` because their destinations differ.
- [x] **Step 5: Finish syntax validation and run `go test -timeout=100s ./internal/projectconfig`.** Expect PASS, including preexisting tests, since public loading is not switched yet. Container paths are normalized later with POSIX `path`, host paths later with `filepath`.
- [x] **Step 6: Commit only the two new files.** Message: `feat: define strict configurable cache declarations`.

## Task 2: Validate and prepare managed cache directories

**Files:** Create `internal/cli/cache_dirs.go` and `internal/cli/cache_dirs_test.go`.

**Interfaces:**
```go
type cacheDir struct {
    Base string // resolved makeslop base; includes shared/workspaces components in checks
    Path string // absolute managed source, strictly below Base
    Entry string // e.g. "cache.shared[0]", for errors
}
func checkCacheDir(dir cacheDir) error
func prepareCacheDirs(ctx context.Context, dirs []cacheDir) error
```
Consumes absolute paths supplied by Task 3, never YAML. `checkCacheDir` permits missing components and performs no writes. `prepareCacheDirs` prechecks every entry, then creates missing directories with mode `0o755` subject to umask, checking context cancellation and component types. Existing directories and contents are preserved.

- [x] **Step 1: Add `TestCheckCacheDir_Containment` and `TestPrepareCacheDirs_CreatesAndPreserves`.** Fixtures include `<base>/shared/.config/tool` and `<base>/workspaces/app-abc123/docs`; neither exists initially. Assert check leaves paths absent, preparation creates directories, a sentinel file survives a second preparation byte-for-byte, and no `.claude.json`/template files appear.
- [x] **Step 2: Run `go test -timeout=100s ./internal/cli -run 'Test(CheckCacheDir|PrepareCacheDirs)'`.** Expect missing-interface failures.
- [x] **Step 3: Implement the interfaces in `cache_dirs.go`.** Use separator-aware containment and inspect existing components starting below the canonical makeslop base, including `shared`, `workspaces`, and the workspace root. Reject a source equal to Base, paths outside Base, files in any directory position, live symlinks, and dangling symlinks. A canonical Base must already be a directory. Recheck components during creation; accept an already-created real directory from a concurrent invocation, never a wrong-type replacement. Do not claim immunity to arbitrary hostile host races.
- [x] **Step 4: Add `TestCheckCacheDir_RejectsSymlinkComponents`, `TestPrepareCacheDirs_PrechecksAll`, and `TestPrepareCacheDirs_Canceled`; run them red before filling gaps.** Cover symlinks at `shared`, at `workspaces/app-abc123`, in an intermediate nested directory, and at a dangling leaf. Assert no target outside Base is created. A valid first directory plus a later file collision returns an error without creating the first. A context canceled before preparation creates nothing. Do not use permission-denial fixtures that stop working when tests run as root.
- [x] **Step 5: Run `go test -timeout=100s ./internal/cli -run 'Test(CheckCacheDir|PrepareCacheDirs)'`.** Expect PASS. Errors include Entry/path and wrap filesystem errors. Document that an operational failure during creation can leave earlier empty directories; retries are safe.
- [x] **Step 6: Commit only the two new files.** Message: `feat: safely prepare managed cache directories`.

## Task 3: Resolve mount plans and reject conflicts

**Files:** Create `internal/cli/cache.go` and `internal/cli/cache_test.go`.

**Interfaces:**
```go
type cachePaths struct { BaseDir, WorkspaceDir, HostHome string }
type cachePlan struct {
    Mounts []docker.Mount
    Dirs   []cacheDir
}
func resolveCache(c projectconfig.CacheMounts, paths cachePaths,
    projects []docker.Project, declaredExcludes []string) (cachePlan, error)
```
Consumes Task 1 declarations, Task 2 `checkCacheDir`, fully resolved projects (main first), and normalized project-relative explicit exclude declarations. BaseDir is canonical; WorkspaceDir is constructed beneath that canonical base using the existing registered name, not independently symlink-resolved. Reads/stat-checks filesystem state but never writes. Produces bind mounts ordered shared/local/static and managed directories for later preparation. No Docker API calls.

- [x] **Step 1: Add `TestResolveCache_Mappings`.** With main `{Host: projectRoot, Name: "app-abc123"}`, assert exact mappings for `.claude`, `docs`, and an existing `./settings.json:~/.settings.json` file:
  ```go
  // Mounts in this order; directory binds retain the existing trailing-slash convention.
  want := []docker.Mount{
      {Host: base + "/shared/.claude/", Container: "/home/user/.claude/"},
      {Host: workspaceDir + "/docs/", Container: "/workspace/app-abc123/docs/"},
      {Host: projectRoot + "/settings.json", Container: "/home/user/.settings.json"},
  }
  ```
  Assert all mounts are writable binds, only two Dirs are returned, and no managed directories were created. Use canonical temp roots to avoid macOS `/tmp` symlink differences.
- [x] **Step 2: Run `go test -timeout=100s ./internal/cli -run '^TestResolveCache'`.** Expect missing-interface failures.
- [x] **Step 3: Implement `resolveCache` in `cache.go`.** Resolve managed sources beneath assigned bases and validate through Task 2. Expand only supported host/container tildes. Resolve relative host paths against `projects[0].Host`; use `filepath.EvalSymlinks`/stat for static sources; accept regular files/directories only. Use `path.Clean` for container targets and separator-aware comparisons before appending trailing slashes. Preserve directory/file distinctions and deterministic group/list order. Return an empty plan on error, not a partially usable plan.
- [x] **Step 4: Add `TestResolveCache_StaticSources` and `TestResolveCache_SharedAndLocalScope`; run them red before completing missing behavior.** Assert missing source errors, file binds lack added directory slashes, directory binds have them, live host symlinks resolve to targets, dangling symlinks/FIFOs fail, both host/container tildes expand separately, and paths with spaces/commas/quotes survive verbatim. For two project names under the same BaseDir, shared source is identical while local sources differ. Empty declarations return an empty plan without requiring HostHome. Shared/local `.claude` together are valid. Valid managed entries followed by an invalid static source leave storage untouched.
- [x] **Step 5: Add `TestResolveCache_Conflicts` and run it red.** Each listed destination/condition must return an error identifying the conflicting entry:
  - normalized equality (`/opt/x` versus `/opt/./x/`) and ancestor/descendant targets across all groups;
  - `/`, `/home`, `/home/user`, `/workspace`, main root, join root, or a path inside a join;
  - main `.makeslop.yaml`, `.git/hooks`, `.git`, or descendants of protected paths, even with `ProtectConfig`/`MaskGitHooks` false;
  - `/tmp`, `/proc`, `/sys`, `/dev`, or their descendants;
  - any overlap with an explicit exclude destination, using declaredExcludes rather than scan masks.
  Also assert `/opt/tool` plus `/opt/toolbox` succeeds, as do `/tmp-cache` and `/device`, and valid mounts beneath the main project/home remain allowed. Static `~` alone resolves to the forbidden home root.
- [x] **Step 6: Finish conflict checks and run `go test -timeout=100s ./internal/cli -run '^TestResolveCache'`.** Expect PASS. A root prohibition is directional (cannot replace the root/ancestors); a protected subtree prohibition rejects both ancestors and descendants. Do not accidentally forbid every valid main-project or home mount.
- [x] **Step 7: Commit only the two new files.** Message: `feat: resolve configurable cache mounts and validate conflicts`.

## Task 4: Switch the public configuration, launch path, and scaffolding

This is one coordinated compatibility-breaking cutover: parser, CLI, Docker options, and their old fixtures must change together. The helper tasks above must already pass. Do not retain old keys, flags, fields, or implicit mounts to keep obsolete tests passing.

**Files:** Modify the existing files in the structure table, including their paired tests and listed documentation. No example Dockerfile cache-build directives need changing: they are unrelated image-build caches.

**Interfaces:**
- `projectconfig.Config.Cache` becomes `CacheMounts`; old `Cache{Content, Agent}` is removed.
- `projectconfig.Excludes` gains `DeclaredPaths []string`: combined, normalized, deduplicated project-relative files/dirs, captured before stat filtering; existing Files/Dirs retain their current absolute live-path meaning.
- `projectconfig.Scaffold(root string) error`; `Stub` contains empty shared/local/static lists and commented examples; remove parameterized `renderStub`.
- `docker.Options.CacheMounts []Mount` replaces `BaseDir`, `WorkspaceHost`, `MountAgentCache`, and `MountContentCache`.
- `runInit(cmd *cobra.Command, ws *workspace.Workspaces, baseDir string, outOfHome, quiet bool) error` drops globalOnly.
- `runRun` keeps its current signature and consumes Tasks 1–3. No new Docker SDK calls or fake-client API methods.

- [x] **Step 1: Replace obsolete config/scaffold expectations and add public loader regressions.** In `projectconfig_test.go`, assert Load returns empty cache declarations for missing/empty config, parses the three groups, rejects old keys and aliases, and preserves unrelated env/network/exclude behavior. `TestLoad_CacheDeclaredExcludes` must assert `DeclaredPaths` retains missing `docs` while Files/Dirs remain filtered. Former agent/content reserved names are accepted without active conflicting mounts; `.makeslop.yaml` remains reserved. Update the scaffold call in `security_test.go`. Run `go test -timeout=100s ./internal/projectconfig ./internal/security`; expect new-contract failures.
- [x] **Step 2: Wire declarations/scaffold in `projectconfig.go`.** Change schema.Cache to raw `yaml.Node`, call `decodeCache`, return zero defaults, populate DeclaredPaths before `statFilter`, and remove the old cache defaults and fixed-agent reserved paths. Retain regular-file/symlink safeguards and exclusive scaffold creation with partial-write cleanup. The generated cache block is exactly three empty lists, with commented shared/local/static examples beneath it; preserve exclude and networking scaffold content.
- [x] **Step 3: Replace old Docker cache toggle tests with `TestBuildSpec_ConfiguredCacheMounts` and `TestBuildSpec_NoImplicitCaches`.** Update sampleOptions and all literal Options fixtures, including those in `cli/run_test.go`. Assert exact order: main bind, policy mounts, configured cache mounts, scan masks, joined sections. With no CacheMounts, there are no home or project cache overlays. Retain sandbox/mask/join/security tests; remove only assertions specific to fixed legacy mounts. Add explicit file/directory cases to `TestDriftGuard_ConfiguredCacheMounts` using `parseMountArg`, checking SDK source/target/read-only parity for spaces, commas, and quotes. Run `go test -timeout=100s ./internal/docker`; expect new-contract failures.
- [x] **Step 4: Replace legacy construction in `docker/spec.go`.** Append `o.CacheMounts` after main policy mounts and before masks. Remove old option fields and unconditional global/agent/content mounts. Keep joins, sections, tmpfs, security flags, and all projections unchanged. Do not mutate or retain an accidentally aliased writable backing array from caller-owned CacheMounts when appending masks.
- [x] **Step 5: Add new bootstrap/init lifecycle expectations and run them red.** `TestBootstrap_StructuralOnly` expects BaseDir, `shared`, and `workspaces`, but no `.claude`, `.codex`, `.claude.json`, or settings file. `TestInit_EmptyCacheScaffold` expects no fixed workspace children and empty config lists. `TestInit_GlobalOnlyRejected` includes init itself. Preserve the existing-config byte-equality test using plain init. Update `status_test.go`'s obsolete global-only invocation and workspace template tests. Run `go test -timeout=100s ./internal/config ./internal/workspace ./internal/cli -run 'Test(Bootstrap|Init|Status)'`; expect contract failures until the cutover is finished.
- [x] **Step 6: Remove automatic cache initialization.** In config, bootstrap structural directories and delete `bootstrapFile` if no caller remains; preserve settings save code. In workspace, remove `scaffoldTemplate` and its Init call, retaining workspace root creation and locked registration. In CLI init, remove the global-only flag/argument and use the new Scaffold signature. Preserve guard ordering, image notices, stdout workspace path, and existing regular configs. Do not touch existing storage during upgrades.
- [x] **Step 7: Add CLI launch regression tests and run them red.** Prefer `runCmdWithDeps` with fake Docker and isolated roots. Add named tests with these assertions:
  - `TestRun_ConfiguredCacheMounts`: all three groups reach `fc.LastSpec` with expected paths; static file content survives; real launch creates only managed directories.
  - `TestRun_CacheDryRunNoWrites`: nonexistent shared/local paths remain absent; printed paths match the real-run plan; missing static source fails even in dry-run.
  - `TestRun_CacheFromSubdirectory`: `./tools` resolves from registered root, not cwd.
  - `TestRun_CacheFailureNoCreation`: invalid later static source, policy conflict, missing image, and failed network preflight each leave managed children absent and `fc.Started == false`.
  - `TestRun_CacheMissingExcludeConflict`: config declares local `docs` and excludes `docs`, but project/docs is absent; error still identifies the conflict.
  - `TestRun_CacheScanMaskOrder`: a scan-matched project file beneath a local overlay remains masked after that overlay; do not confuse scan results with explicit exclude conflicts.
  - `TestRun_NoImplicitCaches`: absent/empty cache never mounts or modifies legacy files already on disk.
  Snapshot directories as well as file contents; the existing `snapshotTree` skips directories and alone cannot prove no creation. Run `go test -timeout=100s ./internal/cli -run '^TestRun_.*Cache'`; expect failures against the not-yet-wired launch path.
- [x] **Step 8: Wire the launch path in `run.go`.** After all projects/configs are loaded, obtain host home only when static source expansion needs it, canonicalize the makeslop base, and call `resolveCache` with DeclaredPaths. Construct cachePaths.WorkspaceDir as `filepath.Join(canonicalBase, config.WorkspacesDir, mainName)`; do not EvalSymlinks the workspace/shared subdirectories, which would hide the symlinks Task 2 must reject. Pass Mounts to BuildSpec. Dry-run prints and returns without preparation. Real launch retains image/network preflight, then calls `prepareCacheDirs(cmd.Context(), plan.Dirs)` before `deps.api.Run`. All validation and source checks precede creation. Keep image resolution before workspace lookup and daemon checks before secret scans. Surface preparation/resolution errors normally, without `--quiet` suppression.
- [x] **Step 9: Add and implement join/removal regressions.** In `join_test.go`, give a join valid managed names plus a syntactically valid static entry whose host is missing: launch must ignore those entries without resolving/creating them. Nonempty join cache/env/network settings emit one combined notice; quiet suppresses the notice but not degraded-masking warnings. In `remove_test.go`, removal deletes the owner's local cache while shared data and external static files survive. A static source inside the removed workspace is removed with that directory, as documented. Update `loadProject`'s ignored-settings condition/message, without changing join config validation or registration requirements.
- [x] **Step 10: Run focused package tests and remove all obsolete fixtures.** Run `go test -timeout=100s ./internal/projectconfig ./internal/config ./internal/workspace ./internal/docker ./internal/security ./internal/cli`; expect PASS. Search `internal/` for `MountAgentCache`, `MountContentCache`, `WorkspaceHost`, `Cache{Content`, `renderStub`, and `scaffoldTemplate`; none remain. Remaining mentions of old YAML keys/flag are deliberate rejection tests, not active functionality. Update dependent expected mount counts, golden output, and notices from observed contract differences, never by deleting unrelated coverage.
- [x] **Step 11: Update user/contributor documentation with the same cutover.** Replace reference init/run/cache-layout/mount-table/join descriptions and the security YAML example. Show shared/local reuse, a static file, a static directory, both tilde meanings, relative host resolution, and nested names. Explain empty defaults, file preparation by the user, writable shared credential risk, lack of scanning for external/cache sources, conflict rules, ignored join caches, dry-run behavior, and removal scope. State legacy data is untouched and must be explicitly rebound/moved manually. Update architecture and CLAUDE's old always-mounted/cache-boolean invariants. Add a short README link/example. Preserve Dockerfile build-cache directives.
- [x] **Step 12: Verify the complete cutover.** Format only changed Go files and run:
  ```sh
  go test -timeout=100s ./...
  build_dir=$(mktemp -d)
  go build -o "$build_dir/makeslop" ./cmd/makeslop
  "$build_dir/makeslop" init --help
  git diff --check
  golangci-lint run  # if installed; CI pins v2.12.2
  ```
  Expect test/build/lint exit 0; init help must not advertise global-only. Use a temporary build output so the existing root `makeslop` artifact is not overwritten. If lint is unavailable, report it rather than install/upgrade dependencies. Read the diff and verify only intended files changed; do not claim tests ran if execution was blocked.
- [x] **Step 13: Commit the coordinated cutover.** Explicitly stage changed implementation/test/docs files from the structure table, excluding unrelated `.gitignore`; use a path-scoped commit when unrelated index entries exist. Message: `feat!: replace fixed cache mounts with project-configured mounts`.

## Plan self-review

- Spec coverage: declarations and strict syntax (Task 1), storage containment/lifecycle (Task 2), source/destination resolution and conflicts (Task 3), public integration/bootstrap/joins/remove/docs/parity (Task 4).
- Interface consistency: CacheMounts/CacheBind flow from decoder to resolver; cachePlan.Dirs flows to prepareCacheDirs; cachePlan.Mounts flows to Options.CacheMounts; DeclaredPaths is project-relative and never confused with live absolute mask paths.
- Review Focus: all five cases have named tests in the owning tasks.
- No implementation or new dependencies were part of writing this plan. The user selected native execution.

## Execution results

Implemented on `feat/cache` in commits `a36ed2a`, `3707576`, `68768ea`, and `9823cde`.
A fresh read-only whole-branch reviewer found no Critical, Important, or Minor findings.
The reviewer inspected code/tests but did not independently execute them.

Final verification:
- `go build` to a temporary output: passed.
- `golangci-lint run`: passed, 0 issues.
- `gopls check` on changed production files: no diagnostics.
- Unfiltered `go test -count=1 -timeout=100s ./...`: the four known baseline permission-fixture failures below; all other packages/tests passed.
- The same suite skipping exactly those four names: passed.
- Race testing unavailable: cgo disabled and no C compiler. Live Docker-daemon mount behavior was not exercised.

Known baseline failures, left unchanged with user approval:
`TestResolveJoins_ConfigLstatError`, `TestRun_Join_ScanWalkError_Aborts`,
`TestStatus_Check5_ScanErrShowsWarn`, `TestScan_WalkError_Propagated`.
One existing alias-overlap assertion failed transiently once; its implementation was unchanged,
and 12 isolated reruns plus subsequent suites passed.

### Rulings and costs

- Work in the current checkout, as explicitly chosen by the user, instead of a linked worktree. Cost: implementation changes reside directly on the current feature branch.
- Use repo-local ignored TMPDIR because `/tmp` is noexec. Cost: test execution here requires that environment override.
- Leave the four baseline failures unchanged, as authorized; use an exactly filtered suite for task completion while retaining unfiltered evidence. Cost: no claim of an entirely green unfiltered suite in this environment.
- Put integration coverage in `run_cache_test.go` and `spec_cache_test.go` because their existing test files are large. Cost: two same-source test files to navigate.
- Preserve the explicitly requested compatibility break and no migration. Cost: existing YAML must be rewritten and legacy data moved or explicitly rebound manually.
- Retain component-based managed symlink checks without claiming resistance to arbitrary concurrent hostile filesystem replacement, per design. Cost: such host races are outside the isolation guarantee.
- Leave the unrelated baseline/transient permission and alias behavior unchanged after review. Cost: those environmental/pre-existing failure modes remain and can obscure a future regression.
- Accept review of validation/projections without live Docker or race-detector execution. Cost: actual daemon mount application and race-detector behavior remain unverified here.

Deferred minors: none. No merge or push was performed; the user's staged `.gitignore` change was preserved.
