# CLI UX Redesign

## Overview

Reshape the makeslop CLI surface for a cleaner first-run story and better discoverability,
applying the decisions approved in the brainstorm (memory: `cli-ux-redesign.md`). Pre-1.0, so
breaking changes are allowed (no aliases, no deprecation shims).

Key changes:
- Rename the `go` subcommand to `run`.
- Take `migrate` off the first-run path: `init` seeds `~/.makeslop/` at the latest
  `MigrationVersion`; a stale base config produces a **non-blocking** nudge (not a required step).
- Add a `status` command: an ordered dependency health check (daemon → base config → image →
  workspace → secret scan → proxy) with a verdict + single next-action line and a `--json` mode.
- `run` gains pre-flight checks (daemon reachable, image built) that each name their remedy.
- `config` with no subcommand shows settings instead of help.
- Scope `--out-of-home` to `init`/`run` only.
- Output conventions: stdout = machine result; stderr = human chrome; every actionable error ends
  with `— <remedy>`; add `--quiet`.

Benefits: the setup story collapses from `init → migrate → build → go` to **`init → build → run`**,
with `status` as the readiness oracle and clear, remedy-bearing errors throughout.

## Context (from discovery)

- **Project**: Go CLI (`cmd/makeslop`, cobra) that sandboxes AI agents in per-project Docker
  containers. POSIX-only.
- **Files/components involved**:
  - `cmd/makeslop/main.go` — cobra tree, `runGo`, `ensureWithinHome`, `runWithExitCode`. Most
    changes land here.
  - `internal/docker/client.go` — `apiClient` seam + `newClientFn`. New `Ping`/image-exists
    methods added here.
  - `internal/docker/testing.go` — `noopClient`, `FakeRunClient`, `FakeBuildClient` (shipped in the
    binary by design). New interface methods must be satisfied here.
  - `internal/docker/run.go` / `build.go` — side-effecting SDK calls; `ErrNoTTY`, `ExitError`.
  - `internal/config/config.go` — `MigrationVersion`, `Bootstrap`, `Load`, `Save`, `Settings`.
  - `internal/config/migrate.go` — `Migrate` (stamps `MigratedVersion`).
  - `internal/config/configkeys.go` — `ConfigList`/`ConfigSet`/`ConfigGet`.
  - `internal/workspace/workspace.go` — `Lookup`, `ErrNotRegistered`.
  - `internal/projectconfig/projectconfig.go` — `Load` (excludes + net cfg).
  - `internal/security/security.go` — `Scan` (used for the status scan summary).
  - `cmd/makeslop/main_test.go` — package-main tests using the docker fakes.
  - `README.md` — must be rewritten to the new surface.
- **Patterns found**:
  - Pure/impure split: pure argv assembly in `spec.go`; side effects behind the `apiClient` seam.
  - Test seam: `newClientFn` swapped via `SetClientForTest`; fakes embed `noopClient`.
  - Errors to stderr prefixed `makeslop: `; `errSilent` suppresses double-printing.
  - `Bootstrap` is idempotent and does **not** stamp `MigratedVersion` (fresh init currently leaves
    `MigratedVersion = 0`).
- **Dependencies identified**: `github.com/moby/moby/client v0.4.1`. Mostly option-struct method
  style (e.g. `ContainerCreate(ctx, moby.ContainerCreateOptions)`), but **`ImageInspect` is the
  exception** — it uses a variadic functional-option (`...moby.ImageInspectOption`), and `Ping`
  takes a `moby.PingOptions` arg. Signatures **verified** against `client@v0.4.1` source during
  planning (see Technical Details). Not-found detection uses `cerrdefs.IsNotFound` from
  `github.com/containerd/errdefs` (currently an indirect dep; Task 2 promotes it to direct).

## Development Approach

- **Testing approach**: Regular (code first, then tests) — matches the repo's table-driven style
  with `_test.go` files plus the shipped `testing.go` fakes.
- Complete each task fully before moving to the next.
- Make small, focused changes.
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task:
  - unit tests for new and modified functions, success + error scenarios;
  - new test cases for new code paths;
  - update existing tests when behavior changes.
