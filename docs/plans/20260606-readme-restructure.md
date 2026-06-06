# README Restructure for GitHub Users

## Overview
Rewrite `README.md` so a GitHub visitor can read it top-to-bottom in one flow — from "what is
this / why care" (evaluator) through "install and run it" (new user) — in roughly 120–150 lines.
The current README (431 lines) is a full reference manual that mixes a 3-command quickstart with
exhaustive per-command flag docs, exit codes, the container mount table, secret-masking rules, and
network internals. That depth is valuable but buries the getting-started path.

The deep content is **relocated, not deleted**, into three themed companion files under `docs/`.
The README keeps only what a first-time reader needs and links out for everything else.

Benefits:
- A scannable landing page that answers "what / why / how do I start" immediately.
- Reference depth preserved and better organized by theme.
- Adds the currently-missing **Install** and **Requirements** sections.

## Context (from discovery)
- Project: **makeslop** — a sandboxed Docker runner for Claude Code + Codex agents.
- Module / repo: `github.com/Zwergpro/makeslop` (confirmed from `go.mod` and `git remote`).
- Files involved:
  - Modify: `README.md` (currently 431 lines, source of most relocated content).
  - Create: `docs/reference.md`, `docs/security.md`, `docs/architecture.md`.
  - Source-only (read, not modified): `CLAUDE.md` ("Key architectural patterns") for `architecture.md`.
- Release tooling: `.goreleaser.yaml` present → prebuilt binaries on GitHub Releases are a valid install path.
- Install command: `go install github.com/Zwergpro/makeslop/cmd/makeslop@latest`.
- This is a **documentation-only** change. No Go code is touched.

## Development Approach
- **Testing approach**: Regular (this is a docs change; "tests" = verification steps).
- Because no code changes, the per-task "write unit tests" requirement is adapted to
  **content-integrity verification**: after each file is written, confirm (a) no information from
  the old README was lost — every relocated block landed somewhere, and (b) all cross-links
  resolve to real files/anchors.
- Complete each file fully before moving to the next.
- Build the three `docs/` files **first** (they receive content), then rewrite the README last so
  its links point at files that already exist.
- Keep the original README open as the content source while relocating; do not paraphrase away
  technical accuracy — move text largely verbatim, lightly edited for its new home.
- **Exception to verbatim relocation — version schema:** the current README documents an old
  `version` + `migrated_version` (`MigrationVersion`) schema (lines ~35, 42-43, 91-92, 335-347),
  but commit `470a6a6` merged these into a single **`CurrentVersion=2`** field (see CLAUDE.md
  "CurrentVersion-on-change rule"). Do **not** relocate the stale schema verbatim — reconcile it
  against `internal/config/config.go` / CLAUDE.md and document the current single-field reality.
  If the implementer cannot confirm the field shape from code, flag with ⚠️ rather than guess.
- **Snapshot before rewrite:** before Task 4 edits `README.md`, capture the original
  (`git show HEAD:README.md` or a temp copy) so Task 5's no-loss diff has a stable baseline — the
  README is rewritten last and is the only source for most relocated content.

## Testing Strategy
- **Content-integrity check** (per task): diff the relocated section against the original README
  text to confirm nothing was dropped or silently altered.
- **Link check** (final task): every `docs/*.md` link in the README and any cross-file links
  resolve; no dead anchors.
- **Line-budget check** (final task): README is ~120–150 lines and reads top-to-bottom without
  requiring a jump to a reference file to complete a first run.
- **Render check** (final task): tables, code fences, and the diagram render as valid
  GitHub-flavored Markdown.

## Progress Tracking
- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope

## Solution Overview
Four files, built in dependency order (leaves first, README last):

1. `docs/reference.md` — the command/flag manual and config/runtime reference.
2. `docs/security.md` — the masking + network egress + home-guard story.
3. `docs/architecture.md` — the internals/design notes (sourced from CLAUDE.md + README internals).
4. `README.md` — rewritten landing page that links to the three above.

