# Add `ls` and `remove` workspace-registry commands

## Overview
Add two new CLI subcommands to makeslop:

- `makeslop ls` — list all registered workspaces (name, path, created time) as an aligned table.
- `makeslop remove <name>` (alias `rm`) — unregister a workspace **by name** and delete its
  per-workspace cache directory (`~/.makeslop/workspaces/<name>/`).

Today there is no way to see which workspaces are registered, nor to undo `makeslop init`. `ls`
makes the registry discoverable; `remove` reverses `init`. The two are designed together: `remove`
takes a name, and `ls` is how the user learns the names.

Both commands operate on the `~/.makeslop/` registry only — they do not depend on the current
working directory. Like `migrate`/`config`/`status`, they are exempt from the home-directory guard
and the TTY requirement, and they touch no docker dependencies.

## Context (from discovery)
- **Settings model** (`internal/config/config.go`): `Settings.Workspaces map[string]config.Workspace`,
  keyed by absolute path; `Workspace{Name string, CreatedAt time.Time}`.
- **Workspace package** (`internal/workspace/workspace.go`): `Workspaces{baseDir}`, `New(baseDir)`,
  `Lookup`, `Init`, unexported `cacheDir(name)` = `filepath.Join(baseDir, config.WorkspacesDir, name)`,
  `workspaceName(absPath)` (basename + 6-char sha256). Cache dirs live at `~/.makeslop/workspaces/<name>`.
  `ErrNotRegistered` sentinel already exists.
- **Locked RMW** (`internal/config/lock.go`): `config.Update(baseDir, mutate func(*Settings) error) error`
  runs Load→mutate→Save under flock. No-nesting invariant: do not call `Update`/`WithLock` from inside
  a locked callback.
- **Command registration** (`internal/cli/root.go`): `newRootCmdWithDeps` builds `ws := workspace.New(baseDir)`
  and registers each `newXxxCmd`. Per-command file pattern (`init.go`, `migrate.go`, `config.go`, ...).
- **Guard helpers** (`internal/cli/guard.go`): `errSilent` (RunE already printed message → exit 1, no
  reprint), `quietWriter{w, quiet}` (gates stderr chrome). `ensureWithinHome` exists but is **not**
  needed here.
- **Chrome convention**: human-facing notices go to stderr via a `quietWriter`; primary data goes to
  stdout so it pipes cleanly. Errors always print (see `exitCodeFromError`).
- `migrate.go`/`config.go` are the closest precedents: non-pwd, home-guard-exempt, docker-free
  commands taking only `baseDir`.

## Development Approach
- **Testing approach**: Regular (code first, then tests) — matches the existing table-driven test
  style in the repo.
- Complete each task fully (including tests) before moving to the next.
- Make small, focused changes; run tests after each change.
- Maintain backward compatibility. **`ConfigVersion` is NOT bumped** — no `Settings` field change,
  no Dockerfile change.
- Every task includes new/updated tests covering success and error/edge scenarios; all tests must
  pass before starting the next task.

## Testing Strategy
- **Unit tests** required for every task (no e2e suite in this project).
- New `workspace.Remove` method tested in `internal/workspace/workspace_test.go`.
- New commands tested in `internal/cli/ls_test.go` and `internal/cli/remove_test.go`, driving the
  cobra command via `newRootCmdWithDeps` (or the existing `runWithExitCodeAndDeps` test helper) with
  injected stdout/stderr buffers and a temp `baseDir`.
- Test command: `go test ./...`. Vet/lint: `go vet ./...` (and `golangci-lint run` if configured).
- **Untested residual (accepted)**: the `os.RemoveAll` failure path in `remove` is not unit-tested —
  injecting an undeletable cache dir is impractical/non-portable. The behavior (wrap with path, return
  error) is simple and verified by reading; documented here rather than silently skipped.