- **CRITICAL: all tests must pass before starting the next task** — no exceptions.
- **CRITICAL: update this plan file when scope changes during implementation.**
- Run tests after each change.
- Pre-1.0: breaking changes are fine; no backward-compat shims for the renamed command.

## Testing Strategy

- **Unit tests**: required for every task (see Development Approach).
- **No UI/e2e**: this is a CLI with no browser e2e harness. The closest equivalent is the
  integration test gated by `MAKESLOP_DOCKER_IT=1` (`-tags integration`), which exercises the live
  daemon. New daemon/image checks should get a small gated integration assertion where practical,
  but the primary coverage is unit tests against the `apiClient` fakes.
- Treat the fake-backed command tests (`cmd/makeslop/main_test.go`) with the same rigor as unit
  tests: must pass before the next task.

## Progress Tracking

- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix.
- Document issues/blockers with ⚠️ prefix.
- Update the plan if implementation deviates from the original scope.

## Solution Overview

The redesign is layered so dependencies land first:

1. **Extend the `apiClient` seam** (`Ping`, image-exists) + update fakes. Everything else builds on
   this. A new exported `docker.CheckDaemon` and `docker.ImageExists` (or a small `Preflight` helper)
   wrap these so both `status` and `run` share one implementation.
2. **Rework `init`/`migrate` first-run semantics** in config + main: fresh init stamps
   `MigratedVersion`; a helper reports whether the base config is stale.
3. **Add `status`** consuming the daemon/image checks, the staleness helper, workspace lookup,
   scan summary, and proxy config.
4. **Rename `go` → `run`** and add its pre-flight checks (reusing the shared daemon/image helpers).
5. **Cross-cutting CLI polish**: `config` bare behavior, `--out-of-home` rescoping, `--quiet`,
   error-message rewording, `init` success message.
6. **Docs + acceptance**: rewrite README, verify, move plan to completed.

Design decisions & rationale:
- **Shared preflight helpers** avoid duplicating daemon/image logic between `status` and `run`
  (DRY here is justified — identical logic, no coupling cost).
- **Non-blocking staleness**: `init`/`status` report a stale base config but never fail on it, so
  `migrate` is opt-in.
- **Stamp-on-fresh-seed only**: `init` stamps `MigratedVersion` **only** when seeding a brand-new
  `~/.makeslop/`. If the directory already exists at an older version, init must NOT stamp (that
  would skip the real migration) — it nudges instead.

## Technical Details

### apiClient seam extension (`internal/docker/client.go`)

Add to the `apiClient` interface. **Verified against `github.com/moby/moby/client@v0.4.1`** (the
real signatures do NOT all follow the `moby.XxxOptions` struct convention — `ImageInspect` takes a
variadic functional-option):

```go
Ping(ctx context.Context, options moby.PingOptions) (moby.PingResult, error)
ImageInspect(ctx context.Context, imageID string, opts ...moby.ImageInspectOption) (moby.ImageInspectResult, error)
```

Notes:
- `Ping` requires a `moby.PingOptions` argument (pass the zero value). The plan's earlier draft
  omitted it.
- `ImageInspect` uses a **variadic functional-option** (`...moby.ImageInspectOption`); there is no
  exported `ImageInspectOptions` struct. The narrow seam interface and `noopClient`/fakes must
  accept and ignore the variadic. Call with no options for a plain existence lookup.
- Prefer `ImageInspect` for existence over `ImageList`+filter (single lookup, typed not-found).
- The compile-time assertion `var _ apiClient = (*moby.Client)(nil)` keeps the real client honest —
  it will fail loudly if the signatures are off, so add the methods and build BEFORE anything else.

### New exported preflight helpers (new file `internal/docker/preflight.go`)

```go
// CheckDaemon pings the daemon; returns a typed error when unreachable.
func CheckDaemon(ctx context.Context) error

// ImageExists reports whether image tag exists locally.
func ImageExists(ctx context.Context, image string) (bool, error)
```

