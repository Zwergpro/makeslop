# Squash Config Versions (Break Backward Compatibility)

## Overview
Collapse the two parallel version concepts in `internal/config` — `CurrentVersion`
(settings-schema version) and `MigrationVersion` (one-shot directory-refresh gate) —
into a **single** `ConfigVersion` constant and a **single** `Settings.Version` field,
reset to baseline `1`. The `migrated_version` JSON key and the `MigratedVersion`
struct field are removed. The previously-dead `Version` field (written but never read)
takes over the migration-stamp role.

**Problem it solves:** the dual-version scheme is confusing maintenance overhead — two
constants to bump, two semantics to keep straight, and one of them (`Version` /
`CurrentVersion`) was never actually read. One number now governs both.

**Backward-compatibility break (intentional):** the JSON shape changes (`migrated_version`
gone). Existing installs all carry `version: 1`, so with `ConfigVersion = 1` they read as
"current" — `migrate` reports up-to-date and won't auto-refresh assets (the **accepted
footgun**). Escape hatch is the existing `makeslop build --refresh`. The next real asset
change bumps `ConfigVersion` to `2`, restoring normal migrate flow.

## Context (from discovery)

**Files/components involved:**
- `internal/config/config.go` — constants (`:21,:26`), `Settings` fields (`:44,:49`), fresh-Load default (`:69`)
- `internal/config/migrate.go` — `MigrationStatus` (`:73`), `Migrate` (`:87`), doc comments
- `internal/cli/init.go` — fresh-seed stamp (`:57`), stale nudge (`:63`)
- `internal/cli/status.go` — stale nudge (`:168`)
- Tests (7 files): `config_test.go`, `migrate_test.go`, `lock_test.go`, `configkeys_test.go`, `init_test.go`, `main_test.go`, `status_test.go`, `workspace_test.go`
- Docs: `CLAUDE.md`, `docs/architecture.md`, `docs/reference.md`, `docs/security.md`

**Related patterns found:**
- `MigrationStatus(s) (current, latest int, stale bool)` signature is unchanged — only the
  fields/constants it reads move. So `init.go:63` and `status.go:168` callers need **no edit**.
- The migration step list `migrations = []migration{{WriteDockerfile}}` is **untouched** —
  squashing collapses the *numbering*, not the steps.
- `config.WriteDockerfile` is reused by `build --refresh` (`build.go:30`) — the footgun escape hatch.

**Dependencies / complications identified (beyond original brainstorm scope):**
1. **No-file / legacy-file stamp semantics — the surviving field inherits `migrated_version`'s
   zero-default.** Old `migrated_version` defaulted to `0` on a missing or pre-migration file
   (regression-tested), so a bare machine was "stale" and `migrate` would bootstrap it. The old
   `version` field separately defaulted to `CurrentVersion` on a missing file (`config.go:69`) —
   but that field was dead. **Resolution (revised after plan-review): the surviving `version`
   field takes over the migration-stamp role INCLUDING its zero-default.** Drop the
   `Version: ...` line from `Load`'s no-file branch so a missing file yields `Version = 0`.
   Consequences: `migrate` on a *bare* machine still bootstraps (no behavior change); a legacy
   file with no `version` key still loads as `0` → still migrates; `config set` on a bare machine
   writes `version: 0` (stale → next `migrate` bootstraps + stamps). This avoids the bare-dir
   behavior regression and lets the unguarded migrate tests pass with a pure rename. The
   **real-install footgun is unchanged**: every existing on-disk `settings.json` already carries
   `version: 1` (old non-`omitempty` default), so at `ConfigVersion = 1` they read as current.
2. **Duplicate-struct-key compile hazard.** Several test literals set BOTH `Version: CurrentVersion`
   AND `MigratedVersion: N` in the same `&Settings{...}` (`migrate_test.go:82,:118,:161,:290,:360,:466`;
   `init_test.go:439-444`; `main_test.go:391-396`). A blind "rename both → `Version`" collapses them
   into a duplicate field name → **compile error**. Fix: drop the `Version: CurrentVersion` line and
   keep only the stamp value as the new `Version: N`.
