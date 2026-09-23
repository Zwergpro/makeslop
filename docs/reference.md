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
- [Setup flow and breaking changes](#setup-flow-and-breaking-changes)
- [Cache layout](#cache-layout)
- [Container layout and mount table](#container-layout-and-mount-table)
- [Environment variables](#environment-variables-environments-block-in-makeslopyaml)
- [In-container security flags](#in-container-security-flags)
- [Host UID](#host-uid)
- [TTY policy](#tty-policy)
- [Dry run](#dry-run)
- [Exit codes](#exit-codes)
- [Output conventions](#output-conventions)
- [Path resolution](#path-resolution)
- [Docker container settings (settings.json)](#docker-container-settings-settingsjson)
- [Using a custom Docker image](#using-a-custom-docker-image)

---

## Requirements

- A Docker **daemon** must be reachable (via `DOCKER_HOST` or the default Unix socket
  `/var/run/docker.sock`). `makeslop` uses the moby/moby Go SDK directly; the `docker` CLI binary
  is **not** required.
- A container **image** present in the local daemon. makeslop never builds or pulls images; build
  or pull one yourself and point makeslop at it (see
  [Using a custom Docker image](#using-a-custom-docker-image)).

---

## Commands

### init

Registers the current working directory as a workspace and seeds `~/.makeslop/` (the `.claude/`,
`.codex/`, and `workspaces/` directories and an empty `.claude.json`). Seeding is idempotent and
never overwrites existing files.

- If `pwd` is already a subdirectory of a registered workspace, the existing workspace's cache path
  is returned (idempotent, no mutation).
- Otherwise a new entry is added to `settings.json`, the cache directory is created, and its
  absolute path is printed to stdout.
- If no image is configured, a non-blocking note is printed to stderr (suppressed by `--quiet`);
  `init` still succeeds:
  ```
  note: no image configured — run 'makeslop config set image <ref>'
  ```
- Prints to stderr (suppressed by `--quiet`): `registered <name> — run 'makeslop run'`
  (emitted on both fresh registration and re-init of an existing workspace).

**Flags:**
- `--out-of-home` — bypass the home-directory guard (see [security.md](security.md#home-directory-guard))
- `--global-only` — scaffold `.makeslop.yaml` with both per-workspace cache overlay groups disabled
  (only the global `~/.makeslop` mounts remain). This only affects a **fresh** scaffold:
  `Scaffold` is idempotent (EEXIST is success when the existing file is a regular file, never
  clobbers existing user edits), so on an already-init'd project the flag is a no-op — a note is
  not printed in that case, but the existing YAML is left unchanged. If `.makeslop.yaml` is a
  symlink, `init` exits with an error (see
  [security.md — symlinked .makeslop.yaml](security.md#breaking-change-symlinked-makeslopyaml-rejected)).

---

### run

From within a registered workspace, launches an interactive, project-scoped Docker container with
the workspace source tree mounted in. By default, per-workspace + global agent config
(`.claude/`, `.codex/`, `CLAUDE.md`, `docs/`) are also mounted as overlay groups; individual
groups can be disabled via `cache.content` and `cache.agent` in `.makeslop.yaml`. Static values
and host-passthrough variables can be injected via the `environments:` block — see
[Environment variables](#environment-variables-environments-block-in-makeslopyaml).

- Exits with the container's exit code.
- Refuses to launch when stdin or stdout is not a TTY (see [TTY policy](#tty-policy)).
- **Image resolution:** `-i/--image` wins, then the `image` setting. There is no default image; if
  neither is set, `run` exits non-zero before the workspace lookup (this applies to `--dry-run`
  too):
  ```
  makeslop: no image configured — run 'makeslop config set image <ref>' or pass -i/--image
  ```
  The resolved value must be a valid image reference (lowercase name, optional tag/digest, no
  leading `-`); an invalid one fails the same way, before any docker call.
- If no ancestor directory is registered, exits non-zero with a hint to run `makeslop init`.
- Before launching, performs two pre-flight checks:
  1. Daemon reachability (`— is docker running?`)
  2. Image existence in the local daemon. makeslop never builds or pulls; a missing image fails
     with:
     ```
     makeslop: image "<ref>" not found locally — build or pull it (e.g. 'docker pull <ref>')
     ```
- `--dry-run` skips both pre-flight checks and the TTY check (printed == executed invariant).

**Flags:**
- `--dry-run` / `-n` — print the equivalent shell command and exit without launching the container
- `--image` / `-i <ref>` — container image to run for this invocation (overrides the `image`
  setting; not persisted)
- `--out-of-home` — bypass the home-directory guard

---

### status

Runs an ordered health check and reports the result. CI-safe; does not require a TTY.

Checks (in order):
1. Daemon reachability — **blocking**
2. Base config (`settings.json`) presence — absent or corrupt is blocking (`✗`)
3. Image — **blocking**. Resolved like `run` (`-i/--image`, then the `image` setting). Fails
   when no image is configured or the reference is invalid, when settings are unreadable and no `-i` was given, when the
   daemon is down, or when the image is not present locally (same "build or pull it" hint as
   `run`). Passing `-i` lets the check run even when `settings.json` is absent or corrupt.
4. Workspace registration — **blocking**
5. Secret scan summary — non-blocking. An unreadable `.makeslop.yaml` (including a symlinked
   one) is reported here as a warning (`!`); `status` does not fail on it, unlike `run`/`init`.

Each check emits one aligned line with a glyph (`✓ ✗ ! –`). A final verdict line names the next
action. Exit code is 0 when all blocking checks pass.

**Flags:**
- `--json` — emit `{"checks":[{"name","state","detail"}...],"ready":bool}`; exit code still
  reflects readiness
- `--image` / `-i <ref>` — image to check (overrides the `image` setting)

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
- If the name is not registered, exits non-zero with:
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
- `makeslop config set <key> <value>` — validate and persist a setting.

**Configurable keys:**
- `image` — container image reference, e.g. `claudebox` or `my-org/agent:latest` (**no default**;
  must be set here or passed per invocation with `-i/--image`; empty values and invalid references are rejected). Unset
  shows as `image = ` in `config list`.
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

## Setup flow and breaking changes

Normal first-run order:

1. Build or pull an image yourself, e.g. `docker build -t claudebox examples/claudebox` from a
   clone of this repo.
2. `makeslop config set image <ref>`.
3. `makeslop init` in the project directory.
4. `makeslop run`.

Steps 2 and 3 can happen in either order. `init` only notes a missing image; `run` is the command
that requires one. `makeslop status` reports what is still missing.

### Breaking change: `build` and `migrate` removed, no default image

makeslop no longer ships or builds an image. The `build` and `migrate` commands are gone, and the
`image` setting has no default (it used to fall back to `claudebox`). To upgrade:

- Build your image yourself, e.g. from [`examples/claudebox/Dockerfile`](../examples/claudebox/Dockerfile)
  or from your old `~/.makeslop/Dockerfile`, then run `makeslop config set image <ref>`. If you
  used the old default, `docker images` probably still lists `claudebox`, so
  `makeslop config set image claudebox` is enough.
- `~/.makeslop/Dockerfile` is no longer read or written; delete it if you like.
- The `version` key in `settings.json` is ignored and dropped on the next settings write. No
  migration step is needed.

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
├── .claude/            # global agent config (mounted at /home/user/.claude/)
├── .claude.json
├── .codex/
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

Inject environment variables into the app container at runtime with an optional `environments:`
block in the project-local `.makeslop.yaml`. It has two optional sub-keys:

```yaml
environments:
  static:            # fixed KEY: value pairs
    NODE_ENV: production
    PORT: 8080
    API_BASE_URL: "https://api.example.com"
  host:              # names whose values are copied from the host environment at run time
    - GITHUB_TOKEN
    - TERM
```

Each resulting pair becomes a `-e KEY=VALUE` flag passed to Docker. Variables appear inside the
container alongside anything set in the image. All pairs (static and host) are passed in sorted
key order (deterministic output in `--dry-run`).

### `static`

A mapping of `KEY: value`. Values must be YAML scalars; strings, numbers, and booleans are all
accepted and coerced to their string representation:

```yaml
environments:
  static:
    PORT: 8080        # → PORT=8080
    DEBUG: true       # → DEBUG=true
```

- Non-scalar values (lists, maps) are rejected with a hard error — `makeslop run` will not launch.
- Null values (`KEY:` or `KEY: null`) are rejected. A bare key with no value is almost always a
  mistake; provide an explicit value or remove the key.
- Explicit empty string (`KEY: ""`) is accepted and injects `KEY=`.
- Empty keys, keys containing `=`, and keys or values containing newline/tab characters are
  rejected. Duplicate keys are rejected.

### `host`

A list of variable names. At `run` time each name is looked up in the host environment and copied
into the container **under the same name**:

- Set on the host → `NAME=<value>`.
- Set but empty on the host → `NAME=`.
- Unset on the host → skipped silently (no `-e` flag).
- Names must be non-empty and must not contain `=` or whitespace. Duplicates are dropped.
- Host values are passed **verbatim and are not validated** — unlike `static` values, they may
  contain newlines (e.g. a PEM key).
- Renaming (host `A` → container `B`) and default values are not supported.

A name listed in both `static` and `host` is an error:

```
projectconfig: environment key "NAME" listed in both environments.static and environments.host
```

### Errors

Any error in the block aborts `makeslop run` before the container starts (`makeslop status` reports
it as a non-blocking `cannot read .makeslop.yaml` secret-scan warning). Messages name keys only, never values:

- `environments` not a mapping: `projectconfig: environments must be a mapping with optional "static" and "host" keys`
- unknown sub-key with a list or map value (e.g. `hosts: [A]`): `projectconfig: unknown key "hosts" in environments (allowed: static, host)`
- `static` not a mapping: `projectconfig: environments.static must be a mapping of KEY: value`
- `host` not a list (e.g. `host: GITHUB_TOKEN`): `projectconfig: environments.host must be a list of variable names`

YAML merge keys (`<<:`) are not supported inside `environments:`; they are rejected as unknown or
non-scalar keys.

### Migration from the flat form (breaking change)

Earlier versions accepted a flat `environments: {KEY: value}` map. That form is now rejected and
`makeslop run` fails with:

```
projectconfig: environments: flat "KEY: value" form is no longer supported; move entries under environments.static
```

Files are not auto-migrated. Move the entries one level down, under `static:`:

```yaml
# before
environments:
  NODE_ENV: production

# after
environments:
  static:
    NODE_ENV: production
```

**Absent block:** When `environments:` is absent or empty, no `-e` flags are emitted.

**Verification (`--dry-run`):** Use `makeslop run --dry-run` to see the exact `-e` flags before
launching the container. **`--dry-run` prints resolved `host` values in full, secrets included** —
do not paste its output into logs or issues without redacting them. See
[security.md — Host environment passthrough](security.md#host-environment-passthrough).

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

`makeslop init`, `makeslop version`, `makeslop config`, `makeslop status`, `makeslop ls`, and
`makeslop remove` do not require a TTY and work correctly
in CI pipelines and non-interactive shells.

---

## Dry run

Pass `--dry-run` (short: `-n`) to print the equivalent shell command for the container launch that
`makeslop` would execute and then exit without launching the container. The output is a multi-line,
backslash-continued, paste-ready shell command on stdout. All pre-launch checks still run
(home-directory guard, settings load, image resolution, workspace lookup, project config parse,
secret scan), so the
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

- `0` — success (`init` registered/reused a project, or the container exited cleanly, or
  `status` found all blocking checks passing).
- container's exit code — `makeslop run` propagates `exit N` from the container as the host's
  exit code.
- `1` — `makeslop status` exits 1 when any blocking check (daemon, base config, image, or workspace) fails.
- `1` — `makeslop remove` exits 1 when the workspace name is not registered.
- `1` — any other failure: no image configured, image not found locally, no workspace registered
  for pwd, no TTY available, corrupt `settings.json`, I/O error, etc. The reason is written to
  stderr.

---

## Output conventions

- **stdout**: machine result only (paths, values, container output, `--json` output).
- **stderr**: `masked N` notice, notes and hints, errors.
- Actionable errors follow the form `makeslop: <what failed> — <remedy>`.
- `--quiet` (inherited by all subcommands): silences stderr chrome (notices and hints) while
  keeping errors. Useful in scripts that parse stdout.

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
`~/.makeslop/settings.json` directly. `image` has no default; `shell` and `tmp_dir_size` default
to `/bin/zsh` and `100m`:

```json
{
    "image": "claudebox",
    "shell": "/bin/zsh",
    "tmp_dir_size": "100m",
    "workspaces": {}
}
```

**Field notes:**
- `image` — required before `run` (unless `-i/--image` is passed). Omitted or empty means unset;
  it is never filled in with a default.
- Omitted or empty `shell`/`tmp_dir_size` fields fall back to their defaults; existing
  `settings.json` files predating these keys keep working unchanged.
- Obsolete keys from older versions (`version`, `migrated_version`) are ignored and dropped the
  next time makeslop writes the file.

`tmp_dir_size` accepts a positive integer with an optional suffix: `k`/`K` (kibibytes), `m`/`M`
(mebibytes), `g`/`G` (gibibytes), or no suffix (bytes). Example: `100m`, `2g`, `512k`, `1048576`.
A bare number without a suffix is interpreted by Docker as **bytes** — `512` means 512 bytes, not
512 MB.

---

## Using a custom Docker image

makeslop does not build, ship, or pull images. The image that `makeslop run` launches is whatever
`-i/--image` names, or else the `image` setting (see
[Docker container settings](#docker-container-settings-settingsjson)). You build or pull it
yourself.

### Starting from the example image

[`examples/claudebox/Dockerfile`](../examples/claudebox/Dockerfile) is a ready-made image with
Claude Code, Codex, Go, and Node.js. From a clone of this repo:

```sh
docker build -t claudebox examples/claudebox
makeslop config set image claudebox
```

Copy and edit that Dockerfile to add packages, tools, or language runtimes, then rebuild with
`docker build`. makeslop never touches it.

### Any other image

```sh
docker pull my-org/my-agent:latest     # or: docker build -t my-org/my-agent:latest .
makeslop config set image my-org/my-agent:latest
makeslop run
```

To try an image without changing the setting, pass it for one invocation:

```sh
makeslop run -i my-org/my-agent:dev
makeslop status -i my-org/my-agent:dev
```

`makeslop run` performs an **image-existence preflight** (a local image inspect) and launches the
image directly. It never pulls and never builds, so make sure the image is present in the local
daemon before running.

### Image contract

A custom image must satisfy the assumptions makeslop and the bind mounts rely on:

- **User:** runs as **uid 1000** with home `/home/user` (the container is launched as uid 1000; the
  agent-config mounts target `/home/user/.claude`, `/home/user/.codex`, etc.). See
  [Host UID](#host-uid).
- **Workdir:** a writable `/workspace` directory (the project tree is bind-mounted at
  `/workspace/<name>`). See the [mount table](#container-layout-and-mount-table).
- **Shell:** the configured `shell` must exist at its path inside the image. The container is exec'd
  with this shell as its command (default `/bin/zsh`). Either install that shell in your image, or
  point makeslop at one your image already has:
  ```sh
  makeslop config set shell /bin/bash
  ```
- **Agents (optional):** the `claude` / `codex` CLIs are not required by makeslop itself — include
  them only if you want them available in the container. Their per-workspace and global state
  directories are mounted regardless.

The in-container security flags (`--cap-drop ALL`, `--security-opt no-new-privileges`, the `/tmp`
tmpfs) and all bind mounts are applied by makeslop at launch time and are independent of which image
you use. Verify the exact launch with `makeslop run --dry-run`.