Both construct the client via `newClientFn` (so the fakes apply) and `Close()` it. Define sentinel
errors `ErrDaemonUnreachable` and `ErrImageNotBuilt` (or return rich errors carrying the image tag /
DOCKER_HOST) for `run`/`status` to format remedies.

**Not-found detection (critical):** `ImageInspect` returns a typed error on a missing image. Use
`cerrdefs.IsNotFound(err)` from `github.com/containerd/errdefs` (the moby client's own tests use
exactly this predicate) to distinguish "image absent" (`return false, nil`) from any other error
(daemon down, permission, etc. → `return false, err`). **Do NOT** treat all errors as "not found" —
a dead daemon must surface as a daemon error, not a misleading "run 'makeslop build'" hint.
`containerd/errdefs v1.0.0` is currently an **indirect** dependency in `go.mod`; promote it to a
direct require (`go get github.com/containerd/errdefs`).

### Fakes (`internal/docker/testing.go`)

Add `Ping` and `ImageInspect` to `noopClient` (defaults: ping OK, image found). Add scripting fields
to `FakeRunClient`/`FakeBuildClient` (or a new `FakePreflightClient`) so tests can simulate:
daemon-down, image-missing, image-present. Keep existing fakes satisfying the wider interface.

### Config / init / migrate (`internal/config/config.go`, `migrate.go`)

- Add `func BaseConfigExists(baseDir string) (bool, error)` — true if `settings.json` exists.
- Add `func MigrationStatus(s *Settings) (current, latest int, stale bool)` reading
  `s.MigratedVersion` vs `MigrationVersion`.
- `init` flow change in `main.go`: detect fresh vs existing **before** `Bootstrap`. On fresh seed,
  after `Bootstrap`, set `s.MigratedVersion = MigrationVersion` and `Save`. On existing-but-stale,
  print the non-blocking nudge.

### status command output

Ordered checks, one aligned line each, glyphs `✓ ✗ – !`; final verdict line names the single next
action. Blocking checks: daemon, image, workspace. Non-blocking/info: stale config (`!`), scan
summary (`–`/`✓`), proxy (`–`). `--json` emits a struct of `{check, state, detail}` + overall
`ready`. Color/glyphs only when stderr is a TTY and `NO_COLOR` is unset. Exit non-zero if any
blocking check fails. CI/pipe-safe (no TTY requirement); exempt from the home guard.

### run pre-flight order (`runGo` → `runRun`)

1. home guard (existing) → 2. workspace lookup (existing `ErrNotRegistered`) → 3. `CheckDaemon`
(`— is docker running?`) → 4. `ImageExists` (`— run 'makeslop build'`, **no auto-build**) →
5. proxy probe (existing) → 6. TTY (existing `ErrNoTTY`). `--dry-run` skips daemon/image/TTY as
today (printed == executed for argv).

### Output conventions

- stdout: machine result only (paths, values, container output).
- stderr: progress, `masked N` notice, nudges, errors.
- Actionable errors: `makeslop: <what failed> — <remedy>`.
- `--quiet` (persistent): silences stderr chrome (notices/nudges/progress), keeps errors.
- `init` success → stderr: `registered <name> — run 'makeslop build' then 'makeslop run'`; stdout:
  bare cache path.

## What Goes Where

- **Implementation Steps** (`[ ]`): all code, tests, and the README rewrite — achievable in this repo.
- **Post-Completion** (no checkboxes): live-daemon manual smoke test, and confirming the exact moby
  `client` v0.4.1 method signatures if they differ from the assumed option-struct shape.

## Implementation Steps

### Task 1: Extend apiClient seam with Ping + ImageInspect and update fakes

**Files:**
- Modify: `internal/docker/client.go`
- Modify: `internal/docker/testing.go`
- Modify: `internal/docker/client_test.go`

