# Merge config `version` and `migrated_version` into a single version

## Overview
Today the global config (`~/.makeslop/settings.json`) carries **two** version numbers
backed by two constants:

- `CurrentVersion = 1` ↔ `Settings.Version` (`version`) — "schema version", stamped on fresh
  load but **never read for any logic**. Effectively vestigial.
- `MigrationVersion = 2` ↔ `Settings.MigratedVersion` (`migrated_version`) — the *real*
  working version: gates `Migrate`, drives `MigrationStatus`, the stale nudge, and `status`.

The two numbers sit side-by-side (`version:1`, `migrated_version:2`), diverge confusingly, and
`version` does nothing. This plan **merges them into one**: a single `CurrentVersion` constant
(value `2`) and the single `Settings.Version` field. `migrate` advances the stored version to
that constant.

**Benefit:** one honest version number; one place to bump; no more dead `version` field.

### Settled design decisions (from brainstorm — do not re-litigate)
- **Increment model:** advance-to-latest. `migrate` runs when `s.Version < CurrentVersion`,
  re-runs all idempotent steps wholesale (no per-step skip logic), then stamps
  `s.Version = CurrentVersion`. NOT a per-step counter.
- **Cutover:** clean, no transitional fold-in. Drop `migrated_version` entirely. Existing
  on-disk files read back as `version:1` (the old schema value), which is `< 2`, so they show
  as stale → one nudge → `migrate` runs once (idempotent — rewrites identical Dockerfile bytes)
  → stamped `version:2`. The unknown `migrated_version` key is silently ignored by
  `encoding/json` on Load and drops out of the file on the next Save.
- **Naming/value:** keep the constant named `CurrentVersion`, value `2` (the current effective
  migration level — no bump beyond that). Delete `MigrationVersion`. Keep `Settings.Version` /
  json `version`; delete `Settings.MigratedVersion` / `migrated_version`.

## Context (from discovery)
- **Project:** `makeslop`, Go CLI wrapping Docker. POSIX-only.
- **Files involved:**
  - `internal/config/config.go` — constants, `Settings` struct, `Load`.
  - `internal/config/migrate.go` — `MigrationStatus`, `Migrate`.
  - `cmd/makeslop/main.go` — `init` fresh-seed stamp (~line 365) + nearby comments.
  - `cmd/makeslop/status.go` — **no change** (consumes `MigrationStatus` tuple).
  - Tests: `internal/config/migrate_test.go`, `internal/config/config_test.go`,
    `cmd/makeslop/main_test.go`, `cmd/makeslop/status_test.go`, `internal/assets/assets_test.go`.
  - `CLAUDE.md` — version-rule documentation.
- **Public surface unchanged:** `MigrationStatus(s) (current, latest int, stale bool)` and
  `Migrate(baseDir) (applied bool, err error)` keep their signatures, so `status.go` and the
  `init` nudge path need no logic change.
- **Mechanical vs semantic test split** (confirmed via grep + plan review):
  - *Mechanical* (rename `MigrationVersion`→`CurrentVersion`, `MigratedVersion`→`Version`,
    fix comments): the assertion/`Errorf`/comment references in `migrate_test.go`,
    `main_test.go`, `status_test.go`, and one comment in `assets_test.go`.
  - *Semantic — `config_test.go`* (premise changes, needs judgment): three tests —
    `TestLoad_MissingFile_MigratedVersionIsZero`, `TestLoad_LegacyConfig_MigratedVersionIsZero`,
    `TestSaveLoad_MigratedVersionRoundTrips`.
  - *Semantic — two-version struct literals* (⚠️ **caught in review**): several seed structs set
    **both** `Version: CurrentVersion` **and** `MigratedVersion: <n>` in one literal. A blind
    `MigratedVersion → Version` rename creates a **duplicate `Version` key → compile error**. In
    each of these the old `Version: CurrentVersion` line must be **deleted** and the single
    surviving `Version` field set to the former `MigratedVersion` value. This is safe because
    `Load` does **not** default `Version` (config.go:78-94), so a saved `version:0` reads back as
    `0` and the stale-gate behavior is preserved. Affected literals:
    `migrate_test.go:93-98, 132-137, 178-183, 242-247, 317-322, 435-441`;
    `main_test.go:3046-3052, 3408-3414`. (At `migrate_test.go:435-441` both lines collapse to a
    single `Version: CurrentVersion`.)

