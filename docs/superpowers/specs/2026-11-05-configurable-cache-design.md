# Configurable cache mounts

Status: approved by the user's request to write the implementation plan.

## Intent

Replace makeslop's hard-coded agent and content mounts with project-configured shared directories, project-local directories, and explicit host binds. Users decide what persists and which projects share it. Backward compatibility is explicitly not required.

## Decisions made with the user

- `cache.shared` contains directory names. Sources live under `~/.makeslop/shared/`; destinations use the same name beneath the container home.
- `cache.local` contains directory names. Sources live under `~/.makeslop/workspaces/<registered-name>/`; destinations use the same name beneath the container project root.
- Create missing shared and local directories; reuse existing directories without modifying their contents.
- `cache.static` contains `host-path:container-path` strings. Sources must exist. Mount regular files as files and directories as directories; never create missing static sources.
- Container destinations support `~` for the container home.
- Remove the old cache implementation rather than retaining compatibility.

## Approaches

For configuration, the user selected names with fixed destinations over explicit name-to-destination mappings or a hybrid shorthand. Static entries provide custom destinations without making managed entries more complex.

For implementation, the proposed choice is to retain the current package boundaries: projectconfig validates declarations, the CLI resolves and prepares sources, and docker renders explicit mounts. A dedicated cache package would isolate lifecycle operations but is unnecessary without cache migration, cleanup policies, or other independent cache operations. This implementation choice is the assistant's recommendation, not a separate user requirement.

## Configuration and layout

```yaml
cache:
  shared:
    - .claude
    - .codex
  local:
    - docs
    - .claude
  static:
    - "~/.claude.json:~/.claude.json"
    - "./CLAUDE.md:/workspace/example-abc123/CLAUDE.md"
    - "./tools:/opt/tools"
```

Example mappings for project `example-abc123`:

| Declaration | Host source | Container destination |
| --- | --- | --- |
| shared `.claude` | `~/.makeslop/shared/.claude` | `/home/user/.claude` |
| local `docs` | `~/.makeslop/workspaces/example-abc123/docs` | `/workspace/example-abc123/docs` |
| static `./tools:/opt/tools` | `<registered-project-root>/tools` | `/opt/tools` |

Managed directories are empty on first creation: do not copy content from the project, host home, or image. Mounting a cache over existing project/image content hides that content for the container's lifetime, as with existing overlays.

### Proposed syntax details

These are assistant decisions for approval:

- Canonical keys are `cache`, `shared`, `local`, and `static`; misspellings and old `content`/`agent` keys fail strict validation.
- Each group is a YAML list of strings. Missing config, a missing cache block, or empty groups mean no cache mounts.
- Managed names may be relative directory paths such as `.config/tool`; retain the relative path beneath both the source base and destination base. Reject empty names, absolute paths, `~`, `.`/`..` components, control characters, and colons. Normalize redundant separators and trailing slashes before checking collisions.
- Static entries have exactly one colon separating two nonempty paths; colon-containing paths and `:ro`/`:rw` suffixes are out of scope. All cache binds are writable.
- Resolve relative static host paths against the registered project root, not the invocation directory. Host `~`/`~/...` uses the host user's home. Container `~`/`~/...` uses `/home/user`, matching existing makeslop conventions. Reject `~otheruser` and do not expand environment variables.
- Static container destinations must be absolute or start with `~`; no implicit relative destinations.
- Resolve static host symlinks to their existing targets. Allow only regular files and directories; reject missing targets and special files.

## Lifecycle

- `init` registers a workspace and scaffolds empty cache lists with commented examples, retaining the existing exclude defaults. Existing regular config files remain untouched.
- Remove `init --global-only`; it no longer has a role without implicit mounts.
- Bootstrap creates only the structural directories makeslop needs, not fixed agent directories or `.claude.json`.
- Workspace registration no longer scaffolds `.claude`, `.codex`, `docs`, or `CLAUDE.md`.
- On real `run`, validate all cache declarations and resolve/check all static sources before creating managed directories. Prepare managed sources after existing image/network preflight succeeds and before container launch. Errors abort launch with the group and entry identified.
- A later directory-creation failure can leave earlier empty directories; retries are idempotent. Never remove or overwrite existing data to recover from an error.
- `run --dry-run` resolves and validates mounts but does not create cache directories. It can display future managed sources that do not yet exist. Missing static sources still fail.
- `remove` retains its current behavior: delete that workspace's cache directory, never shared storage and never separately delete static sources. A static source deliberately placed inside the deleted workspace cache is naturally deleted with that directory.
- Existing legacy data stays where it is. No migration, copying, or automatic cleanup. Users may explicitly bind old paths through `static`.

## Scope and isolation