Content ownership map (where each current-README section goes):

| Current README content | Destination |
|---|---|
| Tagline, "what is this" | README (What & why) |
| Quickstart (init→build→run) | README (Quickstart), expanded detail → `reference.md` |
| Cache layout, Setup commands prose | `reference.md` |
| Per-command Usage deep-dives, flags | `reference.md` (Commands), one-line table → README |
| Requirements (daemon) | README (Requirements), full note → `reference.md` |
| Container layout / mount table | `reference.md` |
| In-container security flags (`--cap-drop ALL`, `no-new-privileges`, `--tmpfs`, bind-mount rationale) | `reference.md` (with mount table); 1-line cross-link from `security.md` |
| Host UID, TTY policy, Dry run | `reference.md` |
| Exit codes, Output conventions, Path resolution (distinct block — `EvalSymlinks`, `/private/tmp` aliasing) | `reference.md` |
| Docker container settings / `settings.json` schema (reconcile to single `CurrentVersion`) | `reference.md` |
| Secret masking, Project-local exclusions | `security.md` |
| Network egress two-state model | `security.md` |
| Home-directory guard | `security.md` |
| Readiness check (`status`) | README (1-line in Commands) + detail in `reference.md` |
| Build (`go build`/`go test`) | `architecture.md` (Contributing/Build) |
| Pure/impure split, apiClient seam, sidecar, BuildKit, scan engine (from CLAUDE.md) | `architecture.md` |

## Technical Details
- Cross-link style: relative links, e.g. `[command reference](docs/reference.md)` from README;
  `[secret masking](docs/security.md#secret-masking)` for anchored deep links.
- The README "How it works" diagram is a small fenced ASCII block (host → makeslop → container,
  with the optional proxy path noted), kept intentionally minimal; the detailed data-path diagram
  lives in `security.md`.
- `architecture.md` may reference CLAUDE.md content but should be self-contained for a reader who
  hasn't opened CLAUDE.md; CLAUDE.md remains the authoritative agent-facing notes and is not deleted.
- No content is invented: every technical claim in the new files traces to existing README/CLAUDE.md text.

## What Goes Where
- **Implementation Steps** (`[ ]`): create the three docs files, rewrite README, verify.
- **Post-Completion** (no checkboxes): optional follow-ups (screenshots/asciinema, Homebrew tap
  instructions, badges) that depend on assets/decisions outside this change.

## Implementation Steps

### Task 1: Create `docs/reference.md` (command + runtime reference)

**Files:**
- Create: `docs/reference.md`
- Read (source): `README.md`

- [x] add an intro line + table of contents for the reference file
- [x] relocate full per-command docs: `init`, `run`, `status`, `migrate`, `build`, `config`, `version` (with all flags: `--dry-run`/`-n`, `--out-of-home`, `--proxy`, `--no-cache`, `--build-arg`, `--json`, `--quiet`)
- [x] merge the "Setup commands" prose (init/migrate/build self-heal + stale-nudge) into the per-command docs — **one canonical description per command**, preserving the "migrate is an explicit upgrade step, not part of normal flow" framing
- [x] relocate Requirements (daemon/moby SDK note), Cache layout, container layout + mount table, in-container security flags (`--cap-drop ALL`, `no-new-privileges`, `--tmpfs`, bind-mount rationale), Host UID, TTY policy
- [x] relocate Dry run, Exit codes, Output conventions, Path resolution (keep as a distinct block)
- [x] relocate Docker container settings + `settings.json` schema — **reconcile version terminology**: confirmed from `internal/config/config.go` that both `version` (CurrentVersion=1) and `migrated_version` (MigrationVersion=2) are separate fields in `Settings`; documented both with clear distinction of purpose
- [x] verify content-integrity: every relocated block matches the original README text (no drops/edits of meaning), except the deliberately-corrected version schema

### Task 2: Create `docs/security.md` (masking + network + guard)