- [x] **first**: add the two methods to the interface using the verified signatures —
      `Ping(ctx, moby.PingOptions) (moby.PingResult, error)` and
      `ImageInspect(ctx, imageID string, opts ...moby.ImageInspectOption) (moby.ImageInspectResult, error)` —
      and run `go build ./internal/docker/` so the `var _ apiClient = (*moby.Client)(nil)` assertion
      confirms them against the real client BEFORE writing any fakes
- [x] add `Ping` and `ImageInspect` no-op implementations to `noopClient` (defaults: ping ok, image
      found); the `ImageInspect` stub must accept and ignore the variadic `...moby.ImageInspectOption`
- [x] add scripting fields to the fakes so tests can simulate daemon-down (`PingErr`) and
      image-missing/present — image-missing MUST return an error satisfying `cerrdefs.IsNotFound`
      (e.g. wrap `errdefs.ErrNotFound`) so the Task 2 classification path is genuinely exercised
- [x] write tests asserting `*moby.Client` still satisfies `apiClient` and the fakes return scripted
      ping/image results (success + not-found + other-error cases)
- [x] run tests — must pass before next task

### Task 2: Add shared preflight helpers (CheckDaemon, ImageExists)

**Files:**
- Create: `internal/docker/preflight.go`
- Create: `internal/docker/preflight_test.go`
- Modify: `go.mod` (promote `github.com/containerd/errdefs` to a direct require)

- [x] promote `containerd/errdefs` to a direct dependency (`go get github.com/containerd/errdefs`)
- [x] create `preflight.go` with `ErrDaemonUnreachable`, `ErrImageNotBuilt` sentinels (carrying the
      image tag / endpoint detail for messaging)
- [x] implement `CheckDaemon(ctx)` — builds client via `newClientFn`, calls `Ping(ctx, moby.PingOptions{})`,
      closes client, maps failure to `ErrDaemonUnreachable`
- [x] implement `ImageExists(ctx, image)` — `ImageInspect(ctx, image)`; returns `(true,nil)` when
      found, `(false,nil)` only when `cerrdefs.IsNotFound(err)`, and `(false, err)` for any other
      error (so a dead daemon is never misreported as "image absent")
- [x] write tests using `SetClientForTest` with the Task 1 fakes: daemon ok/down; image present;
      image not-found (asserts `(false,nil)`); image other-error (asserts the error propagates, NOT
      mistaken for absent)
- [x] run tests — must pass before next task

