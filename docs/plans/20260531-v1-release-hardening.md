# makeslop v1 Release — Security/Correctness Hardening + OSS-Readiness

## Overview

Bring `makeslop` to a credible v1.0 open-source release as **an opinionated, sandboxed
runner for Claude Code + Codex** (explicitly *not* a generalized agent runner). Work is
sequenced in two phases:

- **Phase 1 — correctness/security**: make the existing features actually trustworthy
  (broaden secret masking, harden the egress proxy, multi-arch image, document the uid
  assumption).
- **Phase 2 — OSS-readiness**: license, CI, version command, remove the committed binary,
  user-facing README.

This solves the gap between "carefully built internal tool" and "publishable, trustworthy
OSS project": the security features are under-verified/under-scoped, and the repo is missing
the basics (LICENSE, CI, version stamping) plus carries a committed build artifact.

## Context (from discovery + brainstorm)

- **Project**: Go 1.26 CLI (`github.com/Zwergpro/makeslop`), cobra-based, ~9k lines (~60% tests).
- **Files/components involved**:
  - `internal/security/security.go` (+ `security_test.go`) — `.env` scan via `fd`/`fdfind`.
  - `internal/networks/proxy.go` (+ `proxy_test.go`) — host-side unix-socket→TCP forward proxy.
  - `internal/assets/files/Dockerfile` — embedded base image (amd64-only).
  - `internal/config/config.go` + `migrate.go` — settings, `MigrationVersion`, Dockerfile migration.
  - `cmd/makeslop/main.go` — cobra root + subcommands (no `version` cmd today).
  - `Makefile` (single build target), `README.md`, `.gitignore`.
  - New: `LICENSE`, `.github/workflows/ci.yml`.
- **Patterns found**:
  - Pure/impure split: `internal/docker/spec.go` pure + table-tested; side effects in `run.go`.
  - Crash-safe writes (temp-file + rename) in `Save`/`bootstrapFile`/`writeDockerfile`.
  - Trust boundary: every external/user path is `Rel`+`IsLocal`-checked to stay under root.
  - Table-driven tests throughout; `SkipNonPOSIX` for shim-dependent tests.
- **Dependencies identified**: `docker` CLI (required), `fd`/`fdfind` (required, kept), cobra, yaml.v3, x/term.
- **Decisions locked in brainstorm** (do not revisit):
  - Stay opinionated (Claude/Codex). No generalization of image/mounts.
  - Keep `fd` — widen its regex; do NOT rewrite to a native walk.
  - Proxy: keep unix-socket + `--network none` (enforced egress, "Option A"); fix verification/guards.
  - Secrets: broaden the default denylist.
  - uid 1000: document the assumption, defer remapping to post-1.0.
  - License: MIT.
  - **Out of scope**: generalizing beyond Claude/Codex; `testing.go`-in-production cleanup;
    CONTRIBUTING.md / issue templates.

## Development Approach

- **Testing approach**: Regular (code first, then tests) — matches the existing table-driven style.
- Complete each task fully before moving to the next; small, focused changes.
- **Every task with code changes MUST include new/updated tests** (success + error/edge cases),
  listed as separate checklist items.
- **All tests must pass before starting the next task.** Run with the project's required env:
  `GOTMPDIR=/home/user go test ./...` (the noexec-/tmp shim constraint from CLAUDE.md).
- Maintain backward compatibility: existing `settings.json` files must keep working; pure
  functions (`BuildSpec`/`Args`/`ShellCommand`) must remain pure.

## Testing Strategy

- **Unit tests**: required every task. Security regex and proxy probe-dial are pure/near-pure and
  fully table-testable. Run as `GOTMPDIR=/home/user go test ./...`.
- **e2e tests**: project has no automated e2e harness (needs real docker). The proxy egress
  verification is a **documented manual smoke test** (see Post-Completion), not an automated test.
- Lint: `golangci-lint run` must pass (CI enforces it).