**Files:**
- Create: `docs/security.md`
- Read (source): `README.md`

- [x] add intro + table of contents
- [x] relocate Secret masking section (config-driven opt-in, default patterns/skip-dirs, walk-error fail-loud, pre-existing-project note, `masked N` notice, `.gitignore`-ignored rationale, symlinks-dropped detail)
- [x] add a 1-line cross-link back to the in-container security flags documented in `reference.md`
- [x] relocate Project-local exclusions (`exclude.scan` / `exclude.dirs` / `exclude.files`, reserved paths, merge behavior, example YAML)
- [x] relocate Network egress two-state model (direct default vs opt-in proxy, `--proxy`/`network.proxy.address`, socat sidecar + volume data path + diagram)
- [x] relocate Home-directory guard (default refusal, `--out-of-home` scope, exempt commands)
- [x] verify content-integrity against original README sections

### Task 3: Create `docs/architecture.md` (internals/design)

**Files:**
- Create: `docs/architecture.md`
- Read (source): `CLAUDE.md`, `README.md`

- [x] add intro framing this as design/internals for contributors
- [x] summarize key patterns from CLAUDE.md: pure/impure split (`spec.go` vs `run.go`/`build.go`), `apiClient` seam + `newClientFn`, `newSidecarFn` seam, preflight helpers
- [x] document the socat sidecar lifecycle and BuildKit session build flow
- [x] document the config-driven scan engine, `CurrentVersion`-on-change rule, POSIX-only invariant
- [x] relocate the README "Build" section (`go build ./cmd/makeslop`, `go test ./...`) as a Contributing/Build subsection
- [x] verify self-containment: a reader who never opens CLAUDE.md still understands the design

### Task 4: Rewrite `README.md` (landing page, single flow)

**Files:**
- Modify: `README.md`

- [x] snapshot the original README first (`git show HEAD:README.md > /tmp/readme-orig.md`) so Task 5's no-loss diff has a baseline
- [x] write header + tagline; "What & why" (2–3 sentences + short "why use it" bullets)
- [x] add Requirements (Docker daemon reachable) and Install (`go install` + GitHub Releases binaries)
- [x] write Quickstart (init→build→run, one line each) and "How it works" (4–5 sentences + minimal ASCII diagram)
- [x] write brief Configuration (`.makeslop.yaml` a user actually edits) and "Security at a glance" (1 paragraph)
- [x] add Commands one-line-per-command table + Documentation section linking `docs/reference.md`, `docs/security.md`, `docs/architecture.md`; add License line
- [x] verify README is ~120–150 lines and reads top-to-bottom without needing a reference file to finish a first run

### Task 5: Verify acceptance criteria
- [ ] confirm no information loss: diff against the `/tmp/readme-orig.md` snapshot and walk the content-ownership map; every old-README section exists in README or a docs file
- [ ] confirm version terminology is corrected everywhere: no surviving `migrated_version`/`MigrationVersion` references; `settings.json` schema shows the merged `CurrentVersion` field
- [ ] link check: all README→docs links and cross-file anchors resolve (no dead links)
- [ ] render check: tables, code fences, diagram are valid GitHub-flavored Markdown
- [ ] line-budget check: README within ~120–150 lines; evaluator→user flow is intact

### Task 6: [Final] Update documentation
- [ ] update CLAUDE.md only if a new doc-location convention is worth recording (e.g. "user docs live in docs/, agent notes in CLAUDE.md")
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion
*Items requiring manual intervention or external decisions — informational only*

**Optional enhancements** (not blocking this change):
- Add a terminal demo (asciinema/GIF) or screenshot to the README "How it works" section.
- Add status/CI/release badges once workflows are public.
- Add a Homebrew tap install line if/when a tap is published (GoReleaser can produce one).

**Manual verification:**
- Preview the rendered README + docs on GitHub (or a Markdown previewer) to confirm tables,
  the ASCII diagram, and relative links display correctly in the actual GitHub UI.
