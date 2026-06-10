# Security Fixes — sandbox-policy mounts, scan hardening, Dockerfile hardening

## Overview

Implement all code fixes from `SECURITY_ANALYSIS.md` (2026-06-09 review):

- The agent inside the container can currently **rewrite its own sandbox policy** (`.makeslop.yaml`
  is inside the rw project bind) and **plant git hooks** that execute on the host later. Fix by
  mounting the policy file read-only over itself and overlaying `.git/hooks` with tmpfs.
- Secret masking has **silent failure modes**: symlinks matching secret patterns (or explicitly
  listed in `exclude.files`/`exclude.dirs`) are dropped without a word. Fix by surfacing warnings.
- The default scan pattern list misses common secret shapes (`*.p12`, `*.tfstate`, …). Expand the
  stub (new projects only — no auto-migrate, per existing rule).
- The embedded image has a **sudo user with password `password`** and three unpinned
  `curl | bash` downloads. Remove the escalation path; pin/verify infra downloads; let agent
  installers float ("pin infra, float agents").

Out of scope (explicitly): the `docs/security.md` threat-model section about residual risks
(writable project files, mounted credentials, open egress) — docs changes here are consistency
updates only.

## Context (from discovery)

- Project: Go CLI (`makeslop`) that launches AI agents in hardened Docker containers via the moby
  SDK. Pure argv/SDK-struct assembly in `internal/docker/spec.go`; impure exec in `run.go`;
  preflight in `preflight.go`.
- Files involved: `internal/docker/spec.go`, `internal/docker/spec_test.go`,
  `cmd/makeslop/main.go`, `cmd/makeslop/main_test.go`, `cmd/makeslop/status.go` (calls
  `security.Scan` too), `internal/projectconfig/projectconfig.go` (+ test),
  `internal/security/security.go` (+ test), `internal/assets/files/Dockerfile`,
  `internal/config/config.go` (MigrationVersion), `docs/security.md`, `CLAUDE.md`.
- Key conventions: `BuildSpec` is pure (no filesystem access — existence checks belong in
  `runRun`); mask overlays must follow the project-root bind in mount order; a drift-guard test
  keeps `Args()` and `ContainerConfig()`/`HostConfig()` renderings in sync; Dockerfile changes
  require a `MigrationVersion` bump (`CurrentVersion` untouched — no `Settings` field changes).
- Design decisions (validated in brainstorm): `.git` masking is **hooks only** (push/fetch keeps
  working); `.makeslop.yaml` protection is a **read-only self-bind** (agent can read, not write),
  gated on host existence; Dockerfile pins **infra only** (base digest, Go/Node/zsh-in-docker
  checksums) while claude/codex installers float.

## Development Approach

- **testing approach**: Regular (code first, then tests within the same task)
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
  - tests are not optional - they are a required part of the checklist
  - tests cover both success and error scenarios
- **CRITICAL: all tests must pass before starting next task** - no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- run `go test ./...` after each change; `go vet ./...` before finishing
- maintain backward compatibility (existing `.makeslop.yaml` files keep working; `Load`'s
  documented 4-value return is preserved by carrying warnings inside `Excludes`)

## Testing Strategy

- **unit tests**: required for every task; follow the existing table-driven style
- no UI e2e tests in this project; the gated Docker integration test
  (`MAKESLOP_DOCKER_IT=1 go test -tags integration ./internal/docker/`) is the closest equivalent
  and validates the new Dockerfile against a live daemon (Post-Completion — needs a daemon)

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- keep plan in sync with actual work done

## Solution Overview

1. **Two new `docker.Options` booleans** — `ProtectProjectConfig` and `MaskGitHooks` — consumed by
   pure `BuildSpec`; the impure `os.Stat` gating lives in `runRun`. `--dry-run` reflects both
   automatically via the shared `Spec`.
2. **Warnings as data, not I/O**: `security.Scan` gains a second return slice (symlinks whose
   basename matched a pattern); `projectconfig.Load` carries symlink-drop warnings in a new
   `Excludes.Warnings` field (4-value return preserved). `runRun` prints them to stderr
   **bypassing `--quiet`** — degraded protection is a warning, not chrome.
3. **Stub-only pattern expansion** + `.makeslop.yaml` added to `reservedPaths`.
4. **Dockerfile**: no sudo/password; base pinned by digest; Go and Node installed from official
   tarballs with per-arch sha256 verification (nodesource `curl | bash` removed); zsh-in-docker
   checksum-verified; claude/codex float. `MigrationVersion` 2 → 3.

