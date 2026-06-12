# makeslop — Command & Runtime Reference

Complete reference for all `makeslop` commands, flags, runtime behavior, and configuration.

## Table of Contents

- [Requirements](#requirements)
- [Commands](#commands)
  - [init](#init)
  - [build](#build)
  - [run](#run)
  - [status](#status)
  - [migrate](#migrate)
  - [config](#config)
  - [version](#version)
- [Setup flow and self-healing](#setup-flow-and-self-healing)
- [Cache layout](#cache-layout)
- [Container layout and mount table](#container-layout-and-mount-table)
- [Environment variables](#environment-variables-environments-block-in-makeslopya-ml)
- [In-container security flags](#in-container-security-flags)
- [Host UID](#host-uid)
- [TTY policy](#tty-policy)
- [Dry run](#dry-run)
- [Exit codes](#exit-codes)
- [Output conventions](#output-conventions)
- [Path resolution](#path-resolution)
- [Docker container settings (settings.json)](#docker-container-settings-settingsjson)

---

## Requirements

- A Docker **daemon** must be reachable (via `DOCKER_HOST` or the default Unix socket
  `/var/run/docker.sock`). `makeslop` uses the moby/moby Go SDK directly; the `docker` CLI binary
  is **not** required.

---

## Commands

### init

Registers the current working directory as a workspace and seeds `~/.makeslop/` with initial files
(including `Dockerfile` and `settings.json`).

- On a fresh `~/.makeslop/` the directory is stamped at the current `ConfigVersion`
  (never stale after a fresh seed).
- On an existing but stale directory a non-blocking nudge is printed to stderr and `init`
  continues without modifying the existing files:
  ```
  note: your base config is v<current>, latest is v<latest> — run 'makeslop migrate'
  ```
- If `pwd` is already a subdirectory of a registered workspace, the existing workspace's cache path
  is returned (idempotent, no mutation).
- Otherwise a new entry is added to `settings.json`, the cache directory is created, and its
  absolute path is printed to stdout.
- Prints to stderr (suppressed by --quiet): `registered <name> — run 'makeslop build' then 'makeslop run'`
  (emitted on both fresh registration and re-init of an existing workspace).

**Flags:**
- `--out-of-home` — bypass the home-directory guard (see [security.md](security.md#home-directory-guard))
- `--global-only` — scaffold `.makeslop.yaml` with both per-workspace cache overlay groups disabled
  (only the global `~/.makeslop` mounts remain). This only affects a **fresh** scaffold:
  `Scaffold` is idempotent (EEXIST is success when the existing file is a regular file, never
  clobbers existing user edits), so on an already-init'd project the flag is a no-op — a note is
  not printed in that case, but the existing YAML is left unchanged. If `.makeslop.yaml` is a
  symlink, `init` exits with an error (see
  [security.md — symlinked .makeslop.yaml](security.md#breaking-change-symlinked-makeslopya-ml-rejected)).

---

### build

Builds (or rebuilds) the base Docker image from `~/.makeslop/Dockerfile` via the moby SDK.

- **Self-healing:** if `~/.makeslop/` has not been initialised yet, `build` seeds the base
  directory (runs `Bootstrap`) before building. Unlike `init`, it does not register a workspace or
  create `.makeslop.yaml` — so `makeslop build` works on a fresh machine but a subsequent `run`
  still requires `init`.
- The image tag defaults to `claudebox` (configurable via `settings.json`).
- **Empty context + BuildKit:** `build` passes an empty temporary directory as the Docker build
  context (the Dockerfile downloads everything; no local files need shipping) and uses the BuildKit
  API (`Version: "2"` via the moby SDK) so cache mounts (`--mount=type=cache`) work correctly. No
  `DOCKER_BUILDKIT` environment variable is needed.
- After a `migrate` that refreshes the `Dockerfile`, re-run `build` to pick up the changes.

**Flags:**
- `--no-cache` — bypasses the layer cache (passes `NoCache: true` to the SDK build call)
- `--build-arg K=V` — repeatable; each value is forwarded to the daemon as a build argument
  (use for proxy settings, version pins, etc.)
- `--refresh` — overwrite `~/.makeslop/Dockerfile` from the embedded assets before building.
  Use this to reset a hand-edited base Dockerfile to the shipped version without running a
  separate `migrate` step. Does **not** touch the `version` field or any migration state —
  `migrate` remains the sole owner of version tracking.

---

### run

From within a registered workspace, launches an interactive, project-scoped Docker container with
the workspace source tree mounted in. By default, per-workspace + global agent config
(`.claude/`, `.codex/`, `CLAUDE.md`, `docs/`) are also mounted as overlay groups; individual
groups can be disabled via `cache.content` and `cache.agent` in `.makeslop.yaml`. Static
environment variables can be injected via the `environments:` block — see
[Environment variables](#environment-variables-environments-block-in-makeslopya-ml).

- Exits with the container's exit code.
- Refuses to launch when stdin or stdout is not a TTY (see [TTY policy](#tty-policy)).
- If no ancestor directory is registered, exits non-zero with a hint to run `makeslop init`.
- Before launching, performs two pre-flight checks:
  1. Daemon reachability (`— is docker running?`)
  2. Image existence (`— run 'makeslop build'`). No auto-build.
- `--dry-run` skips both pre-flight checks and the TTY check (printed == executed invariant).

**Flags:**
- `--dry-run` / `-n` — print the equivalent shell command and exit without launching the container
- `--out-of-home` — bypass the home-directory guard

---

### status

Runs an ordered health check and reports the result. CI-safe; does not require a TTY.

Checks (in order):
1. Daemon reachability — **blocking**
2. Base config presence + staleness — absent/corrupt is blocking (`✗`), stale is non-blocking (`!`)
3. Image existence — **blocking**
4. Workspace registration — **blocking**
5. Secret scan summary — non-blocking

Each check emits one aligned line with a glyph (`✓ ✗ ! –`). A final verdict line names the next
action. Exit code is 0 when all blocking checks pass.

**Flags:**
- `--json` — emit `{"checks":[{"name","state","detail"}...],"ready":bool}`; exit code still
  reflects readiness

---

### migrate

Brings `~/.makeslop/` up to date with the current binary.

- Compares the binary's `ConfigVersion` constant against the `version` stored in
  `settings.json`. When they differ, runs all migration steps (force-overwrites
  `~/.makeslop/Dockerfile` from the embedded asset) and stamps the new version.
- On success prints `makeslop: ~/.makeslop updated` to stdout and exits 0.
- When already up to date, prints `makeslop: ~/.makeslop already up to date` to stdout and exits 0.
- Does not require a prior `init` (creates `~/.makeslop/` if it does not exist).
- `migrate` is an **explicit upgrade step**, not part of the normal setup flow. `init` always seeds
  `~/.makeslop/` at the latest version, so a freshly initialized directory is never stale.
- Exempt from the home-directory guard.

---

### config

Manages persistent settings in `~/.makeslop/settings.json`. Works without a prior `init`.

- `makeslop config` / `makeslop config list` — print all current effective settings as
  `key = value` lines.
- `makeslop config set <key> <value>` — validate and persist a setting.

**Configurable keys:**
- `image` — Docker image tag (default: `claudebox`)
- `shell` — shell to exec in the container (default: `/bin/zsh`)
- `tmp_dir_size` — size of the `/tmp` tmpfs (default: `100m`)

Accepted `tmp_dir_size` forms: `100m`, `2g`, `512k`, `1048576` (bare number = bytes).

`config set` self-heals via `Save`'s `MkdirAll` (creates `~/.makeslop/` if absent).
`config list` reads only; it does not write and does not create the directory.

---

### version

Prints the version string stamped at build time via
`-ldflags "-X main.version=$(git describe --tags --always --dirty)"`. Prints `dev` when built
without ldflags (e.g. via a plain `go build`).

---

## Setup flow and self-healing

Normal first-run order: `init` → `build` → `run`. After a binary update that ships a newer
Dockerfile: `migrate` → `build`.

`init` registers the workspace **and** seeds `~/.makeslop/` atomically, so a freshly initialized
directory is always stamped at the current `ConfigVersion` — never reported stale on first run.

`migrate` is the explicit upgrade path for existing installs. `build` self-heals `~/.makeslop/`
(seeds if absent), but does not register a workspace.

### Breaking change: `network:` block removed from `.makeslop.yaml`

Earlier versions of makeslop supported an optional egress-proxy feature configured via a `network:`
block in `.makeslop.yaml`:

```yaml
network:
  proxy:
    address: 10.0.0.5:3128
```

This feature has been removed. The `network:` block is now an **unknown field** and causes a hard
parse error that aborts `makeslop run` before Docker is contacted. If your `.makeslop.yaml` contains
a `network:` block, remove it to upgrade:

```
# Remove the network: block entirely from .makeslop.yaml
```

The app container now always uses standard Docker bridge networking with full internet access. No
socat sidecar, no `--network none`, and no `--proxy` flag.

### Breaking changes: `.makeslop.yaml` validation tightened

Two additional hard errors were added for invalid `.makeslop.yaml` configurations that were
previously silent (and silently lost secret masking). Full details and migration instructions are
in [security.md](security.md#project-local-exclusions).

**Path-style scan patterns now error.** Entries in `exclude.scan.patterns` that contain `/` are
rejected at startup. These patterns could never match (Scan matches basenames). Move path-style
patterns to `exclude.files` for specific paths, or rewrite them as basename globs:

```
# Error: projectconfig: scan pattern "secrets/*.pem" contains a path separator — patterns match basenames only
```

Fix: replace `secrets/*.pem` with `*.pem` (or add `secrets/my.pem` to `exclude.files`).

**Symlinked `.makeslop.yaml` now errors.** If `.makeslop.yaml` is a symlink, both `makeslop init`
and `makeslop run` exit with an error. Replace the symlink with a regular file:

```sh
cp --remove-destination "$(readlink .makeslop.yaml)" .makeslop.yaml
```

---

## Cache layout

```
~/.makeslop/
├── Dockerfile
├── settings.json
└── workspaces/
    └── <basename>-<sha256[:6]>/
```

`settings.json` records each registered workspace keyed by its absolute, symlink-evaluated path.
The per-workspace cache directory under `workspaces/` holds per-project agent state (`.claude/`,
`.codex/`, `docs/`).

---

## Container layout and mount table

`makeslop run` runs with workdir `/workspace/<name>` (where `<name>` is the registered workspace's
cache-dir basename):

| Host                                                  | Container                          | Group              |
| ----------------------------------------------------- | ---------------------------------- | ------------------ |
| `<projectRoot>`                                       | `/workspace/<name>`                | always             |
| `~/.makeslop/.claude/`                                | `/home/user/.claude/`              | global             |
| `~/.makeslop/.claude.json`                            | `/home/user/.claude.json`          | global             |
| `~/.makeslop/.codex/`                                 | `/home/user/.codex/`               | global             |
| `<projectRoot>/.makeslop.yaml`                        | `/workspace/<name>/.makeslop.yaml` | sandbox-policy (ro)|
| tmpfs (empty)                                         | `/workspace/<name>/.git/hooks`     | git-hooks mask     |
| `~/.makeslop/workspaces/<name>/.claude/`              | `/workspace/<name>/.claude/`       | agent-state        |
| `~/.makeslop/workspaces/<name>/.codex/`               | `/workspace/<name>/.codex/`        | agent-state        |
| `~/.makeslop/workspaces/<name>/docs/`                 | `/workspace/<name>/docs/`          | content            |
| `~/.makeslop/workspaces/<name>/CLAUDE.md`             | `/workspace/<name>/CLAUDE.md`      | content            |

The **global** mounts (rows 2–4) are always present. The **sandbox-policy** read-only bind (row 5)
is present only when `.makeslop.yaml` exists at the project root. The **git-hooks mask** tmpfs (row
6) is present only when `.git` is a directory at the project root (not a gitfile). The
**agent-state** and **content** overlay mounts (rows 7–10) can be disabled per-project via the
`cache:` block in `.makeslop.yaml`:

```yaml
cache:
  content: true   # mount docs/ + CLAUDE.md from per-workspace cache (default: true)
  agent: true     # mount .claude/ + .codex/ from per-workspace cache (default: true)
```

Setting a group to `false` omits those overlay mounts so the project's real files show through.
An absent `cache:` block is equivalent to `{content: true, agent: true}` — behavior is identical
to before this feature was added. The `init --global-only` flag is a convenience shortcut that
scaffolds `.makeslop.yaml` with both groups disabled.

---

## Environment variables (`environments:` block in `.makeslop.yaml`)

Declare static environment variables to inject into the app container at runtime using an optional
`environments:` block in the project-local `.makeslop.yaml`:

```yaml
environments:
  NODE_ENV: production
  PORT: 8080
  LOG_LEVEL: debug
  API_BASE_URL: "https://api.example.com"
```

Each key–value pair becomes a `-e KEY=VALUE` flag passed to Docker. Variables appear inside the
container alongside anything set in the base `Dockerfile`.

**Value types:** Values must be YAML scalars. Strings, numbers, and booleans are all accepted and
coerced to their string representation:

```yaml
environments:
  PORT: 8080        # → PORT=8080
  DEBUG: true       # → DEBUG=true
  RETRIES: 3        # → RETRIES=3
```

**Rules and error handling:**

- Non-scalar values (lists, maps) are rejected with a hard error — `makeslop run` will not launch.
- Null values (`KEY:` or `KEY: null`) are rejected. A bare key with no value is almost always a
  mistake; provide an explicit value or remove the key.
- Explicit empty string (`KEY: ""`) is accepted and injects `KEY=` into the container (a valid
  empty environment variable).
- Empty keys are rejected.
- Variables are passed in sorted key order (deterministic output in `--dry-run`).

**Absent block:** When `environments:` is absent from `.makeslop.yaml`, no `-e` flags are emitted —
behavior is byte-identical to before this feature was added (backward-compatible).

**Verification (`--dry-run`):** Use `makeslop run --dry-run` to see the exact `-e` flags before
launching the container.

---

## In-container security flags

Security flags applied inside the container:

- `--tmpfs /tmp:size=<tmp_dir_size>` (default `100m`, configurable via `makeslop config set tmp_dir_size`)
- `--cap-drop ALL`
- `--security-opt no-new-privileges`

Mounts are emitted as `--mount type=bind,source=...,target=...` so paths containing `:` do not
break parsing.

**Sandbox-policy mounts** (applied when the host path exists):

| Mount | Condition | Effect |
|---|---|---|
| `.makeslop.yaml` read-only bind | regular file at project root | agent cannot modify its own scan/exclusion policy |
| `.git/hooks` tmpfs | `.git` is a directory at project root | agent cannot plant hooks that run on the host |

These mounts are layered on top of the read-write project bind. See
[security.md — Sandbox-policy protection](security.md#sandbox-policy-protection) for details and
known residuals.

For secret masking and the home-directory guard, see [security.md](security.md).

---

## Host UID

The container runs as uid 1000. This works transparently on Docker Desktop (macOS) and on Linux
hosts where the running user is uid 1000. Full uid remapping is deferred to post-1.0.

---

## TTY policy

`makeslop run` is interactive-only. When stdin or stdout is not a TTY it exits non-zero with:

```
makeslop: stdin/stdout must be a TTY — run in an interactive terminal
```

`makeslop build`, `makeslop init`, `makeslop migrate`, `makeslop version`, `makeslop config`, and
`makeslop status` do not require a TTY and work correctly in CI pipelines and non-interactive
shells.

---

## Dry run

Pass `--dry-run` (short: `-n`) to print the equivalent shell command for the container launch that
`makeslop` would execute and then exit without launching the container. The output is a multi-line,
backslash-continued, paste-ready shell command on stdout. All pre-launch checks still run
(home-directory guard, settings load, workspace lookup, project config parse, secret scan), so the
printed command equals the real invocation byte-for-byte. Daemon and image pre-flight checks are
skipped on `--dry-run`.

```
makeslop run --dry-run
makeslop run -n
```

Because the TTY check is skipped on dry-run, `--dry-run` succeeds even when stdin/stdout are pipes.
This makes it suitable for CI inspection:

```
makeslop run -n > cmd.sh   # capture only the command; masked-file count goes to stderr
```

---

## Exit codes

- `0` — success (`init` registered/reused a project, or the container exited cleanly, or `build`
  completed successfully, or `status` found all blocking checks passing).
- container's exit code — `makeslop run` propagates `exit N` from the container as the host's
  exit code.
- `1` — `makeslop build` exits 1 on any build failure (the docker SDK returns an error; there is no
  child docker process to propagate an exit code from).
- `1` — `makeslop status` exits 1 when any blocking check (daemon, base config, image, or workspace) fails.
- `1` — any other failure: no workspace registered for pwd, no TTY available, corrupt
  `settings.json`, I/O error, etc. The reason is written to stderr.

---

## Output conventions

- **stdout**: machine result only (paths, values, container output, `--json` output).
- **stderr**: progress, `masked N` notice, nudges, errors.
- Actionable errors follow the form `makeslop: <what failed> — <remedy>`.
- `--quiet` (inherited by all subcommands): silences stderr chrome (notices, nudges, progress)
  while keeping errors. Useful in scripts that parse stdout.

---

## Path resolution

`makeslop` resolves the current working directory through `filepath.EvalSymlinks` before consulting
the cache. As a result `/tmp/foo` and `/private/tmp/foo` (the macOS-style symlinked form) map to
the same workspace, and registering via either alias is idempotent. The key stored in
`settings.json` is always the fully-resolved path. The same applies to symlinked home directories
on Linux hosts.

---

## Docker container settings (settings.json)

The image, shell, and `/tmp` tmpfs size are configurable via `makeslop config set` or by editing
`~/.makeslop/settings.json` directly. Defaults are `claudebox`, `/bin/zsh`, and `100m`:

```json
{
    "version": 1,
    "image": "claudebox",
    "shell": "/bin/zsh",
    "tmp_dir_size": "100m",
    "workspaces": {}
}
```

**Field notes:**
- `version` — single version field (`ConfigVersion`); currently `1`. Written by `makeslop migrate`
  (or stamped by `makeslop init` on a fresh seed) to record which generation `~/.makeslop/` is at.
  Absent or `0` means the directory has not been migrated yet — `makeslop migrate` will bootstrap
  it. Increment `ConfigVersion` whenever `Settings` struct fields change **or** the embedded
  Dockerfile changes.
- Omitted or empty `image`/`shell`/`tmp_dir_size` fields fall back to their defaults; existing
  `settings.json` files predating these keys keep working unchanged.

`tmp_dir_size` accepts a positive integer with an optional suffix: `k`/`K` (kibibytes), `m`/`M`
(mebibytes), `g`/`G` (gibibytes), or no suffix (bytes). Example: `100m`, `2g`, `512k`, `1048576`.
A bare number without a suffix is interpreted by Docker as **bytes** — `512` means 512 bytes, not
512 MB.
