# makeslop — Command & Runtime Reference

Complete reference for all `makeslop` commands, flags, runtime behaviour, and configuration.

## Contents

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

- On a fresh `~/.makeslop/` the directory is stamped at the current `MigrationVersion`
  (never stale after a fresh seed).
- On an existing but stale directory a non-blocking nudge is printed to stderr and `init`
  continues without modifying the existing files:
  ```
  note: base config is v<latest>, yours is v<current> — run 'makeslop migrate'
  ```
- If `pwd` is already a subdirectory of a registered workspace, the existing workspace's cache path
  is returned (idempotent, no mutation).
- Otherwise a new entry is added to `settings.json`, the cache directory is created, and its
  absolute path is printed to stdout.
- Success message on stderr: `registered <name> — run 'makeslop build' then 'makeslop run'`.

**Flags:**
- `--out-of-home` — bypass the home-directory guard (see [security.md](security.md#home-directory-guard))

---

### build

Builds (or rebuilds) the base Docker image from `~/.makeslop/Dockerfile` via the moby SDK.

- **Self-healing:** if `~/.makeslop/` has not been initialised yet, `build` seeds it first (same as
  `init`) before building — so `makeslop build` works on a fresh machine with no prior `init`.
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

---

### run

From within a registered workspace, launches an interactive, project-scoped Docker container with
the workspace source tree and per-workspace + global agent config (`.claude/`, `.codex/`,
`CLAUDE.md`, `docs/`) mounted in.

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
- `--proxy host:port` — route container traffic through a remote HTTP forward proxy
  (see [security.md](security.md#network-egress--two-state-model))

---

### status

Runs an ordered health check and reports the result. CI-safe; does not require a TTY.

Checks (in order):
1. Daemon reachability — **blocking**
2. Base config presence + staleness — absent/corrupt is blocking (`✗`), stale is non-blocking (`!`)
3. Image existence — **blocking**
4. Workspace registration — **blocking**
5. Secret scan summary — non-blocking
6. Proxy configuration — non-blocking; shows `"direct (bridge networking)"` or the upstream address
7. Socat image presence — non-blocking; `!` with hint when `alpine/socat` is absent

Each check emits one aligned line with a glyph (`✓ ✗ ! –`). A final verdict line names the next
action. Exit code is 0 when all blocking checks pass.

**Flags:**
- `--json` — emit `{"checks":[{"name","state","detail"}...],"ready":bool}`; exit code still
  reflects readiness

---

### migrate

Brings `~/.makeslop/` up to date with the current binary.

- Compares the binary's `MigrationVersion` constant against the `migrated_version` stored in
  `settings.json`. When they differ, runs all migration steps (force-overwrites
  `~/.makeslop/Dockerfile` from the embedded asset) and stamps the new version.
- When already up to date, prints `already up to date` and exits immediately.
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

Self-heals via `Save`'s `MkdirAll` (creates `~/.makeslop/` if absent).

---

### version

Prints the version string stamped at build time via
`-ldflags "-X main.version=$(git describe --tags --always --dirty)"`. Prints `dev` when built
without ldflags (e.g. via a plain `go build`).

---

## Setup flow and self-healing

`init` seeds files that are absent and registers the current directory as a workspace. On a fresh
machine `init` writes `~/.makeslop/` at the current `MigrationVersion`, so the directory is never
reported stale.

`migrate` is the explicit upgrade path: it force-refreshes managed files in `~/.makeslop/`
whenever the binary ships a newer `MigrationVersion` than what is recorded in `settings.json`. On
subsequent runs `migrate` prints `already up to date` and exits immediately. Running `migrate`
without a prior `init` is also safe — it creates `~/.makeslop/` if it does not exist.

`build` is safe to run without a prior `init` — it seeds `~/.makeslop/` if absent (self-heal) and
then builds the image. After a `migrate` that refreshes the `Dockerfile`, re-run `build` to pick
up the changes.

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

| Host                                                  | Container                          |
| ----------------------------------------------------- | ---------------------------------- |
| `<projectRoot>`                                       | `/workspace/<name>`                |
| `~/.makeslop/.claude/`                                | `/home/user/.claude/`              |
| `~/.makeslop/.claude.json`                            | `/home/user/.claude.json`          |
| `~/.makeslop/.codex/`                                 | `/home/user/.codex/`               |
| `~/.makeslop/workspaces/<name>/.claude/`              | `/workspace/<name>/.claude/`       |
| `~/.makeslop/workspaces/<name>/.codex/`               | `/workspace/<name>/.codex/`        |
| `~/.makeslop/workspaces/<name>/docs/`                 | `/workspace/<name>/docs/`          |
| `~/.makeslop/workspaces/<name>/CLAUDE.md`             | `/workspace/<name>/CLAUDE.md`      |

---

## In-container security flags

Security flags applied inside the container:

- `--tmpfs /tmp:size=<tmp_dir_size>` (default `100m`, configurable via `makeslop config set tmp_dir_size`)
- `--cap-drop ALL`
- `--security-opt no-new-privileges`

Mounts are emitted as `--mount type=bind,source=...,target=...` so paths containing `:` do not
break parsing.

For secret masking, network egress controls, and the home-directory guard, see
[security.md](security.md).

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
(home-dir guard, workspace lookup, secret scan, settings load), so the printed command equals the
real invocation byte-for-byte. Daemon and image pre-flight checks are skipped on `--dry-run`.

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
- `1` — `makeslop status` exits 1 when any blocking check (daemon, image, workspace) fails.
- `1` — any other failure: no workspace registered for pwd, no TTY available, corrupt
  `settings.json`, invalid `--proxy` address, I/O error, etc. The reason is written to stderr.

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
    "workspaces": {},
    "migrated_version": 2
}
```

**Field notes:**
- `version` — settings schema version (`CurrentVersion`); currently `1`. Increment only when the
  `Settings` struct fields change.
- `migrated_version` — written by `makeslop migrate` (or stamped by `makeslop init` on a fresh
  seed) to record which migration generation `~/.makeslop/` is at. Absent means 0
  (pre-migration). Currently `MigrationVersion = 2`. This field is **distinct** from `version`:
  `version` gates JSON schema compatibility; `migrated_version` gates the one-shot directory
  refresh.
- Omitted or empty `image`/`shell`/`tmp_dir_size` fields fall back to their defaults; existing
  `settings.json` files predating these keys keep working unchanged.

`tmp_dir_size` accepts a positive integer with an optional suffix: `k`/`K` (kibibytes), `m`/`M`
(mebibytes), `g`/`G` (gibibytes), or no suffix (bytes). Example: `100m`, `2g`, `512k`, `1048576`.
A bare number without a suffix is interpreted by Docker as **bytes** — `512` means 512 bytes, not
512 MB.