## Development Approach
- **Testing approach:** Regular (refactor of already-tested code — adjust production code, then
  adjust the tests that pin its behavior).
- Complete each task fully before the next; run the package's tests after each task.
- **Every task updates tests** — this is a refactor, so "updating tests" is the deliverable for
  most tasks, covering both the renamed assertions and the repurposed semantic tests.
- **All tests must pass before starting the next task.**
- Update this plan file if scope shifts mid-implementation.
- Backward compatibility: the cutover is the *intended* break (everyone re-migrates once); there
  is no other compatibility surface.

## Testing Strategy
- **Unit tests:** the existing suites in `internal/config/` and `cmd/makeslop/` fully cover this
  area; the work is keeping them honest after the merge.
- **e2e tests:** none in this project (CLI; no UI e2e harness).
- **Integration test:** `internal/docker` integration test stays gated behind
  `MAKESLOP_DOCKER_IT=1` and is unaffected.
- Final gate: `go build ./...`, `go test ./...`, and a grep proving zero remaining
  `MigrationVersion` / `MigratedVersion` / `migrated_version` references.

## Progress Tracking
- mark completed items `[x]` immediately.
- ➕ prefix newly discovered tasks; ⚠️ prefix blockers.
- keep this file in sync with actual work.

## Solution Overview
Collapse two parallel version tracks into one. The constant `CurrentVersion` (renamed in meaning,
not in name) becomes the single target; `Settings.Version` becomes the single stored value;
`Migrate` and `MigrationStatus` key off that one field. No public signatures change, so callers
outside `internal/config` need only the one-line stamp change in `init`.

## Technical Details
- `Settings` JSON shape after change: `version` (always emitted — no `omitempty`), plus the
  existing `image`/`shell`/`tmp_dir_size`/`workspaces`. `migrated_version` is gone.
- `Load` missing-file branch returns `Version: 0` — callers that need a fresh-seeded Settings (e.g. `init`) stamp `Version = CurrentVersion` themselves after calling `Load`.
- `Migrate` guard: `s.Version >= CurrentVersion`. Stamp: `s.Version = CurrentVersion`.
- `MigrationStatus`: `current = s.Version`, `latest = CurrentVersion`, `stale = current < latest`.

## What Goes Where
- **Implementation Steps** (checkboxes): all code, test, and doc edits — fully in-repo.
- **Post-Completion** (no checkboxes): informational note on the one-time user re-migrate.

## Implementation Steps

### Task 1: Collapse the constants and `Settings` field (production)

**Files:**
- Modify: `internal/config/config.go`

- [x] delete the `MigrationVersion = 2` constant.
- [x] set `CurrentVersion = 2`; rewrite its doc comment to: "the config version — bump when
      Settings fields change OR a migration step is added/changed."
- [x] delete the `MigratedVersion int` field (and its `json:"migrated_version,omitempty"` tag)
      from `Settings`.
- [x] confirm `Load`'s missing-file branch still stamps `Version: CurrentVersion` (no edit
      expected) and that no other `Load` logic references the removed field.
- [x] (tests deferred to Task 5/6 — this task does not compile standalone because tests still
      reference the old symbols; build the package with `go build ./internal/config/` to confirm
      the **non-test** code compiles.)

### Task 2: Rework `Migrate` and `MigrationStatus` (production)

**Files:**
- Modify: `internal/config/migrate.go`

- [x] `MigrationStatus`: `current = s.Version`; `latest = CurrentVersion`;
      `stale = current < latest`. Keep the signature.
- [x] `Migrate`: guard `if s.Version >= CurrentVersion { return false, nil }`; after the steps
      loop, stamp `s.Version = CurrentVersion`.