## Technical Details

- **Mount shapes** (in `BuildSpec`, inserted at a fixed point: after the 4 always-present base
  mounts and **before** the cache overlays — all overlays target distinct paths, so correctness
  only requires following `mounts[0]`, but a fixed index keeps ordering/drift-guard test
  expectations deterministic):
  - `ProtectProjectConfig` → `Mount{Host: <ProjectRoot>/.makeslop.yaml, Container: <workspacePath>/.makeslop.yaml, ReadOnly: true}` (bind)
  - `MaskGitHooks` → `Mount{Type: "tmpfs", Container: <workspacePath>/.git/hooks}`
- **Gating in `runRun`** (before `BuildSpec`):
  - `ProtectProjectConfig = true` iff `<workspaceRoot>/.makeslop.yaml` Lstats as a regular file.
    Rationale: a missing bind source fails container create, and `/dev/null`-masking an absent
    target would make the daemon create an empty `.makeslop.yaml` on the host through the rw bind
    — and an *empty* config means *no scan patterns*, i.e. we'd disable masking ourselves. When
    the file is absent, protection is already nil; skip.
  - `MaskGitHooks = true` iff `<workspaceRoot>/.git` Lstats as a **directory** (avoid the daemon
    creating `.git/hooks/` on the host in non-git projects).
  - **Known residual — `.git`-as-file (worktrees/submodules):** in git worktrees and submodules,
    `.git` is a regular *file* (a gitfile pointing at the real gitdir elsewhere). The directory
    gate correctly leaves `MaskGitHooks` off there (safe: no daemon-created dirs), but the real
    hooks directory lives outside the workspace and is therefore **not protected**. We do NOT
    chase the gitfile target (would reintroduce filesystem parsing complexity); instead this is
    tested explicitly (gate stays off) and documented honestly in `docs/security.md` as an
    unmitigated residual for worktree/submodule checkouts.
  - `MaskGitHooks` is independent of `exclude.scan.skip-dirs` — the stub's `.git` skip-dir only
    prunes the *secret scan*; the hooks tmpfs is an unconditional mount whenever the gate is on.
- **`security.Scan` signature**: `Scan(ctx, root, patterns, skipDirs) (paths, symlinkMatches []string, err error)`.
  Both slices sorted. Callers: `runRun` (prints warnings) and `runStatus` in `status.go`
  (ignore symlinkMatches or surface in detail text — keep minimal: ignore).
- **`projectconfig`**: `Excludes` gains `Warnings []string`; `reservedPaths` gains
  `".makeslop.yaml"`. **`statFilter` needs a signature change** — it currently lumps symlinks in
  with all other `!keep(info)` drops, so it must check `info.Mode()&os.ModeSymlink != 0`
  explicitly *before* the `keep` predicate and return `(result, warnings []string, err error)`.
  It is called twice (`exclude.files` and `exclude.dirs`); warnings from **both** calls are merged
  into `Excludes.Warnings`. Decision: only symlinks warn (`"path %q is a symlink and is NOT
  masked"`); missing entries and non-symlink wrong-type drops (e.g. a directory listed in
  `exclude.files`) stay silent as today — scope kept minimal, decision recorded here.
- **Stub pattern additions** (in `renderStub`): `*.p12`, `*.pfx`, `*.tfstate`, `.pypirc`,
  `.htpasswd`, `service-account*.json`, `kubeconfig`, `*.kubeconfig`. Bare `credentials`
  deliberately excluded (too noisy).
- **Warning output in `runRun`**: write directly to `cmd.ErrOrStderr()` (NOT through
  `quietWriter`) as `makeslop: warning: symlink <path> matches a secret pattern but is NOT masked`
  and `makeslop: warning: <projectconfig warning>`.
- **Dockerfile** specifics:
  - Drop `sudo` from the apt package list; delete the `chpasswd` and `usermod -aG sudo` lines.
  - `FROM debian:trixie-slim@sha256:<digest>` — resolve current digest at implementation time
    (`docker buildx imagetools inspect debian:trixie-slim`); keep the tag in the ref for
    readability.
  - Go tarball: add per-arch sha256 verification (amd64/arm64 case on `dpkg --print-architecture`,
    checksums from go.dev/dl for the pinned `GO_VERSION`).
  - Node: remove the nodesource `curl | bash`; install the pinned current LTS from
    nodejs.org/dist tarball with per-arch sha256, mirroring the Go install (map `amd64→x64`,
    `arm64→arm64`). PATH gains the Node bin dir.
  - zsh-in-docker: keep the `v1.2.1` tag pin; download to a temp file, `sha256sum -c` against the
    release artifact's checksum (computed once at implementation time), then execute.
  - `claude.ai/install.sh` and `npm install -g @openai/codex` stay floating (accepted residual
    risk, user decision).
