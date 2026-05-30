# Embed Dockerfile + version-gated `makeslop migrate`

## Overview
- Embed the container `Dockerfile` into the `makeslop` binary and materialize it onto disk under `~/.makeslop/`.
- `makeslop init` seeds `~/.makeslop/Dockerfile` **if absent** (never overwrites — same `O_EXCL` pattern as other bootstrap seed files).
- New `makeslop migrate` command brings `~/.makeslop/` up to date: it is **version-gated** on a binary `MigrationVersion` constant vs. a persisted `migrated_version`, and when they differ it runs all migration steps (today: force-overwrite the Dockerfile from the embedded asset) and stamps the new version.
- Benefit: the binary is the single source of truth for the Dockerfile; first install gets a working Dockerfile via `init`, and a binary upgrade that bumps `MigrationVersion` refreshes it via `migrate`.

## Context (from discovery)
- Project: `makeslop` (module `github.com/Zwergpro/makeslop`), a Go/cobra CLI that runs agents in a Docker container with a per-user home at `~/.makeslop/` and per-workspace cache.
- Files/components involved:
  - `cmd/makeslop/main.go` — cobra root with `init` and `go` subcommands; `ensureWithinHome`/`--out-of-home` guard applies to `init`/`go`.
  - `internal/config/config.go` — `~/.makeslop/settings.json` management + `Bootstrap(baseDir)`. Constants `SettingsFile`, `WorkspacesDir`, `CurrentVersion=1` (settings **schema** version), `DefaultImage`, `DefaultShell`. `Settings{Version, Image, Shell, Workspaces}`. `Save` writes atomically via temp-file + intra-dir rename. `Bootstrap` is idempotent/never-overwrite: creates `bootstrapDirs` and `bootstrapFiles` (currently `.claude.json` => `"{}\n"`) with `O_EXCL`.
  - Repo-root `Dockerfile` (~2.4KB, starts with `FROM golang:1.26-trixie`) — currently not shipped/built by Go code.
- Related patterns found:
  - Atomic write = temp file + `os.Rename` (in `config.Save`).
  - Idempotent create-if-absent = `os.OpenFile(..., O_CREATE|O_EXCL|O_WRONLY, 0o644)`, treating `fs.ErrExist` as success (in `config.Bootstrap`).
  - Tests are regular (code-first), table-driven, same-package `_test.go`, using `t.TempDir()`. `cmd/makeslop/main_test.go` drives commands via a `runCmd(t, baseDir, args...)` helper and `newRootCmd(baseDir)`.
- Dependencies identified: `assets` package (new) will be imported by `internal/config`. Standard library `embed` only — no new third-party deps.

## Development Approach
- **testing approach**: Regular (code first, then tests) — matches existing repo style (table-driven, same-package tests).
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task (success + error/edge scenarios)
- **CRITICAL: all tests must pass before starting next task** — no exceptions
- run tests after each change
- maintain backward compatibility: existing `Settings` files without `migrated_version` must load cleanly (field is `omitempty`, absent => 0)

## Testing Strategy
- **unit tests**: required for every task. New package `internal/assets` gets an embed guard test; `internal/config` gets migrate + bootstrap-seed tests; `cmd/makeslop` gets a `migrate` command test.
- **e2e tests**: project has no UI/browser e2e suite — not applicable. CLI behavior is covered via `cmd/makeslop/main_test.go` command-level tests.

## Progress Tracking
- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope

## Solution Overview
- **`internal/assets`** — a dedicated package that owns files compiled into the binary. `//go:embed files/Dockerfile` exposes `var Dockerfile []byte`. The repo-root `Dockerfile` is **moved** here (single source of truth; root copy deleted).
- **`init` seeding** — add a `Dockerfile` entry to `bootstrapFiles` so `Bootstrap` writes it with `O_EXCL` (create-if-absent). No version stamp on init.
- **`migrate`** — a coarse, whole-dir version gate. The binary ships `MigrationVersion`; `~/.makeslop/settings.json` persists `migrated_version`. `config.Migrate` runs all idempotent steps only when the two differ, then stamps. A **separate** `MigrationVersion`/`migrated_version` pair is required because `Load` defaults the existing `Version` to `CurrentVersion` on a missing file — reusing it would make `migrate` skip forever after `init`.
- Key design decisions:
  - Whole-dir version (one number, run all steps) over per-step version tracking — YAGNI; steps are idempotent.
  - `writeDockerfile` keeps its own temp+rename block rather than extracting a shared `atomicWrite` helper — duplication preferred over coupling for ~10 lines.
  - `migrate` operates on `~/.makeslop`, not cwd → no `ensureWithinHome` guard, no `Bootstrap` call (its writers `MkdirAll(baseDir)`).

## Technical Details
- **Embed**: `internal/assets/assets.go` with `import _ "embed"` and `//go:embed files/Dockerfile` over `var Dockerfile []byte`. The embed path is relative to the source file, so the file must live at `internal/assets/files/Dockerfile`.
- **Constant**: `MigrationVersion = 1` in `internal/config/config.go` (distinct from `CurrentVersion`). Bump when a migration step is added/changed.
- **Settings field**: `MigratedVersion int `json:"migrated_version,omitempty"`` — NOT defaulted in `Load`; absent => 0. Note: `Migrate` calls `Save`, which intentionally rewrites the whole `settings.json` and adds the `migrated_version` key for already-initialized users. The existing `TestSaveLoadByteIdenticalForSameSettings` invariant still holds because both saves in that test use the same `MigratedVersion`.
- **Bootstrap seed**: append `{name: "Dockerfile", content: assets.Dockerfile}` to `bootstrapFiles`. Existing `O_EXCL` loop handles create-if-absent and skip-if-exists.
- **migration runner** (`internal/config/migrate.go`):
  - `type migration struct { name string; run func(baseDir string) error }`
  - `var migrations = []migration{{name: "Dockerfile", run: writeDockerfile}}`
  - `writeDockerfile(baseDir)`: `MkdirAll(baseDir, 0o755)`; write `assets.Dockerfile` to `<baseDir>/Dockerfile` atomically (temp file via `os.CreateTemp(baseDir, "Dockerfile.tmp-*")`, write, close, `os.Rename`, perm `0o644`; cleanup temp on failure — mirror `Save`). Always overwrites.
  - `Migrate(baseDir) (applied bool, err error)`: `Load`; if `s.MigratedVersion == MigrationVersion` return `(false, nil)`; else run each step (`fmt.Errorf("migrate %q: %w", m.name, err)` on failure), set `s.MigratedVersion = MigrationVersion`, `Save`, return `(true, nil)`.
- **CLI** (`cmd/makeslop/main.go`): `migrateCmd` (cobra.NoArgs) → `applied, err := config.Migrate(baseDir)`; print `makeslop: ~/.makeslop updated` when applied else `makeslop: ~/.makeslop already up to date` to stdout; register via `rootCmd.AddCommand(initCmd, goCmd, migrateCmd)`. No persistent-flag/home interaction.

## What Goes Where
- **Implementation Steps** (`[ ]`): all code changes, file move, and tests within this repo.
- **Post-Completion** (no checkboxes): manual smoke test of the binary on a real `~/.makeslop`; documenting the two-step `init` + `migrate` first-run flow in README.

## Implementation Steps

### Task 1: Create `internal/assets` embed package and move the Dockerfile

**Files:**
- Create: `internal/assets/assets.go`
- Create: `internal/assets/files/Dockerfile` (moved from repo-root `Dockerfile`)
- Delete: `Dockerfile` (repo root)
- Create: `internal/assets/assets_test.go`