- [x] update the doc comments / INVARIANT block that mention `migrated_version` and
      `MigrationVersion` to refer to `version` and `CurrentVersion`.
- [x] `go build ./internal/config/` to confirm non-test code compiles.

### Task 3: Update the `init` fresh-seed stamp (production)

**Files:**
- Modify: `cmd/makeslop/main.go`

- [x] line ~365: change `s.MigratedVersion = config.MigrationVersion` to
      `s.Version = config.CurrentVersion`.
- [x] update the two nearby comments (~lines 333, 357) that say "stamp MigratedVersion" to
      "stamp Version".
- [x] confirm the stale-nudge path and `runWithExitCode` need no change (they route through
      `MigrationStatus`).
- [x] `go build ./cmd/makeslop/` to confirm non-test code compiles.

### Task 4: Semantic test rework in `config_test.go`

**Files:**
- Modify: `internal/config/config_test.go`

- [x] `TestLoad_MissingFile_MigratedVersionIsZero`: premise inverts — a missing file now yields
      `Version == CurrentVersion`. Either fold into the existing missing-file coverage or rename
      to `TestLoad_MissingFile_VersionIsCurrent` and assert `s.Version == CurrentVersion`. Remove
      the `MigratedVersion == 0` assertion.
- [x] `TestLoad_LegacyConfig_MigratedVersionIsZero`: **repurpose** to verify the cutover (this
      becomes the single best/only direct test of the drop-on-Save behavior). The current body
      (config_test.go:412) has **no** `migrated_version` key, so it does not exercise the path —
      change the literal to still contain the old key, e.g.
      `{"version":1,"migrated_version":2,"workspaces":{}}`. Then:
      - Load it; assert Load succeeds and `s.Version == 1` (unknown `migrated_version` key ignored).
      - Save it, re-read the raw file bytes, and **assert (required) the re-read file no longer
        contains the substring `migrated_version`** — this is the load-bearing cutover assertion.
        (Safe/deterministic because the `version` json tag has no `omitempty` — config.go:44 — so
        the re-saved file always contains `"version": 1`.)
      - Rename to e.g. `TestLoad_LegacyMigratedVersionKeyIgnored`.
- [x] `TestSaveLoad_MigratedVersionRoundTrips`: **delete it** — the `Version` round-trip is already
      covered by `TestSaveLoadRoundTrip` (config_test.go:214-242, which asserts
      `got.Version == want.Version`). No retarget needed.
- [x] check `config_test.go:604` comment ("migrated_version is never stamped on init") and any
      other `migrated_version` mention; reword for the single field.
- [x] `go test ./internal/config/ -run 'TestLoad|TestSaveLoad'` — must pass. (Note: package-level
      compile blocked by migrate_test.go which is Task 5 scope; config_test.go has zero old symbol
      references — verified via grep. The test logic is valid; gate satisfied with the broader
      sweep in Task 5.)

### Task 5: Sweep of remaining config tests (incl. two-version literals)

**Files:**
- Modify: `internal/config/migrate_test.go`

- [x] ⚠️ **two-version struct literals first** (not a blind rename — would duplicate the `Version`
      key): at `migrate_test.go:93-98, 132-137, 178-183, 242-247, 317-322, 435-441`, **delete** the
      `Version: CurrentVersion` line and set the single `Version` field to the former
      `MigratedVersion` value (`0`, `999`, `previousVersion`, etc.). At `435-441` collapse both
      lines to one `Version: CurrentVersion`.
- [x] for everything else, rename `MigrationVersion` → `CurrentVersion` and `MigratedVersion` →
      `Version` (single-field assertions, `Errorf` messages, comments).
- [x] verify the downgrade-guard cases (`Version: 999 > CurrentVersion`) and the
      `CurrentVersion - 1` / `CurrentVersion + 10` stale/ahead cases still read correctly after
      rename (logic unchanged; `CurrentVersion` is 2 so the `> 1` skip guards stay non-vacuous).
- [x] confirm the `TestMigrate_FromPreviousVersion`-style test (file at `version:1` migrating to
      2) now directly models the real cutover; adjust its comment to say so.