## Progress Tracking

- Mark completed items `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix; blockers with ⚠️ prefix.
- Keep this file in sync with actual work; update if scope changes.

## Solution Overview

- **Secret masking**: a single broadened `fd` regex (basename-scoped alternation) covering the
  common secret-file shapes. No architecture change; the existing `--print0` parse and
  `Rel`+`IsLocal` trust filter are untouched.
- **Proxy**: keep the enforced-egress model. Add an upstream probe-dial in `Start` so a
  misconfigured proxy fails the launch loudly instead of silently black-holing requests. Scope is
  reframed in docs as "Node/undici clients only" (Claude Code + Codex both qualify).
- **Image**: derive Go arch at build time so the image builds on arm64 (Apple Silicon). Because the
  Dockerfile is embedded and refreshed via `migrate`, bump `MigrationVersion 1 → 2` so existing
  installs actually pick up the new Dockerfile.
- **OSS basics**: MIT LICENSE, GitHub Actions CI (with the `GOTMPDIR` workaround documented inline),
  `makeslop version` with ldflags stamping, removal of the committed binary, and a user-facing README.

## Technical Details

- **Secret regex** (basename-scoped, single `--regex` arg). `fd` matches basenames by default and
  uses the Rust `regex` crate (unanchored search, so each alternative must carry its own anchors —
  a bare `^\.env` would wrongly match `.envrc`). Each alternative is individually anchored, and the
  `.env` shape is split into "ends-in-.env" (backward-compatible with the old `\.env$`) plus
  "`.env.<suffix>`":
  `\.env$|^\.env\.|\.pem$|\.key$|^id_rsa|^id_ed25519|^\.npmrc$|^\.netrc$|^\.git-credentials$`
  - Masked: `.env` (`\.env$`), `local.env` (`\.env$`), `.env.local` (`^\.env\.`), `foo.pem`,
    `foo.key`, `id_rsa`, `id_rsa.pub` (`^id_rsa`), `id_ed25519`, `.npmrc`, `.netrc`, `.git-credentials`.
  - Not masked: `.envrc` (no `$` after `.env`, no dot), `environment`, `keyboard.txt`, `keyfile`.
  - This is a security boundary — the regex above is the exact string to ship; do not paraphrase it.
- **Proxy probe-dial**: in `Proxy.Start`, after binding the socket, dial the upstream
  (`net.Dialer.DialContext`, short timeout) once; on failure, tear down the listener/socket and
  return the error so `runGo` aborts before `docker run`. Keep `handle()`'s per-connection dial as is.
  The probe verifies **TCP reachability only** (a listening socket), not that the upstream speaks a
  valid HTTP CONNECT proxy protocol — full protocol validation is out of scope and would be
  over-engineering for a launch guard. Document this limitation in the `Start` doc comment.
  - **Existing-test impact**: ~6 proxy tests (proxy_test.go lines 64, 159, 181, 199, 290, 316) call
    `Start` with the dead upstream `127.0.0.1:1` and assert success; the probe-dial makes those error.
    They must be repointed at a live fake upstream (the suite already has a TCP-echo helper used at
    lines 93/124/254). This is a required part of Task 2, not an afterthought.
- **Dockerfile arch**: replace `go${GO_VERSION}.linux-amd64.tar.gz` with an arch resolved via
  `dpkg --print-architecture` (emits `amd64`/`arm64`, matching go.dev's `linux-<arch>` tarball
  naming). nodesource already serves multi-arch.
- **MigrationVersion**: `1 → 2` in `internal/config/config.go`; `migrate` re-runs `writeDockerfile`
  (already idempotent) and re-stamps. `CurrentVersion` (settings schema) is unchanged.
- **Version stamping**: `var version = "dev"` in `package main`; `makeslop version` prints it;
  `Makefile` passes `-ldflags "-X main.version=$(shell git describe --tags --always --dirty)"`.
- **CI env**: `GOTMPDIR=${{ github.workspace }}/gotmp` (executable, non-noexec) so shim tests in
  `run_test.go` / `main_test.go` pass.

## What Goes Where

- **Implementation Steps** (`[ ]`): all code/test/doc changes in this repo.
- **Post-Completion** (no checkboxes): the manual docker-based proxy egress smoke test, tag/release
  mechanics, and image publishing — all require external action or real docker.

## Implementation Steps

### Task 1: Broaden the secret-masking denylist

**Files:**
- Modify: `internal/security/security.go`
- Modify: `internal/security/security_test.go`

- [x] Replace the `--regex` value `\.env$` with the exact anchored alternation
      `\.env$|^\.env\.|\.pem$|\.key$|^id_rsa|^id_ed25519|^\.npmrc$|^\.netrc$|^\.git-credentials$`
      in the `argv` slice; keep all other flags, `--print0` parsing, and the `Rel`+`IsLocal` filter.
- [x] Update the package/function doc comment on `Scan` to describe the denylist (not just `.env`).
- [x] **Update existing `TestScan_ArgvShape`** (security_test.go:167): change its expected `--regex`
      value from `\.env$` to the new alternation string (exact match).
- [x] **Update existing `TestScan_RealFd_MatchesExpectedFiles`** (security_test.go:~232): move
      `.env.local` from the NOT-included set to the included set; add fixtures `app.pem`, `server.key`,
      `id_rsa`, `id_rsa.pub`, `id_ed25519`, `.npmrc`, `.netrc`, `.git-credentials` to the included set;
      add `.envrc` and `environment` as negative (NOT-included) fixtures.
- [x] Add table-test cases asserting each new pattern is matched: `.env`, `.env.local`, `local.env`,
      `app.pem`, `server.key`, `id_rsa`, `id_rsa.pub`, `id_ed25519`, `.npmrc`, `.netrc`, `.git-credentials`.
- [x] Add negative table-test cases asserting these are NOT matched: `.envrc`, `environment`,
      `notes.txt`, `keyfile` (no extension).
- [x] Confirm the existing exclude dirs (`.git`, `node_modules`, `vendor`, `.venv`) still apply.
- [x] Run `GOTMPDIR=/home/user go test ./internal/security/...` — must pass before Task 2.

### Task 2: Probe-dial the upstream proxy on Start (fail loud)

**Files:**
- Modify: `internal/networks/proxy.go`
- Modify: `internal/networks/proxy_test.go`

- [x] In `Proxy.Start`, after the listener is bound and chmod'd, perform a single probe dial of
      `p.upstream` via `(&net.Dialer{}).DialContext(ctx, "tcp", p.upstream)` with a short timeout;
      close the probe conn immediately on success.
- [x] On probe failure: close the listener, `os.Remove` the socket, and return the dial error so the
      launch aborts (mirror the existing chmod-failure cleanup path).
- [x] Update the `Start` doc comment to state that an unreachable upstream aborts the launch, and
      that the probe checks TCP reachability only (not HTTP CONNECT protocol validity).
- [x] **Repoint existing dead-upstream tests**: change the `NewProxy(sock, "127.0.0.1:1")` calls at
      proxy_test.go lines 64, 159, 181, 199, 290, 316 to use a live fake upstream (reuse the existing
      TCP helper from lines 93/124/254) so `Start` still succeeds for the socket-lifecycle assertions.
      The bind-error test at line 221 (path-too-long) does not reach the probe, so leave its upstream.
- [x] Add a test: `Start` against an unreachable upstream returns an error and leaves no socket file.
- [x] Add a test: `Start` against a reachable (test-spawned `net.Listen("tcp")`) upstream succeeds and
      the accept loop still tunnels bytes end-to-end (extend/keep existing tunneling test).
- [x] Verify `Close` remains idempotent after a failed `Start` (existing behavior; assert in test).
- [x] Run `GOTMPDIR=/home/user go test ./internal/networks/...` — must pass before Task 3.

### Task 3: Multi-arch Dockerfile + MigrationVersion bump

**Files:**
- Modify: `internal/assets/files/Dockerfile`
- Modify: `internal/config/config.go`
- Modify: `internal/config/migrate_test.go`

- [x] In the Dockerfile, resolve the Go arch at build time (`ARCH=$(dpkg --print-architecture)`)
      and download `go${GO_VERSION}.linux-${ARCH}.tar.gz`; keep the cleanup/`go version` steps.
- [x] Bump `MigrationVersion` from `1` to `2` in `internal/config/config.go` (leave `CurrentVersion`).
- [x] Confirm `internal/assets/assets_test.go` still passes (it only checks non-empty + `FROM`
      presence, so it survives a content change).
- [x] Confirm the existing `migrate_test.go` suite survives the bump: its tests assert against the
      `MigrationVersion` symbol (not a literal `1`), so `_AlreadyUpToDate`, `_VersionAheadSkips`
      (uses `999`), etc. still hold. No edits expected — verify, don't assume.
- [x] Add a migrate test for the upgrade path: a baseDir stamped at `migrated_version: 1` triggers a
      migration (returns `applied=true`) and re-stamps to `2`; an already-`2` baseDir returns `applied=false`.
- [x] Run `GOTMPDIR=/home/user go test ./internal/config/... ./internal/assets/...` — must pass before Task 4.

### Task 4: Add `makeslop version` subcommand + ldflags stamping

**Files:**
- Modify: `cmd/makeslop/main.go`
- Modify: `cmd/makeslop/main_test.go`
- Modify: `Makefile`

- [x] Add `var version = "dev"` at package scope in `main.go`.
- [x] Add a `versionCmd` (cobra, `Args: cobra.NoArgs`, CI/pipe-safe — no TTY/home guard) that prints
      `version` to stdout; register it on the root command.
- [x] Update the `Makefile` build target to (a) build the package `./cmd/makeslop` instead of the
      single file `cmd/makeslop/main.go` (consistency with CI/acceptance), and (b) pass
      `-ldflags "-X main.version=$(shell git describe --tags --always --dirty)"`.
- [x] Add a test (in `package main`): set `version = "test-1.2.3"` in-test, run `makeslop version`,
      assert exit 0 and stdout equals `test-1.2.3\n` (deterministic — do not assert the ldflags value).
- [x] Confirm `version` is exempt from the home-directory guard and TTY check (assert it runs from a
      pipe / outside home).
- [x] Run `GOTMPDIR=/home/user go test ./cmd/...` — must pass before Task 5.

### Task 5: Add MIT LICENSE and remove the committed binary

**Files:**
- Create: `LICENSE`
- Modify: `.gitignore` (verify only)

- [x] Create `LICENSE` with the standard MIT text, year `2026`, copyright holder per the repo owner
      (`makeslop` / `github.com/Zwergpro`).
- [x] `git rm --cached makeslop` to untrack the committed build artifact (keep the local file).
- [x] **Add `/makeslop` to `.gitignore`** — it is NOT currently ignored (the file only has
      `.makeslop.yaml`). Use the root-anchored `/makeslop` so it does not affect the `cmd/makeslop/`
      package directory.
- [x] Confirm `git status` no longer shows `makeslop` as tracked, and a fresh `make build`-style
      repo-root binary (if any) is ignored.
- [x] (No unit tests for this task — license/VCS change only; note in the commit.)

### Task 6: Add GitHub Actions CI

**Files:**
- Create: `.github/workflows/ci.yml`

- [ ] Create a CI workflow triggered on push + pull_request: checkout, `setup-go` (1.26.x).
- [ ] Add a step exporting `GOTMPDIR` to an executable workspace path (e.g.
      `${{ github.workspace }}/gotmp`, `mkdir -p` first) with an inline comment explaining the
      noexec-/tmp shim constraint from CLAUDE.md.
- [ ] Steps: `go build ./cmd/makeslop`, `go test ./...` (with `GOTMPDIR` set), and
      `golangci-lint` (via the official action).
- [ ] Install the `fd-find` apt package in CI (Debian installs the binary as `fdfind`, which `Scan`
      already handles) so `TestScan_RealFd_MatchesExpectedFiles` actually runs instead of skipping —
      the real-binary coverage matters precisely because this release hardens the secret scanner.
- [ ] (No unit tests — CI config; validated by the workflow running green on the PR.)

### Task 7: README rewrite

**Files:**
- Modify: `README.md`

- [ ] Rewrite the top: one-line opinionated pitch ("a sandboxed runner for Claude Code + Codex") and
      a short quickstart (`init` → `migrate` → `build` → `go`).
- [ ] Update the "Secret masking" section to describe the broadened denylist (patterns + the
      masked/not-masked examples from Technical Details); reframe the "`fd` is non-negotiable" framing
      so the denylist is the story, `fd` is just the implementation detail.
- [ ] Reframe the proxy section: state **"Node/undici clients only"** as an explicit scope
      requirement (Claude Code + Codex qualify); note that an unreachable upstream now aborts the
      launch; replace the alarmist client-support table with a concise scope statement.
- [ ] Add a short "Host UID" note: container runs as uid 1000; works on Docker Desktop (macOS) and
      uid-1000 Linux hosts; full uid remapping is deferred to post-1.0.
- [ ] Trim internal-dev voice ("future milestones will store…") throughout; keep the reference detail
      (container layout, exit codes, config keys) below the quickstart.
- [ ] Document `makeslop version` in the usage list.

### Task 8: Verify acceptance criteria

- [ ] Verify all Phase 1 + Phase 2 items from Overview are implemented.
- [ ] Verify backward compatibility: an old `settings.json` (no `tmp_dir_size`, `migrated_version: 1`)
      still loads and `migrate` upgrades it to version 2.
- [ ] Verify pure/impure split intact: `internal/docker/spec.go` unchanged and still pure.
- [ ] Run the full suite: `GOTMPDIR=/home/user go test ./...`.
- [ ] Run `golangci-lint run` locally.
- [ ] Build for arm64 sanity (optional local check): `GOOS=linux GOARCH=arm64 go build ./cmd/makeslop`.

### Task 9: [Final] Update documentation and close out

- [ ] Update `CLAUDE.md` if any new pattern emerged (e.g. the `MigrationVersion`-on-Dockerfile-change
      rule, the proxy probe-dial invariant).
- [ ] Confirm README reflects the shipped behavior.
- [ ] Move this plan to `docs/plans/completed/`.

## Post-Completion

*Items requiring manual intervention or external systems — informational only, no checkboxes.*

**Manual verification**:
- **Proxy egress smoke test** (needs real docker, both arches if possible): start a host-side HTTP
  forward proxy (e.g. tinyproxy), set `network.proxy.address` in `.makeslop.yaml`, run `makeslop go`,
  and confirm from inside the container that Claude Code and Codex (Node/undici) reach the network
  through the unix socket — and that with a *bad* upstream address the launch now aborts with an
  error rather than hanging/black-holing.
- **arm64 image build**: `docker build` the embedded Dockerfile on an Apple Silicon / arm64 host and
  confirm the Go toolchain installs correctly.
- **Migration upgrade path**: on a machine with an existing `~/.makeslop` at `migrated_version: 1`,
  run `makeslop migrate` and confirm the Dockerfile is refreshed and the version stamped to 2.

**External system updates**:
- Tag and cut the `v1.0.0` GitHub release (drives `git describe` version stamping).
- (Optional, post-1.0) Publish a prebuilt base image to GHCR so first-run doesn't require a local
  multi-minute build.