- [x] `mkdir -p internal/assets/files && mv Dockerfile internal/assets/files/Dockerfile` (plain `mv` — the root `Dockerfile` is untracked, so `git mv` would fail; there is no history to preserve)
- [x] create `internal/assets/assets.go`: `package assets`, `import _ "embed"`, `//go:embed files/Dockerfile`, `var Dockerfile []byte`, with a package doc comment
- [x] `git add internal/assets/files/Dockerfile` and verify it is tracked (`git ls-files internal/assets/files/Dockerfile` must print the path) — `//go:embed` reads the working tree locally, but a fresh clone / CI build fails to compile if the file is not committed
- [x] write `internal/assets/assets_test.go`: assert `len(assets.Dockerfile) > 0`
- [x] add test case: `assets.Dockerfile` starts with `FROM ` (`bytes.HasPrefix`) — guards against an empty/wrong embed
- [x] run tests: `go test ./internal/assets/...` — must pass before next task

### Task 2: Add `MigrationVersion` constant and `MigratedVersion` field to config

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

- [x] add constant `MigrationVersion = 1` to the const block (with comment distinguishing it from `CurrentVersion`)
- [x] add field `MigratedVersion int `json:"migrated_version,omitempty"`` to `Settings`
- [x] confirm `Load` does NOT default `MigratedVersion` (leave absent => 0); no code change expected, just verify
- [x] add/extend test in `config_test.go`: `Load` of a missing file yields `MigratedVersion == 0`
- [x] add test: a `settings.json` written without `migrated_version` round-trips with `MigratedVersion == 0` (backward compat); and `Save`→`Load` round-trips a set `MigratedVersion`
- [x] run tests: `go test ./internal/config/...` — must pass before next task

### Task 3: Seed Dockerfile on `init` via Bootstrap

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

- [x] add `{name: "Dockerfile", content: assets.Dockerfile}` to `bootstrapFiles` (import `internal/assets`)
- [x] verify `Bootstrap` writes it with the existing `O_EXCL` loop (create-if-absent, skip-if-exists); no loop change needed
- [x] write test: `Bootstrap` on a fresh `t.TempDir()` creates `<base>/Dockerfile` with content equal to `assets.Dockerfile`
- [x] write test: `Bootstrap` does NOT clobber a pre-existing `<base>/Dockerfile` (pre-write sentinel bytes, run `Bootstrap`, assert unchanged)
- [x] write test: `Bootstrap` leaves `migrated_version` unset (init must not stamp) — assert no `settings.json` written by Bootstrap (matches current behavior)
- [x] run tests: `go test ./internal/config/...` — must pass before next task

### Task 4: Migration runner — `writeDockerfile` and `Migrate`

**Files:**
- Create: `internal/config/migrate.go`
- Create: `internal/config/migrate_test.go`

- [ ] create `internal/config/migrate.go`: `migration` struct + ordered `migrations` slice with the single `Dockerfile` step
- [ ] implement `writeDockerfile(baseDir)`: `MkdirAll`, atomic temp-file + rename write of `assets.Dockerfile` to `<baseDir>/Dockerfile` (perm `0o644`, temp cleanup on failure — mirror `Save`); always overwrites
- [ ] implement `Migrate(baseDir) (applied bool, err error)`: skip when `MigratedVersion == MigrationVersion`; else run all steps (wrap errors `migrate %q: %w`), stamp `MigratedVersion = MigrationVersion`, `Save`
- [ ] write test: fresh `t.TempDir()` → `Migrate` returns `applied == true`, writes Dockerfile == `assets.Dockerfile`, and persisted `migrated_version == MigrationVersion`
- [ ] write test: second `Migrate` call on the now-stamped dir returns `applied == false` and leaves the file unchanged (skip path)
- [ ] write test: `Migrate` overwrites a locally-edited `<base>/Dockerfile` when version is behind (write sentinel + `Save` with `MigratedVersion=0`, run `Migrate`, assert content restored to `assets.Dockerfile`)
- [ ] write test: with `MigratedVersion` set to a different value than `MigrationVersion` (e.g. 0 or 999), `Migrate` re-runs (`applied == true`) and re-stamps to `MigrationVersion`
- [ ] write test: `Migrate` on a non-existent baseDir succeeds (writers `MkdirAll`) — covers the standalone `migrate` case
- [ ] write test: `Migrate` preserves other settings while stamping — pre-`Save` a settings.json with non-default `Image`/`Shell` and a populated `Workspaces` map plus `MigratedVersion=0`, run `Migrate`, then `Load` and assert `Image`/`Shell`/`Workspaces` are unchanged and `MigratedVersion == MigrationVersion` (guards against `Save` dropping fields)
- [ ] run tests: `go test ./internal/config/...` — must pass before next task; confirm the existing `TestSaveLoadByteIdenticalForSameSettings` still passes