- **`MigrationVersion` 2 → 3** in `internal/config/config.go` (Dockerfile changed).
  `CurrentVersion` stays 1.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code, tests, doc consistency updates.
- **Post-Completion** (no checkboxes): live-daemon integration build, digest/checksum freshness
  notes, `makeslop migrate` rollout note.

## Implementation Steps

### Task 1: Sandbox-policy mounts in BuildSpec

**Files:**
- Modify: `internal/docker/spec.go`
- Modify: `internal/docker/spec_test.go`

- [x] add `ProtectProjectConfig bool` and `MaskGitHooks bool` to `docker.Options` with doc comments
- [x] in `BuildSpec`, when `ProtectProjectConfig`, insert the read-only `.makeslop.yaml` self-bind after the 4 base mounts, before the cache overlays
- [x] in `BuildSpec`, when `MaskGitHooks`, insert the `.git/hooks` tmpfs mount at the same fixed point (after `.makeslop.yaml` bind when both are on)
- [x] write table-driven tests: both flags off (mount lists unchanged), each flag on (mount present, correct shape, `ReadOnly: true` rendered as `,readonly` in `Args()`), exact position after `mounts[0]` and before cache overlays
- [x] verify the drift-guard test passes (Args ↔ ContainerConfig/HostConfig parity for the new mounts)
- [x] run `go test ./internal/docker/` — must pass before task 2

### Task 2: Symlink reporting in security.Scan

**Files:**
- Modify: `internal/security/security.go`
- Modify: `internal/security/security_test.go`
- Modify: `cmd/makeslop/status.go` (caller signature update only)
- Modify: `cmd/makeslop/main.go` (caller signature update only; warning printing comes in Task 4)

- [x] change `Scan` to return `(paths, symlinkMatches []string, err error)`: a symlink whose basename matches a pattern goes into `symlinkMatches` (sorted) instead of being silently skipped
- [x] update both callers (`runRun`, `runStatus`) for the new signature (`runStatus` ignores the slice)
- [x] write tests: matching symlink reported, non-matching symlink still silent, regular-file behavior unchanged, both slices sorted
- [x] update package doc comment (fail-loud now includes symlink visibility)
- [x] run `go test ./...` — must pass before task 3

### Task 3: projectconfig — reserved path, stub patterns, symlink warnings

**Files:**
- Modify: `internal/projectconfig/projectconfig.go`
- Modify: `internal/projectconfig/projectconfig_test.go`

- [ ] add `".makeslop.yaml"` to `reservedPaths`
- [ ] add the 8 new patterns to `renderStub` (and therefore `Stub`)
- [ ] add `Warnings []string` to `Excludes`
- [ ] change `statFilter` signature to `(result, warnings []string, err error)` with an explicit `os.ModeSymlink` check before the `keep` predicate; merge warnings from both the files and dirs calls into `Excludes.Warnings` (missing entries and non-symlink wrong-type drops stay silent — recorded decision)
- [ ] write tests: `.makeslop.yaml` in `exclude.files`/`exclude.dirs` rejected with the reserved-path error; symlinked exclude entry (in each list) produces a warning and no mask; non-symlink wrong-type entry stays silent; stub renders the new patterns; absent-file behavior unchanged
- [ ] run `go test ./internal/projectconfig/` — must pass before task 4

### Task 4: runRun gating + warning output

**Files:**
- Modify: `cmd/makeslop/main.go`
- Modify: `cmd/makeslop/main_test.go`