## Progress Tracking
- Mark completed items `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix; blockers with ⚠️ prefix.
- Keep this plan in sync with actual work.

## Solution Overview
- **`workspace.Remove(name string) (cacheDir string, err error)`** is the single new behavior in the
  `workspace` package. It runs under raw `config.WithLock` (mirroring `Init`, **not** `config.Update`):
  Load settings, scan `s.Workspaces` for the entry whose `.Name == name`, delete that map key, Save. It
  returns the computed `w.cacheDir(name)` so the caller can delete the directory **after** the lock is
  released. On no match it returns `ErrNotRegistered` and does **not** Save. `config.Update` is
  deliberately rejected: the not-found path must return a sentinel without saving and the success path
  must surface a value (`cacheDir`) computed inside the lock — neither expresses cleanly through
  `Update`'s mutate-then-always-save shape. `Init` uses the same raw-`WithLock` form for the same reason.
- **Sentinel reuse**: `Remove` returns the existing `workspace.ErrNotRegistered`. Its wording
  (`"no workspace registered for path"`) is path-flavored, but that text is **never surfaced** — the CLI
  layer matches it with `errors.Is` and prints its own `no workspace named %q` message. No new sentinel
  is minted; do not "fix" the sentinel wording.
- **Ordering rationale**: settings (the source of truth) is mutated first under the short lock; the
  filesystem `os.RemoveAll` follows outside the lock. If `RemoveAll` fails, the entry is already
  unregistered and the FS error is surfaced — no half-state where the name still resolves.
- **Residual (documented, accepted)**: if `RemoveAll` fails the workspace is already unregistered, so
  re-running `remove <name>` hits `ErrNotRegistered` and cannot clean up the orphaned cache dir. The
  error message therefore includes the cache-dir path so the user can delete it manually. This mirrors
  `Init`'s "Save failure leaves the cache dir orphaned" tolerance (workspace.go:84).
- **`makeslop ls`**: load settings once, build sorted rows, render via `text/tabwriter` to stdout.
  Empty registry prints a friendly nudge to stderr chrome (suppressible by `--quiet`); stdout stays
  empty.
- **`makeslop remove`**: call `ws.Remove(name)`, then `os.RemoveAll(cacheDir)` (idempotent — nil if
  already gone), then print `removed <name>` to stderr chrome. Unknown name → print
  `no workspace named %q — run 'makeslop ls'` to stderr and return `errSilent`.

### Design decisions (from brainstorm, do not revisit)
- Remove identifies the workspace **by name**, not by pwd/path.
- Removal **always** deletes the cache dir — no flag, no opt-out.
- **No confirmation prompt** — deletes immediately (CI-safe).
- `ls` is an **aligned table only** — no `--json` (YAGNI; add later if needed).
- `remove` has alias `rm`.

## Technical Details
- `Remove` lives next to `Init` in `internal/workspace/workspace.go`; reuses `cacheDir`,
  `config.WithLock`, `config.Load`, `config.Save`, and `ErrNotRegistered`.
- Sorting in `ls`: collect `[]struct{Name, Path, Created string}` and `sort.Slice` by `Name`.
- Time formatting in `ls`: render `CreatedAt` with the fixed layout `"2006-01-02 15:04 UTC"`
  (`.UTC().Format(...)`), matching how `Init` stamps `time.Now().UTC()`. The Task 2 test asserts this
  exact format.
- `tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)`; header `NAME\tPATH\tCREATED`; `Flush()` at end.
- Both commands take only `(baseDir string)` except `remove`, which also needs `ws *workspace.Workspaces`
  (for `Remove`); `ls` can take `baseDir` alone and call `config.Load` directly (mirrors `config.go`),
  or take `ws` for symmetry — prefer `baseDir` only for `ls` to keep it dependency-light.
- Registration in `newRootCmdWithDeps`: add `newLsCmd(baseDir)` and `newRemoveCmd(ws, baseDir)` to the
  `rootCmd.AddCommand(...)` list.

## What Goes Where
- **Implementation Steps** (`[ ]`): the `Remove` method, the two command files, registration, tests,
  doc updates.
- **Post-Completion** (no checkboxes): manual smoke test against a real `~/.makeslop`.

## Implementation Steps

### Task 1: Add `Remove` method to the workspace package

**Files:**
- Modify: `internal/workspace/workspace.go`
- Modify: `internal/workspace/workspace_test.go`

- [x] Add `func (w *Workspaces) Remove(name string) (cacheDir string, err error)` mirroring `Init`'s
      `config.WithLock(w.baseDir, func() error {...})` structure: `config.Load`, scan
      `s.Workspaces` for the entry whose `.Name == name`, capture its map key, `delete(s.Workspaces, key)`,
      then `config.Save`.
- [x] On no match, return `("", ErrNotRegistered)` from inside the locked func and do **not** Save.
- [x] Set `cacheDir = w.cacheDir(name)` before returning success so the caller can delete the dir
      after the lock releases. Add a doc comment noting: the lock does not touch the filesystem (caller
      owns `os.RemoveAll`); `WithLock` is used over `config.Update` because the not-found path must skip
      Save and the success path returns an in-lock value; the reused `ErrNotRegistered` sentinel's
      path-flavored wording is intentionally never surfaced (the CLI prints its own message).
- [x] Write test: registering two workspaces, `Remove(name)` deletes only the matching settings entry
      and returns the correct cache dir path.
- [x] Write test: `Remove("does-not-exist")` returns `ErrNotRegistered` and leaves settings unchanged.
- [x] Run tests: `go test ./internal/workspace/` — must pass before next task.

### Task 2: Add `makeslop ls` command

**Files:**
- Create: `internal/cli/ls.go`
- Create: `internal/cli/ls_test.go`

- [x] Create `newLsCmd(baseDir string) *cobra.Command` with `Use: "ls"`, `Short`, `Args: cobra.NoArgs`,
      `SilenceUsage: true`.
- [x] In `RunE`: read `quiet, _ := cmd.Flags().GetBool("quiet")`; `config.Load(baseDir)`; detect empty
      via `len(s.Workspaces) == 0` (Load returns default `Settings{Workspaces: {}}` on a missing file —
      a load error is a real error, not the empty case); build a sorted `[]row{Name, Path, Created}`
      from `s.Workspaces`; if empty, write `no workspaces registered — run 'makeslop init'` to a
      `quietWriter` over `cmd.ErrOrStderr()` and return nil (stdout stays empty).
- [x] Otherwise render an aligned table to `cmd.OutOrStdout()` via `text/tabwriter` with header
      `NAME  PATH  CREATED`, sorted by name; format `CreatedAt` in UTC with a stable layout.
- [x] Write test: empty registry prints the nudge to stderr (and nothing to stdout), and `--quiet`
      suppresses the nudge.
- [x] Write test: multiple workspaces are listed sorted by name with all three columns populated
      (assert substrings / ordering; tolerate tabwriter spacing).
- [x] Run tests: `go test ./internal/cli/` — must pass before next task.

### Task 3: Add `makeslop remove` command

**Files:**
- Create: `internal/cli/remove.go`
- Create: `internal/cli/remove_test.go`

- [x] Create `newRemoveCmd(ws *workspace.Workspaces, baseDir string) *cobra.Command` with
      `Use: "remove <name>"`, `Aliases: []string{"rm"}`, `Short`, `Args: cobra.ExactArgs(1)`,
      `SilenceUsage: true`.
- [x] In `RunE`: call `cacheDir, err := ws.Remove(args[0])`; if `errors.Is(err, workspace.ErrNotRegistered)`,
      print `no workspace named %q — run 'makeslop ls'` to `cmd.ErrOrStderr()` and return `errSilent`;
      return any other error as-is.
- [x] After successful `Remove`, call `os.RemoveAll(cacheDir)`; if it errors, wrap it so the message
      includes `cacheDir` (entry is already unregistered and cannot be retried — the path lets the user
      delete it manually) and return the wrapped error.
- [x] On full success, write `removed <name>` to a `quietWriter` over `cmd.ErrOrStderr()`.
- [x] Write test: remove an existing workspace → settings entry gone AND cache dir gone on disk;
      `removed <name>` on stderr (suppressed under `--quiet`).
- [x] Write test: unknown name → message on stderr and non-zero exit via `errSilent`; settings unchanged.
- [x] Write test: idempotent FS — when the cache dir was already deleted manually, `remove` still
      succeeds (RemoveAll returns nil). Plus a test that the `rm` alias resolves to the same command.
- [x] Run tests: `go test ./internal/cli/` — must pass before next task.

### Task 4: Register both commands in the root command

**Files:**
- Modify: `internal/cli/root.go`

- [x] Add `newLsCmd(baseDir)` and `newRemoveCmd(ws, baseDir)` to the `rootCmd.AddCommand(...)` list in
      `newRootCmdWithDeps`.
- [x] Write/extend test: a root-level test asserts `makeslop ls` and `makeslop remove`/`rm` are
      registered and dispatch (e.g. via `runWithExitCodeAndDeps` or executing the cobra tree with
      injected buffers).
- [x] Run tests: `go test ./internal/cli/` — must pass before next task.

### Task 5: Verify acceptance criteria
- [x] `makeslop ls` lists registered workspaces sorted by name; empty registry shows the nudge and
      empty stdout.
- [x] `makeslop remove <name>` unregisters and deletes the cache dir; `rm` alias works; unknown name
      exits non-zero with a clear message; FS delete is idempotent.
- [x] Confirm both commands work with no live docker daemon and outside `$HOME` (home-guard/TTY exempt).
- [x] Run full suite: `go test ./...` and `go vet ./...`.

### Task 6: Update documentation
- [x] Update `docs/reference.md` to document `ls` and `remove` (including the `rm` alias, by-name
      argument, always-deletes-cache behavior, no-confirmation).
- [x] Add a short CLAUDE.md note: new `ls`/`remove` commands are home-guard- and TTY-exempt and
      docker-free (consistent with the documented command matrix in the run-only TTY / home-guard
      sections); `remove` always deletes the cache dir; `ConfigVersion` NOT bumped.
- [x] Move this plan to `docs/plans/completed/` (create the dir if needed).

## Post-Completion
*Informational only — manual / external steps.*

**Manual verification:**
- In a scratch `~/.makeslop`, run `makeslop init` in a temp project, `makeslop ls` (see it listed),
  `makeslop remove <name>`, then `makeslop ls` again (gone) and confirm
  `~/.makeslop/workspaces/<name>/` was deleted.
- Confirm `makeslop remove bogus` exits non-zero and prints the `run 'makeslop ls'` hint.
