# makeslop — Command & Runtime Reference

Complete reference for all `makeslop` commands, flags, runtime behavior, and configuration.

## Table of Contents

- [Requirements](#requirements)
- [Commands](#commands)
  - [init](#init)
  - [run](#run)
  - [status](#status)
  - [ls](#ls)
  - [remove](#remove)
  - [config](#config)
  - [version](#version)
- [Setup flow](#setup-flow)
- [Using a custom Docker image](#using-a-custom-docker-image)
- [Project config (`.makeslop.yaml`)](#project-config-makeslopyaml)
- [Cache layout](#cache-layout)
- [Container layout and mount table](#container-layout-and-mount-table)
- [Environment variables (`environments:` block in `.makeslop.yaml`)](#environment-variables-environments-block-in-makeslopyaml)
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

Registers the current working directory as a workspace, seeds `~/.makeslop/` with initial files
(`.claude.json` and the global `.claude/`, `.codex/`, and `workspaces/` directories, plus
`settings.json` on first registration), and scaffolds a `.makeslop.yaml` at the workspace root.
Existing files are never overwritten.

- When no image is configured, a non-blocking note is printed to stderr (suppressed by `--quiet`)
  and `init` still exits 0:
  ```
  note: no image configured — run 'makeslop config set image <ref>'
  ```
- If `pwd` is already a subdirectory of a registered workspace, the existing workspace's cache path
  is returned (idempotent, no registry mutation).
- Otherwise a new entry is added to `settings.json`, the cache directory is created, and its
  absolute path is printed to stdout.
- A symlinked `.makeslop.yaml` at the workspace root is rejected with an error (see
  [Project config](#project-config-makeslopyaml)).
- Prints to stderr (suppressed by --quiet): `registered <name> — run 'makeslop run'`
  (emitted on both fresh registration and re-init of an existing workspace).

**Flags:**
- `--out-of-home` — bypass the home-directory guard (see [security.md](security.md#home-directory-guard))
- `--global-only` — scaffold `.makeslop.yaml` with both per-workspace cache overlay groups disabled
  (only the global `~/.makeslop` mounts remain). This only affects a **fresh** scaffold:
  `Scaffold` is idempotent (EEXIST = success, never clobbers existing user edits), so on an
  already-init'd project the flag is a no-op — a note is not printed in that case, but the
  existing YAML is left unchanged.

---

### run

From within a registered workspace, launches an interactive, project-scoped Docker container with
the workspace source tree mounted in. By default, per-workspace + global agent config
(`.claude/`, `.codex/`, `CLAUDE.md`, `docs/`) are also mounted as overlay groups; individual
groups can be disabled via `cache.content` and `cache.agent` in `.makeslop.yaml`.

- Exits with the container's exit code.
- Refuses to launch when stdin or stdout is not a TTY (see [TTY policy](#tty-policy)).
- Resolves the image first (see [Using a custom Docker image](#using-a-custom-docker-image)): the
  `-i/--image` flag, else the `image` setting. With neither set it exits 1 before any workspace
  lookup or daemon call (also on `--dry-run`):
  ```
  makeslop: no image configured — run 'makeslop config set image <ref>' or pass -i/--image
  ```
- If no ancestor directory is registered, exits non-zero with a hint to run `makeslop init`.
- Parses `.makeslop.yaml` strictly: unknown keys (including the `network:` block from older
  versions), path-style scan patterns, and a symlinked config file abort the launch before the
  container starts (see [Project config](#project-config-makeslopyaml)).
- Before launching, performs two pre-flight checks (each bounded by a 10-second timeout; a
  black-hole `DOCKER_HOST` is surfaced as an error rather than hanging indefinitely):
  1. Daemon reachability (`— is docker running?`)
  2. Image existence. makeslop never pulls or builds; a missing image fails with
     `image "X" not found locally — build or pull it (e.g. 'docker pull X')`.
- Ctrl-C / SIGTERM cancels the running container session cleanly.
- `--dry-run` skips both pre-flight checks and the TTY check (printed == executed invariant).

**Flags:**
- `--dry-run` / `-n` — print the equivalent shell command and exit without launching the container
- `--image` / `-i <ref>` — container image to run, overriding the `image` setting for this invocation
- `--out-of-home` — bypass the home-directory guard

---

### status

Runs an ordered health check and reports the result. CI-safe; does not require a TTY.

Checks (in order); daemon and image checks are bounded by a 10-second preflight timeout:
1. Daemon reachability — **blocking**
2. Base config — **blocking**; `✗` when `settings.json` is absent, unreadable, or corrupt
3. Image — **blocking**; `✗` when no image is configured (`no image configured — run 'makeslop
   config set image <ref>'`), when settings are unreadable, when the daemon is down, or when the
   image is missing locally (`— build or pull it (e.g. 'docker pull X')`). An `-i` value skips the
   settings steps, so the check works even when `settings.json` is absent or corrupt.
4. Workspace registration — **blocking**
5. Secret scan summary — non-blocking; `✓ will mask N file(s)` when the scan finds matches, `!`
   when `.makeslop.yaml` cannot be loaded or the scan fails, `–` otherwise (including when no
   workspace resolved)

Each check emits one aligned line with a glyph (`✓ ✗ ! –`; `[ok] [fail] [!] [–]` when stderr is not
a terminal or `NO_COLOR` is set). A final verdict line (`ready`, or `not ready — <first failing
check's detail>`) names the next action. The human-readable report goes to stderr. Exit code is 0
when all blocking checks pass.

**Flags:**
- `--json` — emit `{"checks":[{"name","state","detail"}...],"ready":bool}` to stdout; `state` is
  one of `ok`, `fail`, `warn`, `info`. Exit code still reflects readiness
- `--image` / `-i <ref>` — check this image instead of the `image` setting

---

### ls

Lists all registered workspaces in an aligned table. CI-safe; does not require a TTY or a live
Docker daemon.

Output columns: `NAME`, `PATH`, `CREATED` (UTC, format `2006-01-02 15:04 UTC`). Rows are sorted
by workspace name.

When no workspaces are registered, a nudge is printed to stderr and stdout stays empty:

```
no workspaces registered — run 'makeslop init'
```

The nudge is suppressed by `--quiet`; stdout stays empty in all cases (pipe-safe).

**Flags:** inherits `--quiet` (root-level persistent flag).

---

### remove

Unregisters a workspace **by name** and deletes its per-workspace cache directory
(`~/.makeslop/workspaces/<name>/`). Does not require a TTY or a live Docker daemon.

```sh
makeslop remove <name>
makeslop rm <name>       # alias
```

- Takes the workspace name as printed by `makeslop ls` (the `NAME` column).
- Removes the registry entry from `settings.json` under a file lock.
- Deletes the cache directory (`os.RemoveAll`) after the lock releases — idempotent if the
  directory was already deleted manually.
- Prints `removed <name>` to stderr on success (suppressed by `--quiet`).
- If the name is not registered, exits 1 with:
  ```
  no workspace named "<name>" — run 'makeslop ls'
  ```
- **No confirmation prompt** — deletes immediately (CI-safe).
- **Always deletes the cache dir** — there is no opt-out flag.

**Residual:** if the `os.RemoveAll` step fails after the registry entry is already deleted,
re-running `remove <name>` will report "no workspace named" because the entry is gone. The error
message includes the cache-dir path so the user can delete it manually.

**Flags:** inherits `--quiet` (root-level persistent flag).

---

### config

Manages persistent settings in `~/.makeslop/settings.json`. Works without a prior `init`.

- `makeslop config` / `makeslop config list` — print all current effective settings as
  `key = value` lines.
- `makeslop config set <key> <value>` — validate and persist a setting, then print the stored
  value as `key = value` on stdout.

**Configurable keys:**
- `image` — Docker image reference (no default; required by `run`)
- `shell` — shell to exec in the container (default: `/bin/zsh`)
- `tmp_dir_size` — size of the `/tmp` tmpfs (default: `100m`)

Accepted `tmp_dir_size` forms: `100m`, `2g`, `512k`, `1048576` (bare number = bytes). `0` is
rejected (docker would treat it as unlimited). `image` and `shell` reject empty or whitespace-only
values. An unknown key is an error that lists the valid keys.

`config set` creates `~/.makeslop/` if absent. `config list` reads only; it does not write and
does not create the directory.

---

### version

Prints the version string stamped at build time via
`-ldflags "-X main.version=$(git describe --tags --always --dirty)"`. Prints `dev` when built
without ldflags (e.g. via a plain `go build`).

---

## Setup flow

Normal first-run order: build or pull an image yourself → `config set image <ref>` → `init` →
`run`. `init` registers the workspace **and** seeds `~/.makeslop/`; `config set` creates
`~/.makeslop/` if needed, so the first two makeslop steps can run in either order.

---

## Using a custom Docker image

makeslop does not build or pull images. Any image you have locally works, as long as it provides
the configured shell and the agent CLIs you want to run as uid 1000 (see [Host UID](#host-uid)).
[`examples/claudebox/Dockerfile`](../examples/claudebox/Dockerfile) is a starting point. From a clone of this repo:

```
docker build -t claudebox examples/claudebox
makeslop config set image claudebox
```

The image is resolved per invocation of `run` and `status`:

1. the `-i/--image` flag, if non-empty (whitespace is trimmed)
2. the `image` setting in `settings.json`, if non-empty
3. otherwise an error: `no image configured — run 'makeslop config set image <ref>' or pass -i/--image`

If the resolved image is not present locally, `run` fails with
`image "X" not found locally — build or pull it (e.g. 'docker pull X')` and `status` reports the same hint.

---

## Project config (`.makeslop.yaml`)

`makeslop init` scaffolds `.makeslop.yaml` at the workspace root; it is never overwritten or
auto-migrated afterwards. The file holds three optional top-level blocks:

- `exclude:` — secret-scan patterns and skip-dirs plus extra file/dir masks (see
  [security.md — Project-local exclusions](security.md#project-local-exclusions))
- `cache:` — per-workspace overlay groups (see [Container layout and mount table](#container-layout-and-mount-table))
- `environments:` — static environment variables (see
  [Environment variables](#environment-variables-environments-block-in-makeslopyaml))

A missing, empty, or comment-only file is equivalent to no configuration (no scan, both cache
groups enabled, no env vars).

The file is decoded in **strict mode**: any unknown key is a hard error that aborts `makeslop run`
before the container starts. This includes the `network:` block (`network.proxy.address`) from
older makeslop versions, which no longer exists — remove it if present. Other hard errors:

- `.makeslop.yaml` is a symlink (dangling or live) —
  `projectconfig: .makeslop.yaml is a symlink — the project config must be a regular file`.
  Replace it with a regular file (`init` rejects it too).
- A scan pattern contains `/` —
  `projectconfig: scan pattern "secrets/*.pem" contains a path separator — patterns match basenames only`.
- Invalid `exclude.files`/`exclude.dirs` entries (absolute, escaping the root, reserved agent
  paths, or listed in both lists) and invalid `environments:` entries.

`makeslop status` reports the same errors as a non-blocking `!` on the secret-scan check.

---

## Cache layout

```
~/.makeslop/
├── settings.json
├── .settings.lock
├── .claude.json
├── .claude/
├── .codex/
└── workspaces/
    └── <basename>-<sha256[:6]>/
        ├── .claude/
        ├── .codex/
        ├── docs/
        └── CLAUDE.md
```

`settings.json` records each registered workspace keyed by its absolute, symlink-evaluated path.
The per-workspace cache directory under `workspaces/` holds per-project agent state (`.claude/`,
`.codex/`, `docs/`, `CLAUDE.md`). `.claude.json`, `.claude/`, and `.codex/` at the top level are
the global agent state shared by every workspace.

`.settings.lock` is an advisory lock file that serializes concurrent writes to `settings.json`
(used by `init`, `config set`, and `remove`). It is a permanent artifact created on first write
and is safe to ignore in directory listings.

---

## Container layout and mount table

`makeslop run` runs with workdir `/workspace/<name>` (where `<name>` is the registered workspace's
cache-dir basename). Mounts are applied in this order:

| Host                                                  | Container                          | Group               |
| ----------------------------------------------------- | ---------------------------------- | ------------------- |
| `<projectRoot>`                                       | `/workspace/<name>`                | always              |
| `~/.makeslop/.claude/`                                | `/home/user/.claude/`              | global              |
| `~/.makeslop/.claude.json`                            | `/home/user/.claude.json`          | global              |
| `~/.makeslop/.codex/`                                 | `/home/user/.codex/`               | global              |
| `<projectRoot>/.makeslop.yaml`                        | `/workspace/<name>/.makeslop.yaml` | sandbox-policy (ro) |
| tmpfs (empty)                                         | `/workspace/<name>/.git/hooks`     | git-hooks mask      |
| `~/.makeslop/workspaces/<name>/.claude/`              | `/workspace/<name>/.claude/`       | agent-state         |
| `~/.makeslop/workspaces/<name>/.codex/`               | `/workspace/<name>/.codex/`        | agent-state         |
| `~/.makeslop/workspaces/<name>/docs/`                 | `/workspace/<name>/docs/`          | content             |
| `~/.makeslop/workspaces/<name>/CLAUDE.md`             | `/workspace/<name>/CLAUDE.md`      | content             |

Secret masks (`/dev/null` file overlays and tmpfs dir overlays) follow all of the above, so a mask
always wins over the mount it shadows.

The **global** mounts (rows 2–4) are always present. The **sandbox-policy** read-only bind (row 5)
is present only when `.makeslop.yaml` is a regular file at the project root. The **git-hooks mask**
tmpfs (row 6) is present only when `.git` is a directory at the project root (not a gitfile). See
[security.md — Sandbox-policy protection](security.md#sandbox-policy-protection). The
**agent-state** and **content** overlay mounts (rows 7–10) can be disabled per-project via the
`cache:` block in `.makeslop.yaml`:

```yaml
cache:
  content: true   # mount docs/ + CLAUDE.md from per-workspace cache (default: true)
  agent: true     # mount .claude/ + .codex/ from per-workspace cache (default: true)
```

Setting a group to `false` omits those overlay mounts so the project's real files show through.
An absent `cache:` block (or an absent key within it) is equivalent to `true`. The
`init --global-only` flag is a convenience shortcut that scaffolds `.makeslop.yaml` with both
groups disabled.

---

## Environment variables (`environments:` block in `.makeslop.yaml`)

Declare static environment variables to inject into the container at runtime using an optional
`environments:` block in the project-local `.makeslop.yaml`:

```yaml
environments:
  NODE_ENV: production
  PORT: 8080
  LOG_LEVEL: debug
  API_BASE_URL: "https://api.example.com"
```

Each key–value pair becomes a `-e KEY=VALUE` flag passed to Docker. Variables appear inside the
container alongside anything set in the image.

**Value types:** Values must be YAML scalars. Strings, numbers, and booleans are all accepted and
coerced to their string representation:

```yaml
environments:
  PORT: 8080        # → PORT=8080
  DEBUG: true       # → DEBUG=true
  RETRIES: 3        # → RETRIES=3
```

**Rules and error handling** (each violation is a hard error — `makeslop run` will not launch):

- Non-scalar values (lists, maps) are rejected.
- Null values (`KEY:` or `KEY: null`) are rejected. A bare key with no value is almost always a
  mistake; provide an explicit value or remove the key.
- Explicit empty string (`KEY: ""`) is accepted and injects `KEY=` into the container (a valid
  empty environment variable).
- Empty keys, keys containing `=`, and keys or values containing a newline or tab are rejected.
- Variables are passed in sorted `KEY=VALUE` order (deterministic output in `--dry-run`).

**Absent block:** When `environments:` is absent from `.makeslop.yaml`, no `-e` flags are emitted.

**Verification (`--dry-run`):** Use `makeslop run --dry-run` to see the exact `-e` flags before
launching the container.

---

## In-container security flags

Security flags applied inside the container:

- `--tmpfs /tmp:size=<tmp_dir_size>` (default `100m`, configurable via `makeslop config set tmp_dir_size`)
- `--cap-drop ALL`
- `--security-opt no-new-privileges`

Mounts are emitted as `--mount type=bind,source=...,target=...` (with `,readonly` for the
sandbox-policy bind) or `--mount type=tmpfs,target=...`, so paths containing `:` do not break
parsing; fields containing `,` or `"` are CSV-quoted.

**Sandbox-policy mounts** (applied when the host path exists):

| Mount | Condition | Effect |
|---|---|---|
| `.makeslop.yaml` read-only bind | regular file at project root | agent cannot modify its own scan/exclusion policy |
| `.git/hooks` tmpfs | `.git` is a directory at project root | agent cannot plant hooks that run on the host |

These mounts are layered on top of the read-write project bind. See
[security.md — Sandbox-policy protection](security.md#sandbox-policy-protection) for details and
known residuals.

For secret masking, network egress, and the home-directory guard, see [security.md](security.md).

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

`makeslop init`, `makeslop version`, `makeslop config`, `makeslop status`, `makeslop ls`, and
`makeslop remove` do not require a TTY and work correctly in CI pipelines and non-interactive
shells.

---

## Dry run

Pass `--dry-run` (short: `-n`) to print the equivalent shell command for the container launch that
`makeslop` would execute and then exit without launching the container. The output is a multi-line,
backslash-continued, paste-ready shell command on stdout. All pre-launch checks still run
(home-directory guard, settings load, image resolution, workspace lookup, project config parse,
secret scan), so the printed command equals the real invocation byte-for-byte. Daemon and image
pre-flight checks are skipped on `--dry-run`.

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

- `0` — success (`init` registered/reused a project, or the container exited cleanly, or `status`
  found all blocking checks passing).
- container's exit code — `makeslop run` propagates `exit N` from the container as the host's
  exit code (including `137` for a SIGKILLed container).
- `1` — `makeslop status` exits 1 when any blocking check (daemon, base config, image, or workspace) fails.
- `1` — `makeslop remove` exits 1 when the workspace name is not registered.
- `1` — any other failure: no image configured, image missing locally, daemon unreachable, no
  workspace registered for pwd, no TTY available, corrupt `settings.json`, invalid
  `.makeslop.yaml`, I/O error, etc. The reason is written to stderr.

---

## Output conventions

- **stdout**: machine result only (paths, values, container output, `--json` output).
- **stderr**: progress, `masked N` notice, nudges, the human-readable `status` report, errors.
- Actionable errors follow the form `makeslop: <what failed> — <remedy>`.
- `--quiet` (inherited by all subcommands): silences stderr chrome (notices and hints such as the
  `masked N` count, the `init` notes, the `ls` nudge, and `removed <name>`) while keeping errors
  and warnings. It never changes stdout.

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
`~/.makeslop/settings.json` directly. `image` has no default; `shell` and `tmp_dir_size` default to
`/bin/zsh` and `100m`:

```json
{
    "image": "claudebox",
    "shell": "/bin/zsh",
    "tmp_dir_size": "100m",
    "workspaces": {}
}
```

**Field notes:**
- `image` — required by `run`; can be overridden per invocation with `-i/--image`. An omitted or
  empty value means unset (see [Using a custom Docker image](#using-a-custom-docker-image)).
- Omitted or empty `shell`/`tmp_dir_size` fields fall back to their defaults.
- Obsolete keys from older versions (`version`, `migrated_version`) are ignored and dropped on the
  next write.

`tmp_dir_size` accepts a positive integer with an optional suffix: `k`/`K` (kibibytes), `m`/`M`
(mebibytes), `g`/`G` (gibibytes), or no suffix (bytes). Example: `100m`, `2g`, `512k`, `1048576`.
A bare number without a suffix is interpreted by Docker as **bytes** — `512` means 512 bytes, not
512 MB.
