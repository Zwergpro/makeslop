# makeslop — Command & Runtime Reference

Complete reference for all `makeslop` commands, flags, runtime behavior, and configuration.

## Table of Contents

- [Requirements](#requirements)
- [Commands](#commands)
  - [init](#init)
  - [run](#run)
  - [status](#status)
  - [config](#config)
  - [version](#version)
- [Setup flow](#setup-flow)
- [Using a custom Docker image](#using-a-custom-docker-image)
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
(`settings.json`, `.claude.json`, and the global agent directories). Existing files are never
overwritten.

- When no image is configured, a non-blocking note is printed to stderr (suppressed by `--quiet`)
  and `init` still exits 0:
  ```
  note: no image configured — run 'makeslop config set image <ref>'
  ```
- If `pwd` is already a subdirectory of a registered workspace, the existing workspace's cache path
  is returned (idempotent, no mutation).
- Otherwise a new entry is added to `settings.json`, the cache directory is created, and its
  absolute path is printed to stdout.
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
- `--proxy host:port` — route container traffic through a remote HTTP forward proxy
  (see [security.md](security.md#network-egress--two-state-model)); note the `unix://` proxy URL
  scheme used internally is not honored by most HTTP clients — see the Known Limitation note in
  [security.md](security.md#network-egress--two-state-model)

---

### status

Runs an ordered health check and reports the result. CI-safe; does not require a TTY.

Checks (in order); daemon and image checks are bounded by a 10-second preflight timeout:
1. Daemon reachability — **blocking**
2. Base config — **blocking**; `✗` when `settings.json` is absent or corrupt
3. Image — **blocking**; `✗` when no image is configured (`no image configured — run 'makeslop
   config set image <ref>'`), when settings are unreadable, when the daemon is down, or when the
   image is missing locally (`— build or pull it (e.g. 'docker pull X')`). An `-i` value skips the settings steps, so
   the check works even when `settings.json` is absent or corrupt.
4. Workspace registration — **blocking**
5. Secret scan summary — non-blocking
6. Proxy configuration — non-blocking; shows `"direct (bridge networking)"` or the upstream address
7. Socat image presence — non-blocking; `!` with hint when `alpine/socat` is absent

Each check emits one aligned line with a glyph (`✓ ✗ ! –`). A final verdict line names the next
action. Exit code is 0 when all blocking checks pass.

**Flags:**
- `--json` — emit `{"checks":[{"name","state","detail"}...],"ready":bool}`; exit code still
  reflects readiness
- `--image` / `-i <ref>` — check this image instead of the `image` setting

---

### config

Manages persistent settings in `~/.makeslop/settings.json`. Works without a prior `init`.

- `makeslop config` / `makeslop config list` — print all current effective settings as
  `key = value` lines.
- `makeslop config set <key> <value>` — validate and persist a setting.

**Configurable keys:**
- `image` — Docker image reference (no default; required by `run`)
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

## Cache layout

```
~/.makeslop/
├── settings.json
├── .settings.lock
└── workspaces/
    └── <basename>-<sha256[:6]>/
```

`settings.json` records each registered workspace keyed by its absolute, symlink-evaluated path.
The per-workspace cache directory under `workspaces/` holds per-project agent state (`.claude/`,
`.codex/`, `docs/`).

`.settings.lock` is an advisory lock file that serializes concurrent writes to `settings.json`
(used by `init`, `config set`, and `remove`). It is a permanent artifact created on first write
and is safe to ignore in directory listings.

---

## Container layout and mount table

`makeslop run` runs with workdir `/workspace/<name>` (where `<name>` is the registered workspace's
cache-dir basename):

| Host                                                  | Container                          | Group         |
| ----------------------------------------------------- | ---------------------------------- | ------------- |
| `<projectRoot>`                                       | `/workspace/<name>`                | always        |
| `~/.makeslop/.claude/`                                | `/home/user/.claude/`              | global        |
| `~/.makeslop/.claude.json`                            | `/home/user/.claude.json`          | global        |
| `~/.makeslop/.codex/`                                 | `/home/user/.codex/`               | global        |
| `~/.makeslop/workspaces/<name>/.claude/`              | `/workspace/<name>/.claude/`       | agent-state   |
| `~/.makeslop/workspaces/<name>/.codex/`               | `/workspace/<name>/.codex/`        | agent-state   |
| `~/.makeslop/workspaces/<name>/docs/`                 | `/workspace/<name>/docs/`          | content       |
| `~/.makeslop/workspaces/<name>/CLAUDE.md`             | `/workspace/<name>/CLAUDE.md`      | content       |

The **global** mounts (rows 2–4) are always present. The **agent-state** and **content** overlay
mounts (rows 5–8) can be disabled per-project via the `cache:` block in `.makeslop.yaml`:

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

`makeslop init`, `makeslop version`, `makeslop config`, `makeslop status`, `makeslop ls`, and
`makeslop remove` do not require a TTY and work correctly in CI pipelines and non-interactive
shells.

---

## Dry run

Pass `--dry-run` (short: `-n`) to print the equivalent shell command for the container launch that
`makeslop` would execute and then exit without launching the container. The output is a multi-line,
backslash-continued, paste-ready shell command on stdout. All pre-launch checks still run
(home-directory guard, workspace lookup, secret scan, settings load), so the printed command equals the
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

- `0` — success (`init` registered/reused a project, or the container exited cleanly, or `status`
  found all blocking checks passing).
- container's exit code — `makeslop run` propagates `exit N` from the container as the host's
  exit code.
- `1` — `makeslop status` exits 1 when any blocking check (daemon, base config, image, or workspace) fails.
- `1` — any other failure: no image configured, image missing locally, no workspace registered
  for pwd, no TTY available, corrupt
  `settings.json`, invalid `--proxy` address, I/O error, etc. The reason is written to stderr.

---

## Output conventions

- **stdout**: machine result only (paths, values, container output, `--json` output).
- **stderr**: progress, `masked N` notice, nudges, errors.
- Actionable errors follow the form `makeslop: <what failed> — <remedy>`.
- `--quiet` (inherited by all subcommands): silences stderr chrome (notices and hints such as the
  `masked N` count and the `init` notes) while keeping errors. It never changes stdout.

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