### Task 3: Config helpers for fresh-seed stamping and migration staleness

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/migrate.go`
- Modify: `internal/config/config_test.go`
- Modify: `internal/config/migrate_test.go`

- [x] add `BaseConfigExists(baseDir string) (bool, error)` (existence of `settings.json`,
      distinguishing not-exist from other stat errors)
- [x] add `MigrationStatus(s *Settings) (current, latest int, stale bool)` comparing
      `s.MigratedVersion` to `MigrationVersion`
- [x] write tests for `BaseConfigExists` (present/absent/error) and `MigrationStatus`
      (fresh=stale-from-0, equal=not-stale, behind=stale)
- [x] run tests — must pass before next task

### Task 4: Rework `init` to seed-at-latest + non-blocking stale nudge

**Files:**
- Modify: `cmd/makeslop/main.go`
- Modify: `cmd/makeslop/main_test.go`

- [x] in the `init` RunE, call `BaseConfigExists` **before** `Bootstrap` to record fresh-vs-existing
- [x] on fresh seed: after `Bootstrap`, set `s.MigratedVersion = MigrationVersion` and `Save` (so a
      freshly-init'd dir is never reported stale)
- [x] on existing-but-stale (`MigrationStatus.stale`): print non-blocking nudge to stderr
      `note: base config is v<latest>, yours is v<current> — run 'makeslop migrate'` and continue
- [x] change `init` success output: stderr `registered <name> — run 'makeslop build' then 'makeslop run'`,
      stdout keeps the bare cache path
- [x] write tests: fresh init stamps MigratedVersion and prints registered/next-step; second init on
      stale dir prints the nudge and does not stamp; stdout still carries the bare path
- [x] write edge-case test: `build` then `init`. `build` calls `Bootstrap` (seeds dirs + Dockerfile)
      but never writes `settings.json`, so `BaseConfigExists` keys off `settings.json` presence and
      this path must be handled deliberately — decide and assert whether such a dir is treated as
      fresh (stamps latest) or stale-detectable, so init does not falsely stamp-as-latest over an
      already-seeded older Dockerfile
- [x] run tests — must pass before next task

### Task 5: Add the `status` command (human + --json)

**Files:**
- Create: `cmd/makeslop/status.go` (or add `newStatusCmd` within `main.go` if preferred for parity)
- Modify: `cmd/makeslop/main.go` (register command, wire into `newRootCmd`)
- Create: `cmd/makeslop/status_test.go`

- [x] implement an ordered check pipeline: daemon (`CheckDaemon`) → base config present + staleness
      (`BaseConfigExists`/`MigrationStatus`) → image (`ImageExists`) → workspace (`ws.Lookup`) →
      secret scan summary (`security.Scan` count) → proxy (`projectconfig.Load`)
- [x] add a small `cmd/makeslop`-local rendering helper (e.g. `glyphStyle(w io.Writer) styler`) that
      decides color/glyph use from an injectable `isTTY(w)` predicate AND `NO_COLOR`; keep the TTY
      check injectable so the renderer is unit-testable without a real PTY (per the POSIX-only /
      `SkipNonPOSIX` convention). Do NOT couple the branch directly to `os.Stderr`.
- [x] render aligned lines with glyphs `✓/✗/–/!`; final verdict line = state + single next action
- [x] add `--json` flag emitting `{checks:[{name,state,detail}], ready:bool}` (JSON path bypasses the
      glyph/color renderer entirely)
- [x] exit non-zero when any blocking check (daemon, image, workspace) fails; mark `status` exempt
      from the home guard and TTY requirement
- [x] write tests using fakes: all-green ready path (exit 0); daemon-down and image-missing
      (exit non-zero, correct verdict); stale-config emits `!` but stays ready; `--json` shape;
      renderer test with a forced non-TTY predicate asserting plain (no-glyph/no-color) output
- [x] run tests — must pass before next task

> Note (client lifecycle): `CheckDaemon` and `ImageExists` each build+close their own client, so a
> `status` run constructs the client twice. Accepted for DRY/simplicity (no shared-client coupling);
> revisit only if construction cost becomes visible.

### Task 6: Rename `go` → `run` and add run pre-flight checks

**Files:**
- Modify: `cmd/makeslop/main.go`
- Modify: `cmd/makeslop/main_test.go`
- Modify: `README.md` (command references touched here; full rewrite in Task 9)

- [x] rename the cobra command `Use: "go"` → `"run"` and update `Short`; rename `runGo` → `runRun`
      (and the closure wiring) — no alias kept
- [x] insert pre-flight before `docker.Run`: `CheckDaemon` (`— is docker running?`) then
      `ImageExists` (`makeslop: image '<image>' not built — run 'makeslop build'`, no auto-build),
      preserving existing order (home → workspace → daemon → image → proxy → TTY)
- [x] ensure `--dry-run` still skips daemon/image/TTY (printed == executed invariant intact)
- [x] update all tests referencing the `go` subcommand to `run`; add tests for daemon-down and
      image-missing pre-flight errors (exact remedy strings) and the happy path via `FakeRunClient`
- [x] run tests — must pass before next task

### Task 7: `config` bare shows settings; scope `--out-of-home`; add `--quiet`

**Files:**
- Modify: `cmd/makeslop/main.go`
- Modify: `cmd/makeslop/main_test.go`

- [x] change `configCmd` RunE to print `ConfigList` (key = value lines) instead of `cmd.Help()`
- [x] move `--out-of-home` from `rootCmd.PersistentFlags` to flags registered only on `init` and
      `run`; ensure `version`/`config`/`migrate`/`build`/`status` reject it
- [x] add a persistent `--quiet` flag and a small stderr-chrome gate so notices/nudges/progress are
      suppressed while errors still print
- [x] write tests: bare `config` prints settings; `version --out-of-home` errors as unknown flag;
      `--quiet` suppresses the `masked N` / nudge lines but not errors
- [x] run tests — must pass before next task

### Task 8: Error-voice pass — every actionable error names its remedy

**Files:**
- Modify: `cmd/makeslop/main.go`
- Modify: `cmd/makeslop/main_test.go`

- [x] audit (do NOT re-author) each user-facing error/notice in `main.go` for the format
      `makeslop: <what failed> — <remedy>` where a remedy exists (no-workspace, no-TTY, daemon,
      image, home-guard). Remedy strings set in Tasks 4/6 are already correct — normalize only;
      avoid churning the same lines twice
- [x] keep `errSilent` semantics (no double-print) intact
- [x] update/add tests asserting the remedy clause is present in each error string
- [x] run tests — must pass before next task

### Task 9: Rewrite README for the new surface

**Files:**
- Modify: `README.md`

- [x] update Quickstart to `init → build → run` (drop `migrate` from the happy path; document it as
      an explicit upgrade)
- [x] document `makeslop run` (was `go`), `makeslop status` (+ `--json`, exit codes), the `init`
      stale-config nudge, bare `config`, scoped `--out-of-home`, and `--quiet`
- [x] update the TTY-policy and home-guard sections to include `status` in the exempt/CI-safe lists
- [x] update the exit-codes section for `status`
- [x] no test (docs only)

### Task 10: Verify acceptance criteria

- [x] verify all Overview requirements are implemented (run/status/init-seeds-latest/config/flags/
      quiet/error-voice)
- [x] verify edge cases: fresh-vs-stale init, daemon-down, image-missing, `--dry-run` unaffected,
      `--json` output, `--out-of-home` rejected on exempt commands
- [x] run full suite: `go test ./...`
- [x] run vet/lint: `go vet ./...` and `golangci-lint run` (per `.golangci.yml`) — also fixed two
      pre-existing deprecation warnings: `NewClientWithOpts`→`New`, removed no-op `WithAPIVersionNegotiation`;
      simplified `b.fakeClient.wasStarted` → `b.wasStarted` (QF1008)
- [x] (optional, if a daemon is reachable) gated integration: `MAKESLOP_DOCKER_IT=1 go test -tags integration ./internal/docker/` — manual test (skipped - no live daemon in CI)

### Task 11: [Final] Update documentation and close out

**Files:**
- Modify: `CLAUDE.md`
- Modify: `docs/plans/20260531-cli-ux-redesign.md` (this file)

- [ ] update `CLAUDE.md`: note the renamed `run` command, the new `status` command + shared
      `preflight.go` helpers, the extended `apiClient` seam (Ping/ImageInspect) + fakes, the scoped
      `--out-of-home`, and the `init` seed-at-latest/stale-nudge behavior
- [ ] **specifically** update the migration-stamping invariant note in `CLAUDE.md`: it currently
      documents "Bootstrap does not stamp MigratedVersion" — record that `init` now stamps
      `MigratedVersion = MigrationVersion` on a fresh seed, so the documented contract stays truthful
- [ ] update the "TTY requirement is `go`-only" and home-guard-exemptions notes in `CLAUDE.md` to
      reflect `run`/`status`
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

*Items requiring manual intervention or external systems — informational only.*

**Manual verification**:
- Run against a live Docker daemon on macOS (Docker Desktop) and Linux: `status` all-green, `run`
  enters a shell, `run` with no image prints the build hint, daemon-stopped path prints the daemon
  remedy.
- Confirm glyph/color rendering in a real TTY and plain output when piped / under `NO_COLOR`.

**External / dependency confirmation**:
- moby `client@v0.4.1` `Ping`/`ImageInspect` signatures and the `cerrdefs.IsNotFound` predicate were
  verified during planning. If the pinned `moby/moby/client` version is ever bumped, re-check these
  signatures and the not-found predicate — the `var _ apiClient = (*moby.Client)(nil)` assertion
  will catch signature drift at compile time, but the not-found classification will not.