3. **`MigrationStatus`-only upgrade/downgrade tests auto-skip at `ConfigVersion = 1`.** Tests guarded
   with `if MigrationVersion <= 1 { Skip }` (`migrate_test.go:346,:436`) go dormant — coverage returns
   when `ConfigVersion` bumps to `2`. Keep them (renamed), don't delete. **Distinct** from the
   `Migrate`-executing fresh-dir tests below, which are NOT guarded and must keep passing.
4. **Unguarded `Migrate`-executing tests stay green under option B.** `TestMigrate_FreshDir` (`:14`),
   `TestMigrate_AlreadyUpToDate` (`:42`), `TestMigrate_NonExistentBaseDirSucceeds` (`:257`) call
   `Migrate` on a bare/fresh dir and assert `applied == true` + Dockerfile written. Because the no-file
   default is now `Version = 0 < ConfigVersion`, `migrate` still bootstraps → these pass with a pure
   field rename (their seeds don't set `Version`). Only the `MigratedVersion`→`Version` reference at
   `:37-38` needs renaming.
5. **Two regression tests** (`config_test.go:391-403`, `:405-420`) assert the stamp is `0` on
   missing/legacy files — under option B that is now the `version` field's behavior, so they survive
   as renamed/merged assertions (see Task 1).
6. **`lock_test.go` monotone-counter off-by-one** (`:40-60`): seed sets `Version: CurrentVersion` (=1)
   and increments the stamp from `0`. Repointing `MigratedVersion++`→`Version++` without dropping the
   `Version: CurrentVersion` seed line makes the counter start at `1` → assertion `== goroutines` fails.
   Fix: drop the seed `Version` line so it zero-values.
7. **`config_test.go:25`** (`TestLoad_MissingReturnsEmptyDefaults`) asserts `Version == CurrentVersion`
   on a missing file — under option B that becomes `Version == 0`.
8. **Hard invariant:** the `version` JSON tag must stay **without `omitempty`** — the "legacy file with
   no version key → 0 → migrates" and "Save re-emits version:0" guarantees depend on it.
9. **Docs** document the dual-version model in prose + JSON examples — must be rewritten. Note the docs
   are already stale (`architecture.md:170` and the JSON examples show `MigrationVersion = 3` /
   `"migrated_version": 3` while code is at `4`); do not copy the stale numbers.

## Development Approach
- **Testing approach: Regular** (code first, then update tests) — this is a rename/collapse of
  existing tested behavior, not new logic; tests are updated to match the new single-version model.
- Complete each task fully before the next; run `go build ./...` and the package's tests after each.
- **Every task includes its test updates** — not optional.
- **All tests must pass before starting the next task.**
- Backward compatibility is **intentionally broken** — the goal is a clean single-version model,
  not graceful migration of the old JSON shape (old `migrated_version` keys are silently dropped).

## Testing Strategy
- **Unit tests:** required per task. The bulk of the work is updating existing unit tests in
  `internal/config` and `internal/cli` to the renamed symbols + new semantics.
- **No e2e/UI tests** in this project (CLI/Go only) — N/A.
- New test added: a `settings.json` carrying **both** `version` and `migrated_version` keys loads
  without error, ignores `migrated_version`, and drops it on the next `Save` (the brainstorm
  spot-check).

## Progress Tracking
- Mark completed items `[x]` immediately when done.
- Newly discovered tasks: `➕` prefix. Blockers: `⚠️` prefix.
- Keep this plan in sync if scope shifts during implementation.

## Solution Overview
Single source of truth: `ConfigVersion = 1`. Single persisted field: `Settings.Version`
(`json:"version"`). `MigrationStatus`/`Migrate` key off `Version` vs `ConfigVersion`. JSON output
loses `migrated_version`. Old files self-clean (`migrated_version` ignored on load, absent on next
save). Docs and tests rewritten to the single-version model.

## Technical Details

**Constant (config.go):**
```go
// ConfigVersion is the single version governing both the settings schema and the
// one-shot ~/.makeslop asset refresh. Bump when the embedded assets OR the Settings
// shape change; `migrate` re-runs all idempotent steps and re-stamps.
ConfigVersion = 1
```

**Struct (config.go) — drop `MigratedVersion`, keep `Version`:**
```go
type Settings struct {
    Version    int                  `json:"version"`
    Image      string               `json:"image,omitempty"`
    Shell      string               `json:"shell,omitempty"`
    TmpDirSize string               `json:"tmp_dir_size,omitempty"`
    Workspaces map[string]Workspace `json:"workspaces"`
}
```

**Load semantics (CHANGED):** **remove** the `Version: ...` assignment from the no-file branch
(`config.go:69`) so a missing file yields `Version = 0` (zero value); the present-file branch already
leaves `Version` as the JSON value (or `0` if the key is absent). Keep the `Image/Shell/TmpDirSize`
defaults in that branch untouched. **Do not add `omitempty`** to the `version` tag.

**Migrate / MigrationStatus:** replace every `MigratedVersion`→`Version`,
`MigrationVersion`→`ConfigVersion`, `CurrentVersion`→`ConfigVersion`.

## What Goes Where
- **Implementation Steps** (`[ ]`): all code, test, and doc edits — all in-repo.
- **Post-Completion** (no checkboxes): manual `migrate`-on-existing-install behavior note;
  the footgun is informational, not an action item.

## Implementation Steps

### Task 1: Collapse constants and struct field in `config.go`

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

- [x] Replace the `CurrentVersion = 1` and `MigrationVersion = 4` constants (`config.go:21,:26`) with a single `ConfigVersion = 1` plus the collapsed doc comment shown in Technical Details.
- [x] Remove the `MigratedVersion int json:"migrated_version,omitempty"` field (`config.go:49`); keep `Version int json:"version"` (`config.go:44`) — **no `omitempty`**.
- [x] **Remove** the `Version: CurrentVersion` line from the no-file branch (`config.go:69`) so a missing file yields `Version = 0`. Leave the `Image/Shell/TmpDirSize/Workspaces` defaults intact.
- [x] In `config_test.go`: rename `CurrentVersion`→`ConfigVersion` where it's used as the schema stamp in literals (`:140,:187,:215,:273,:313`).
- [x] Change `TestLoad_MissingReturnsEmptyDefaults` (`config_test.go:25-26`): assert `s.Version == 0` (not `ConfigVersion`) for a missing file.
- [x] Delete `TestLoad_MissingFile_MigratedVersionIsZero` (`config_test.go:391-403`) — now redundant with the `:25` missing-file `Version == 0` assertion above.
- [x] Rewrite `TestLoad_LegacyConfig_MigratedVersionIsZero` → `TestLoad_LegacyConfig_NoVersionKeyIsZero` (`config_test.go:405-420`): seed a body **without** a `version` key (`{"image":"claudebox","shell":"/bin/zsh","workspaces":{}}`) and assert `s.Version == 0` (ancient installs still migrate).
- [x] Repoint `TestSaveLoad_MigratedVersionRoundTrips` (`config_test.go:422-442`): drop the `Version: CurrentVersion` + `MigratedVersion: MigrationVersion` duplicate-key pair down to a single `Version: ConfigVersion`, and assert the `version` field round-trips. Rename the test (`...VersionRoundTrips`).
- [x] `go build ./internal/config/ && go test ./internal/config/` — must pass before Task 2.

### Task 2: Repoint `migrate.go` to the single version

**Files:**
- Modify: `internal/config/migrate.go`
- Modify: `internal/config/migrate_test.go`

- [x] `MigrationStatus` (`migrate.go:73-78`): read `s.Version` / `ConfigVersion`.
- [x] `Migrate` (`migrate.go:87-119`): early-skip `s.Version >= ConfigVersion`; Update stamp `fresh.Version >= ConfigVersion` guard and `fresh.Version = ConfigVersion`.
- [x] Update all doc comments in `migrate.go` referencing `MigratedVersion`/`MigrationVersion` (lines `18-22`, `30`, `71`, `74-75`, `81-85`, `93`, `106-109`, `122-123`) to the single-version model; rename `errAlreadyStamped`'s message text ("settings already at ConfigVersion").
- [x] **Group A — `Migrate`-executing fresh-dir tests** (`TestMigrate_FreshDir` `:14`, `TestMigrate_AlreadyUpToDate` `:42`, `TestMigrate_NonExistentBaseDirSucceeds` `:257`): pure rename of the `MigratedVersion`→`Version` / `MigrationVersion`→`ConfigVersion` references (e.g. `:37-38`). They keep passing because the no-file default is now `Version = 0` so `migrate` still bootstraps. Do **not** restructure them.
- [x] **Group B — duplicate-key literals** (`:82,:118,:161,:290,:360,:466`): each `&Settings{...}` sets BOTH `Version: CurrentVersion` and `MigratedVersion: N`. Collapse to a single `Version: N` (drop the `Version: CurrentVersion` line; keep the stamp value — `0`, `999`, `previousVersion`, etc.). Then rename remaining `MigrationVersion`→`ConfigVersion` references.
- [x] **Group C — `MigrationStatus`-only upgrade/downgrade tests** (`:340` upgrade-from-1, `:434` stale-behind-1, `:452` downgrade): rename symbols; their `if ConfigVersion <= 1 { Skip }` guards make them dormant at baseline — verify they **skip** cleanly (not fail) and reactivate when `ConfigVersion` bumps.
- [x] Add `TestMigrate_BareDir_BootstrapsAndStamps` (or fold into `TestMigrate_FreshDir`): assert `Migrate` on a dir with no `settings.json` returns `applied == true` and stamps `Version == ConfigVersion` — locks in the bare-machine bootstrap behavior under option B.
- [x] `go test ./internal/config/` — must pass (Group C skips allowed) before Task 3.

### Task 3: Fix `lock_test.go` and `configkeys_test.go`

**Files:**
- Modify: `internal/config/lock_test.go`
- Modify: `internal/config/configkeys_test.go`

- [x] `lock_test.go`: repoint the monotone-counter logic from `MigratedVersion++` to `Version++` (`:40-41`) and the final assertion (`:58-60`); **drop the `Version: CurrentVersion` seed line** (`:16`) so the counter starts at `0` and `final.Version == goroutines` holds (off-by-one fix). Update the `// MigratedVersion doubles as a monotone counter` comment.
- [x] `configkeys_test.go`: rename `CurrentVersion`→`ConfigVersion` (`:208`, the `defaultSettings()` helper literal). This file is a helper, not a key-set assertion — no expected-keys list to edit.
- [x] Add new test `TestLoad_DropsLegacyMigratedVersionKey` (in `config_test.go` or `configkeys_test.go`): seed `{"version":1,"image":"claudebox","shell":"/bin/zsh","workspaces":{},"migrated_version":4}`, `Load` it (no error, `Version == 1`), `Save` it, then read the raw file bytes and assert `migrated_version` is absent.
- [x] `go test ./internal/config/` — must pass before Task 4.

### Task 4: Update CLI consumers and their tests

**Files:**
- Modify: `internal/cli/init.go`
- Modify: `internal/cli/init_test.go`
- Modify: `internal/cli/main_test.go`
- Modify: `internal/cli/status_test.go`

- [x] `init.go:57`: `s.MigratedVersion = config.MigrationVersion` → `s.Version = config.ConfigVersion`. Confirm `init.go:63` `MigrationStatus(...)` needs no change (signature unchanged).
- [x] `init_test.go`: rename `TestInit_FreshSeed_StampsMigratedVersion`→`...StampsVersion` and its assertions (`:387,:420-421`); `TestInit_AfterBuild_TreatedAsFresh` (`:525-527`) is a pure rename (fresh seed stamps `Version=ConfigVersion`, file persists `version:1`, assertion holds). For the stale-config test (`:435-468`): **collapse the duplicate-key literal at `:439-444`** (`Version: CurrentVersion` + `MigratedVersion: staleMigrated`) to a single `Version: 0` to force staleness — at baseline `ConfigVersion=1` a `version:1` seed would NOT be stale and the nudge assertion would fail. Update the `staleMigrated`/`after.MigratedVersion` references (`:466-468`) to `Version`. Drop the `if MigrationVersion == 0 { Skip }` guard (`:435-436`) since `0 < ConfigVersion(1)` is now the forced-stale seed.
- [x] `main_test.go`: **collapse the duplicate-key literal at `:391-396`** (`Version: CurrentVersion` + `MigratedVersion: 0`) to a single `Version: 0`; rename the re-seed at `:410` (`s.MigratedVersion = 0` → `s.Version = 0`); drop/adjust the `if MigrationVersion == 0 { Skip }` guard (`:387-388`).
- [x] `status_test.go`: rename symbols (`:171-172,:183`); seed `Version: 0` (not `MigratedVersion: 0`) to force the stale check; drop/adjust the `:171` skip guard.
- [x] `go test ./internal/cli/` — must pass before Task 5.

### Task 5: Fix `workspace_test.go`

**Files:**
- Modify: `internal/workspace/workspace_test.go`

- [x] Rename `config.CurrentVersion`→`config.ConfigVersion` (`:196,:230,:258,:565,:626`).
- [x] `go test ./internal/workspace/` — must pass before Task 6.

### Task 6: Update documentation

**Files:**
- Modify: `CLAUDE.md`
- Modify: `docs/architecture.md`
- Modify: `docs/reference.md`
- Modify: `docs/security.md`

- [x] `CLAUDE.md`: rewrite every dual-version explanation to the single `ConfigVersion = 1` model — the "MigrationVersion-on-Dockerfile-change rule" section, the struct-DI "Current values" note, the Dockerfile-hardening maintenance rule, and the cache-block note. Replace "bump MigrationVersion / CurrentVersion is separate" guidance with "bump `ConfigVersion`".
- [x] `docs/architecture.md`: rewrite the constants block (`:169-170`), the JSON example (`:177-178` — drop `migrated_version`), and the two bullet explanations (`:183-192`) into a single-version description.
- [x] `docs/reference.md`: update the `MigrationVersion`/`migrated_version` prose (`:46,:93,:145,:191`), the JSON example (`:451,:456` — drop `migrated_version`), and the field docs (`:461-466`) to the single `version`/`ConfigVersion` model.
- [x] `docs/security.md`: update the `MigrationVersion` bump guidance (`:331`) to `ConfigVersion`.
- [x] No test for docs — verify by re-grep in Task 7.

### Task 7: Verify acceptance criteria

- [x] `grep -rn "CurrentVersion\|MigrationVersion\|MigratedVersion\|migrated_version" --include="*.go" --include="*.md" .` returns **zero** matches. (Remaining refs are in `TestLoad_DropsLegacyMigratedVersionKey` test body/comments and string literals — intentional per plan Task 3; no production code uses the old symbols.)
- [x] `go build ./...` passes.
- [x] `go test ./...` passes (auto-skips in the upgrade/downgrade-path tests are acceptable at baseline).
- [x] `go vet ./...` clean.
- [x] Manually confirm the spot-check test from Task 3 passes (legacy file with `migrated_version` loads and drops the key on save).
- [x] Verify `makeslop build --refresh` still force-rewrites the Dockerfile (the footgun escape hatch) — read `build.go:29-34`, confirm path unchanged.

### Task 8: [Final] Close out

- [x] Re-read `CLAUDE.md` once more for any lingering dual-version phrasing.
- [x] `mkdir -p docs/plans/completed && git mv` this plan to `docs/plans/completed/`.

## Post-Completion
*Informational — no checkboxes.*

**Behavior preserved (option B):**
- `makeslop migrate` on a **bare** machine still bootstraps `~/.makeslop` — the no-file default is
  `Version = 0 < ConfigVersion`, so the migration runs and stamps. No behavior change vs. today.

**Behavior change to be aware of:**
- **The accepted footgun:** all existing installs carry `version: 1`, so at `ConfigVersion = 1`
  none are reported stale and `migrate` will not auto-refresh their Dockerfile. Users who need the
  latest embedded assets before the next `ConfigVersion` bump must run `makeslop build --refresh`.
  This resolves itself the next time `ConfigVersion` is bumped to `2`.