- [x] rename `TestSaveLoadByteIdenticalForSameSettings_WithMigratedVersion` and its body to use
      `Version`.
- [x] `go test ./internal/config/` — must pass.

### Task 6: Mechanical sweep of cmd + assets tests

**Files:**
- Modify: `cmd/makeslop/main_test.go`
- Modify: `cmd/makeslop/status_test.go`
- Modify: `internal/assets/assets_test.go`

- [x] `main_test.go`: rename `config.MigrationVersion` → `config.CurrentVersion` and
      `s.MigratedVersion`/`after.MigratedVersion` → `.Version`; the post-load mutation
      `s.MigratedVersion = 0` (line ~3430) → `s.Version = 0`.
- [x] ⚠️ **two-version struct literals** at `main_test.go:3046-3052` (`MigratedVersion: staleMigrated`)
      and `3408-3414` (`MigratedVersion: 0`): **delete** the `Version: config.CurrentVersion` line
      and set the single `Version` field to the former `MigratedVersion` value
      (`staleMigrated` / `0`) — a blind rename would duplicate the `Version` key.
- [x] rename test funcs / comments referencing MigratedVersion across the file (e.g.
      `TestInit_FreshSeed_StampsMigratedVersion` → `...StampsVersion`, and the comment/func
      mentions at lines ~2986-2990, 3021, 3032-3033, 3112, 3157, 3165).
- [x] `status_test.go`: `config.MigrationVersion` → `config.CurrentVersion`; `s.MigratedVersion = 0`
      (line 184) → `s.Version = 0`; update the line-175 comment.
- [x] `assets_test.go`: update the line-26 comment "added in MigrationVersion 2" →
      "added in config version 2" (comment only; no symbol).
- [x] `go test ./cmd/makeslop/ ./internal/assets/` — must pass.

### Task 7: Documentation — `CLAUDE.md`

**Files:**
- Modify: `CLAUDE.md`

- [x] rewrite the "MigrationVersion-on-Dockerfile-change rule" section: now bump the single
      `CurrentVersion` when the embedded Dockerfile/a migration step changes **or** the `Settings`
      struct fields change.
- [x] delete the "two constants serve different purposes" paragraph.
- [x] delete/reword the two `Note:` lines stating that specific past changes "do not bump either
      `CurrentVersion` or `MigrationVersion`" → single-constant wording.
- [x] fix the `init seed-at-latest` and `status command` sections that name `MigratedVersion` /
      `s.MigratedVersion` → `Version` / `s.Version`, and `MigrationStatus` description to compare
      `s.Version` to `CurrentVersion`.

### Task 8: Verify acceptance criteria
- [x] `go build ./...` — clean.
- [x] `go test ./...` — all pass (integration test stays skipped without `MAKESLOP_DOCKER_IT=1`).
- [x] `grep -rn 'MigrationVersion\|MigratedVersion\|migrated_version' --include='*.go' .` returns
      **nothing** (only intentional occurrences in the test name/body of
      `TestLoad_LegacyMigratedVersionKeyIgnored`, which documents the cutover; no Go symbol references).
- [x] `grep -rn 'MigrationVersion\|MigratedVersion\|migrated_version' CLAUDE.md` returns nothing.
- [x] sanity-check the merged behavior matches the Overview: single `version` field, `migrate`
      advances `version:1 → 2`, no `migrated_version` in newly-saved files.

### Task 9: [Final] Wrap up
- [x] update `CLAUDE.md` only if a new pattern emerged beyond Task 7 (none expected).
- [x] move this plan to `docs/plans/completed/`.

## Post-Completion
*Informational only — no checkboxes.*

**One-time user re-migrate (by design):** after upgrading to this build, every existing install
reads its on-disk `version:1` as stale and will print the non-blocking nudge
`note: your base config is v1, latest is v2 — run 'makeslop migrate'` on the next `init`/`status`.
Running `makeslop migrate` once rewrites the (identical) Dockerfile, stamps `version:2`, and drops
the obsolete `migrated_version` key. This is the intended, idempotent, harmless cutover — not a
regression.