Only the main project's cache configuration applies. Joined projects continue contributing excludes only; do not register joins or create their caches. Nonempty ignored join cache settings should be included in the existing quiet-aware notice about ignored settings.

A local cache is selected only for its owning registered workspace. This is not a host-user permissions boundary: a user who explicitly binds another directory through `static` can expose it. Shared cache contents are writable by every project that opts into them; documentation must call out the credential and cross-project modification implications.

## Validation and protection

- Keep strict YAML decoding, including rejection of duplicate keys and malformed entry types. Reject YAML aliases and merge keys within cache declarations rather than introducing alternate representations.
- Reject duplicate or nested/overlapping cache destinations across all groups after normalization; never rely on last-mount-wins behavior.
- Do not allow a cache mount to replace `/`, the container home itself, `/workspace`, a main/join project root, or an ancestor of those roots. Normal cache entries beneath home and the main project remain allowed.
- Reject destinations inside joined project roots or overlapping makeslop's protected config and Git-hook paths. Treat protection paths as reserved even if their source is absent at validation time.
- Reject overlap with `/tmp` (the existing tmpfs), `/proc`, `/sys`, and `/dev`; custom cache mounts must not replace runtime or sandbox infrastructure.
- Replace the old fixed reserved list for `.claude`, `.codex`, `docs`, and `CLAUDE.md` with checks against actual configured mounts. Preserve `.makeslop.yaml` protection. Explicit excludes that conflict with a cache overlay fail clearly rather than silently masking unrelated cache content.
- Scan-generated masks keep their established precedence after cache mounts. This change does not extend secret scanning into shared/local cache contents or arbitrary static source directories; document that limitation.
- Managed source creation must stay beneath its assigned shared/workspace base, including checking existing path components for symlinks. Check the shared/workspace base itself and its components beneath the resolved makeslop base directory as well. Reject symlink components and wrong-type existing sources rather than following them to another workspace or outside makeslop storage. Do not claim protection against arbitrary concurrent host filesystem changes.

## Implementation boundaries

- `internal/projectconfig`: replace cache booleans with declarations for shared/local/static entries; parse and validate syntax; scaffold the new YAML. No source creation or process-environment lookup in loading.
- `internal/cli`: resolve host home, project-relative sources, container targets, collisions and live filesystem types; create managed directories for real runs; pass a resolved ordered mount list to Docker.
- `internal/config` and `internal/workspace`: remove agent-specific bootstrap and fixed workspace templates. Preserve registration locking, workspace names, settings behavior, and removal semantics.
- `internal/docker`: replace BaseDir/WorkspaceHost-derived cache mounts and cache booleans with explicit cache mounts. Keep BuildSpec pure and preserve SDK/dry-run projection parity. Emit shared, then local, then static mounts in list order; masks must follow mounts they cover.
- Keep using the Docker SDK; no new dependencies or Docker CLI calls are needed.
- Update reference/security/architecture docs, CLI help, examples, and contributor notes to describe the replacement behavior and remove stale invariants.

## Non-goals and anti-goals

No compatibility shim, data migration, named Docker volumes, automatic image building/pulling, read-only cache mode, static source creation, or arbitrary container-home discovery. No silent fallback from invalid config, implicit credential sharing, last-entry-wins collisions, or overwriting existing data.

## Verification expectations

- Parser coverage: empty/absent config; valid lists; old/unknown keys; malformed YAML/types; duplicates/aliases; traversal and invalid paths; static delimiter errors.
- Resolver/lifecycle coverage: shared reuse across two projects; local separation; nested managed names; idempotent creation; files versus directories; missing static paths; host/container tilde; project-relative resolution from a subdirectory; symlinks and wrong types; no side effects on dry runs and validation failures.
- Mount coverage: normalized collisions, policy/exclude conflicts, joins, directory/file representation, no implicit legacy mounts, deterministic order, SDK/dry-run parity.
- CLI coverage: new scaffold, removed flag, no agent-specific bootstrap, ignored join caches, existing removal behavior, preservation of existing configs/data.
- Run focused tests while implementing, then `go test -timeout=100s ./...` and `go build ./cmd/makeslop`; run the configured linter if available.

## Workflow checklist

- [x] Inspect the existing implementation.
- [x] Confirm managed destinations and missing/static-file behavior.
- [x] Present approaches and recommend one consistent with existing architecture.
- [x] Write the design.
- [x] Review the design as a builder and resolve material gaps (self-review; clarified YAML aliases, managed base symlinks, and removal of static sources located inside workspace storage).
- [x] Obtain user approval.
- [x] Invoke writing-plans and create the implementation plan: `docs/superpowers/plans/2026-11-05-configurable-cache.md`.

Exact helper/type names and test organization remain implementation choices.