- [ ] in `runRun`, Lstat `<workspaceRoot>/.makeslop.yaml` (regular file → `ProtectProjectConfig: true`) and `<workspaceRoot>/.git` (directory → `MaskGitHooks: true`; a `.git` *file* — worktree/submodule gitfile — leaves it false); set both on `docker.Options`
- [ ] print `security.Scan` symlink warnings and `Excludes.Warnings` to `cmd.ErrOrStderr()` directly — NOT via `quietWriter` (warnings bypass `--quiet`)
- [ ] write boundary tests using **real temp-dir workspaces** (security.Scan and the Lstat gates are not injected — tests must create a real `.makeslop.yaml`, `.git/` dir, symlinks): git project + config present → both mounts in the spec the fake runner receives; no `.git` / no `.makeslop.yaml` → respective mount absent; `.git` as a regular file → hooks mount absent
- [ ] write quiet-contract test: with `--quiet`, the `masked N secret file(s)` chrome line IS suppressed while symlink warnings are NOT — both assertions in one test to lock the contract
- [ ] write `--dry-run` test: output includes the new mounts
- [ ] run `go test ./cmd/makeslop/` — must pass before task 5

### Task 5: Dockerfile hardening + MigrationVersion bump

**Files:**
- Modify: `internal/assets/files/Dockerfile`
- Modify: `internal/config/config.go`
- Modify: `internal/config/migrate_test.go` (if it asserts the version constant)

- [ ] remove `sudo` from the apt list; delete the `chpasswd` and `usermod -aG sudo` lines
- [ ] pin base: `FROM debian:trixie-slim@sha256:<digest>` (resolve via `docker buildx imagetools inspect`; if no daemon/network available at implementation time, note it as a ⚠️ blocker and leave a clearly-marked placeholder that MUST be resolved before merge)
- [ ] add per-arch sha256 verification to the Go tarball download (checksums from go.dev/dl for GO_VERSION)
- [ ] replace the nodesource `curl | bash` with a pinned Node LTS tarball install + per-arch sha256 (amd64→x64 mapping), removing the apt nodejs package; verify `node`/`npm` on PATH
- [ ] checksum-verify the zsh-in-docker v1.2.1 script before executing it
- [ ] bump `MigrationVersion` to 3 in `internal/config/config.go`
- [ ] verify `migrate_test.go` assertions reference the `MigrationVersion` constant rather than a hardcoded `2` (review confirmed: they do — no test changes expected); run `go test ./internal/config/ ./internal/assets/`
- [ ] run `go test ./...` — must pass before task 6

### Task 6: Verify acceptance criteria

- [ ] verify all SECURITY_ANALYSIS.md code-fix items are implemented (mounts, reserved path, patterns, symlink warnings, sudo removal, pinning, version bump)
- [ ] verify edge cases: project without `.git`; project without `.makeslop.yaml`; symlinked `.env`; `--quiet` + warnings; `--dry-run` rendering
- [ ] run full suite: `go test ./...` and `go vet ./...`
- [ ] `go build ./...` clean

### Task 7: [Final] Documentation consistency

**Files:**
- Modify: `docs/security.md`
- Modify: `docs/reference.md` (if it lists mounts/flags affected)
- Modify: `CLAUDE.md`

- [ ] update the quoted stub pattern list in `docs/security.md` to match the new stub
- [ ] document in `docs/security.md`: `.makeslop.yaml` is mounted read-only in the container and is a reserved path; `.git/hooks` is tmpfs-masked in git projects; symlink warnings and their `--quiet` bypass
- [ ] document the worktree/submodule residual in `docs/security.md`: when `.git` is a gitfile, the real hooks directory is outside the workspace and is NOT masked
- [ ] update `CLAUDE.md`: new `Options` fields, new `Scan` signature, `Excludes.Warnings`, MigrationVersion=3 note, reserved-paths addition
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

**Manual verification:**
- Run the gated integration build against a live daemon:
  `MAKESLOP_DOCKER_IT=1 go test -tags integration ./internal/docker/` — validates the hardened
  Dockerfile end to end (checksums, digest, no-sudo user).
- Smoke test interactively: `makeslop migrate && makeslop build && makeslop run` in a git project;
  confirm `.makeslop.yaml` is read-only inside the container (`touch` fails), `.git/hooks` is
  empty tmpfs, and `git push` still works.

**External notes:**
- Existing installs pick up the hardened Dockerfile only after `makeslop migrate` + `makeslop build`
  (MigrationVersion 3 nudge handles discovery).
- Pattern-list expansion reaches only newly scaffolded projects; existing `.makeslop.yaml` files
  need a manual copy of the new patterns (consistent with the documented no-auto-migrate rule).
- The pinned digest/checksums will go stale over time; bumping `GO_VERSION`/Node LTS later means
  refreshing the corresponding sha256s and re-bumping `MigrationVersion`.