### Task 5: Add the `migrate` CLI subcommand

**Files:**
- Modify: `cmd/makeslop/main.go`
- Modify: `cmd/makeslop/main_test.go`

- [ ] add `migrateCmd` (`Use: "migrate"`, `Args: cobra.NoArgs`, `SilenceUsage: true`) whose `RunE` calls `config.Migrate(baseDir)`
- [ ] print `makeslop: ~/.makeslop updated` when `applied`, else `makeslop: ~/.makeslop already up to date`, to `cmd.OutOrStdout()`
- [ ] register via `rootCmd.AddCommand(initCmd, goCmd, migrateCmd)`; do NOT wire `ensureWithinHome`/`--out-of-home` and do NOT call `Bootstrap`
- [ ] write test (using `runCmd` helper): first `migrate` on a fresh baseDir prints `updated` and creates `<base>/Dockerfile`
- [ ] write test: second `migrate` prints `already up to date` and leaves the file unchanged
- [ ] write test: `migrate` works without a prior `init` (no pre-created dirs) — succeeds and writes the Dockerfile
- [ ] write test: bare-invocation help (extend `TestRoot_BareInvocation_PrintsHelp` or add a case) lists the `migrate` command
- [ ] run tests: `go test ./cmd/...` — must pass before next task

### Task 6: Verify acceptance criteria
- [ ] verify `init` seeds Dockerfile if absent and never clobbers an existing one
- [ ] verify `migrate` is version-gated: runs when `migrated_version != MigrationVersion`, skips when equal, and force-overwrites on run
- [ ] verify `migrate` needs no home check and works from any cwd / without prior `init`
- [ ] verify the working-tree root `Dockerfile` is gone and `internal/assets/files/Dockerfile` is the only copy AND is git-tracked (`git ls-files internal/assets/files/Dockerfile`)
- [ ] run full test suite: `go test ./...`
- [ ] run vet/lint if available: `go vet ./...` and `golangci-lint run` (binary present in dev image)
- [ ] build check: `go build ./...`

### Task 7: [Final] Update documentation
- [ ] update `README.md` (git-tracked): document `makeslop migrate` and the first-run flow (`init` seeds, `migrate` refreshes on upgrade)
- [ ] move this plan to `docs/plans/completed/` (note: `docs/` is gitignored, so this is a local-only move)
- [ ] skip `CLAUDE.md` — it is empty AND gitignored, so edits would not be committed

## Post-Completion
*Items requiring manual intervention or external systems — no checkboxes, informational only*

**Manual verification**:
- Build the binary (`make build`) and run `makeslop init` in a scratch dir, then confirm `~/.makeslop/Dockerfile` exists and matches the embedded file.
- Run `makeslop migrate` twice; confirm `updated` then `already up to date`, and that `~/.makeslop/settings.json` gains `"migrated_version": 1`.
- Edit `~/.makeslop/Dockerfile`, set `migrated_version` back to `0` in settings, run `migrate`, confirm the edit is overwritten.

**Process note** (future migrations):
- When adding a new migration step, append it to `migrations` and bump `MigrationVersion`. Existing users' `migrate` will then re-run all (idempotent) steps and re-stamp.
