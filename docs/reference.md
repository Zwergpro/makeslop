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
- [Cache layout](#cache-layout)
- [Container layout and mount table](#container-layout-and-mount-table)
- [Joined projects (`--join`)](#joined-projects---join)
- [Environment variables](#environment-variables-environments-block-in-makeslopyaml)
- [Container networking](#container-networking-network_mode--networks-in-makeslopyaml)
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
  [security.md — symlinked .makeslop.yaml](security.md#symlinked-makeslopyaml-rejected)).

---

### run

From within a registered workspace, launches an interactive, project-scoped Docker container with
the workspace source tree mounted in. By default, per-workspace + global agent config
(`.claude/`, `.codex/`, `CLAUDE.md`, `docs/`) are also mounted as overlay groups; individual
groups can be disabled via `cache.content` and `cache.agent` in `.makeslop.yaml`. Static values
and host-passthrough variables can be injected via the `environments:` block — see
[Environment variables](#environment-variables-environments-block-in-makeslopyaml). The
container's network is set by `network_mode` / `networks` — see
[Container networking](#container-networking-network_mode--networks-in-makeslopyaml).

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
- Before launching, performs these pre-flight checks:
  1. Daemon reachability (`— is docker running?`)
  2. Image existence in the local daemon. makeslop never builds or pulls; a missing image fails
     with:
     ```
     makeslop: image "<ref>" not found locally — build or pull it (e.g. 'docker pull <ref>')
     ```
  3. Network targets from `.makeslop.yaml`, when set: the `container:` target must be running and
     each named network must exist (see
     [Container networking](#container-networking-network_mode--networks-in-makeslopyaml)).
- `--dry-run` skips the pre-flight checks and the TTY check (printed == executed invariant).

**Flags:**
- `--dry-run` / `-n` — print the equivalent shell command and exit without launching the container
- `--image` / `-i <ref>` — container image to run for this invocation (overrides the `image`
  setting; not persisted)
- `--out-of-home` — bypass the home-directory guard (for the current project and every `--join`)
- `--join` / `-j <path>[:ro|:rw]` — also mount another makeslop project at
  `/workspace/<basename>`; repeatable. See [Joined projects](#joined-projects---join)

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
   one) is reported here as a warning (`!`).
6. Network — **blocking**. `–` when the workspace is unresolved or neither `network_mode` nor
   `networks` is set; `✓` without a daemon call for the built-in modes (`bridge`, `host`, `none`,
   `default`). Otherwise the `container:` target or named networks are checked like `run` does,
   and a failure shows the same hint; when such a target needs checking and the daemon is down,
   the row is `✗ cannot check — daemon unreachable`. A `.makeslop.yaml` that fails to load (any
   load error, including a symlink; the cause is in the secret-scan row) gives
   `✗ cannot check — .makeslop.yaml not loaded (see secret scan)`, so `status` is not ready on a
   config that `run` would reject.

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

## Setup flow

Normal first-run order:

1. Build or pull an image yourself, e.g. `docker build -t claudebox examples/claudebox` from a
   clone of this repo.
2. `makeslop config set image <ref>`.
3. `makeslop init` in the project directory.
4. `makeslop run`.

Steps 2 and 3 can happen in either order. `init` only notes a missing image; `run` is the command
that requires one. `makeslop status` reports what is still missing.

### `.makeslop.yaml` validation

Invalid `.makeslop.yaml` configurations are hard errors, so masking is never silently lost. Full
details are in [security.md](security.md#project-local-exclusions).

**Path-style scan patterns error.** Entries in `exclude.scan.patterns` that contain `/` are
rejected at startup, because Scan matches basenames only. Move path-style
patterns to `exclude.files` for specific paths, or rewrite them as basename globs:

```
# Error: projectconfig: scan pattern "secrets/*.pem" contains a path separator — patterns match basenames only
```

Fix: replace `secrets/*.pem` with `*.pem` (or add `secrets/my.pem` to `exclude.files`).

**Symlinked `.makeslop.yaml` errors.** If `.makeslop.yaml` is a symlink, both `makeslop init`
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
An absent `cache:` block is equivalent to `{content: true, agent: true}`. The `init --global-only` flag is a convenience shortcut that
scaffolds `.makeslop.yaml` with both groups disabled.

Each join adds a project-root bind at `/workspace/<basename>`, then its policy mounts and
masks. Writable joins protect `.makeslop.yaml` with a read-only bind and mask `.git/hooks` when
`.git` is a directory. Read-only joins need neither policy mount. Joins receive no global or
cache overlays. See [Joined projects](#joined-projects---join).

---

## Joined projects (`--join`)

`makeslop run -j <path>[:ro|:rw]` mounts another makeslop project at
`/workspace/<basename>`, next to the current project. The flag is repeatable; joins appear in
flag order. The current project remains the workdir. The default mode is writable.

```sh
makeslop run -j ../lib:ro -j ../util
```

The suffix is recognized only when the final `:` segment is `ro` or `rw`. Thus `foo:bar` is a
path, and `foo:ro:rw` names a directory literally called `foo:ro`. Relative paths use the
physical current directory, even when the shell's `$PWD` contains a symlink. The resolved path
sets the mount name. The shell expands `~` in `-j ~/lib`, but generally not in `--join=~/lib`;
makeslop itself does not expand it.

Each join must be a directory with a regular, non-symlinked `.makeslop.yaml`. Only its `exclude:`
settings apply, and only within that join. Its `cache:`, `environments:`, `network_mode`, and
`networks` settings are ignored; the current project's config controls those settings. Nonempty
environment or network settings produce one notice unless `--quiet` is set. Cache produces no
notice. Config and symlink warnings remain visible under `--quiet` and name the join.

Roots cannot overlap the current project, the makeslop data directory, or another join. Mount
basenames must also be unique and differ from the current workspace name. Path and inode checks
catch case aliases and aliases of a root or its ancestors; a bind mount sourced from a
subdirectory can escape this check. See [Joined projects in the security guide](security.md#joined-projects).

The [home guard](security.md#home-directory-guard) applies to every join; one `--out-of-home`
covers all projects.

Join paths are validated before daemon preflight. Configs are parsed and scanned afterward, in
flag order, so a down daemon takes precedence over a malformed config. A config removed between
validation and loading fails the run. `--dry-run` performs the same path, config, and scan checks
but skips daemon, image, and network preflight. `status` does not accept `--join`.

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
container alongside anything set in the image. All pairs (static and host) are passed sorted by
key (deterministic output in `--dry-run`).

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
- Empty keys, keys containing `=`, and keys or values containing newline, carriage-return, or tab
  characters are rejected. Duplicate keys are rejected.

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
it as a non-blocking `cannot read .makeslop.yaml` secret-scan warning). Messages name keys or
line numbers only, never values. The full list:

- `environments` not a mapping: `projectconfig: environments must be a mapping with optional "static" and "host" keys`
- `KEY: value` directly under `environments:` instead of under `static`: `projectconfig: environments: variables must be listed under environments.static, not directly under environments`
- unknown key with a list or map value, or a misspelled `static`/`host` (other case or a trailing
  `s`, e.g. `hosts:` or `Host:`): `projectconfig: unknown key "hosts" in environments (allowed: static, host)`.
  All-uppercase keys such as `HOST:` count as `KEY: value` entries (the error above).
- duplicate `static` or `host`: `projectconfig: duplicate key "static" in environments`
- null or non-scalar key: `projectconfig: environments: key at line N must be a non-null scalar`
  (inside `static`: `projectconfig: environments.static: key at line N must be a non-null scalar`)
- merge key: `projectconfig: environments: merge keys (<<) are not supported` (inside `static`:
  `projectconfig: environments.static: merge keys (<<) are not supported`)
- `static` not a mapping: `projectconfig: environments.static must be a mapping of KEY: value`
- `host` not a list: `projectconfig: environments.host must be a list of variable names`
- when `static` or `host` holds a single scalar (e.g. `host: GITHUB_TOKEN`, or a variable
  literally named `host` placed directly under `environments:`), the two messages above end with
  ` (variables belong under environments.static)`
- `static` keys: `projectconfig: empty key in environments.static`,
  `projectconfig: environments.static: key at line N must not contain '='`,
  `projectconfig: environments.static: key at line N must not contain newline, carriage-return, or tab characters`,
  `projectconfig: duplicate key "KEY" in environments.static`
- `static` values: `projectconfig: environments.static: key "KEY" must be a scalar value`,
  `projectconfig: environments.static: key "KEY" has no value`,
  `projectconfig: environments.static: key "KEY" value must not contain newline, carriage-return, or tab characters`
- `host` entries: `projectconfig: environments.host entry at line N must be a variable name` (list
  or map entry), `projectconfig: environments.host entry at line N has no name` (null or `""`),
  `projectconfig: environments.host entry at line N must not contain '='`,
  `projectconfig: environments.host entry at line N must not contain whitespace`
- name in both lists: `projectconfig: environment key "NAME" listed in both environments.static and environments.host`

YAML aliases (`*name`) work inside `environments:`: anchor a `static` value and reuse it in another
`static` value or a `host` entry:

```yaml
environments:
  static:
    TOKEN_VAR: &tok GITHUB_TOKEN
  host:
    - *tok
```

The file is decoded strictly, so a top-level key added only to hold an anchor (e.g. `base: &e ...`
followed by `environments: *e`) is rejected as an unknown field. YAML merge keys (`<<:`) are not
expanded; they are rejected with the merge key error above.

**Absent block:** When `environments:` is absent or empty, no `-e` flags are emitted.

**Verification (`--dry-run`):** Use `makeslop run --dry-run` to see the exact `-e` flags before
launching the container. **`--dry-run` prints resolved `host` values in full, secrets included** —
do not paste its output into logs or issues without redacting them. See
[security.md — Host environment passthrough](security.md#host-environment-passthrough).

---

## Container networking (`network_mode` / `networks` in `.makeslop.yaml`)

Two optional top-level keys choose the app container's network. They use compose's names and
meaning:

```yaml
network_mode: "container:proxy"   # bridge | host | none | container:<name|id> | <network>
# or, instead:
networks: [myapp_default, egress_internal]
```

- **Unset** (default): no `--network` flag, Docker's default bridge network. `network_mode: ""`
  and an empty or null `networks` list also count as unset.
- **`network_mode`** is passed verbatim as `--network <mode>` (SDK: `HostConfig.NetworkMode`).
  `container:<name|id>` shares another container's network namespace, so all traffic goes
  through it. makeslop keeps no list of modes; any name-shaped value is passed to the daemon.
- **`networks`** attaches the container to each listed network at create time, in file order, one
  `--network <name>` per entry. The first entry is the primary network. Attaching to more than
  one network at create time needs Docker Engine 25+ (API 1.44).
- **Not both.** Setting both keys is an error.

makeslop only attaches. It never creates or removes networks or containers. `--dry-run` prints
the `--network` flags.

### Validation errors

Names must match `^[a-zA-Z0-9][a-zA-Z0-9_.-]*$` (a permissive check; container IDs match too, and
a name Docker itself would reject is reported as not found by the pre-flight). Errors quote the
offending name, or give a line number, and abort `run` before the container is created (printed
with a `makeslop: ` prefix):

```
projectconfig: set either network_mode or networks, not both
projectconfig: network_mode "container:" has no container name
projectconfig: network_mode "container:a b": invalid container name "a b"
projectconfig: invalid network_mode "a b"
projectconfig: empty entry in networks at line 3
projectconfig: networks entry "host" is a network_mode, not a network
projectconfig: invalid network name "a b" in networks
projectconfig: duplicate network "a" in networks
projectconfig: networks must be a list of names; per-network options are not supported
projectconfig: networks must be a list of names
projectconfig: networks entry at line 2 must be a network name
```

The built-in modes (`bridge`, `host`, `none`, `default`) and any `container:` value are rejected
inside `networks` (the docker CLI refuses to mix built-in modes with user-defined networks); use
`network_mode` for them. Compose's
mapping form (`networks: {a: {}}`) is not supported.

### Pre-flight checks

`run` (not `--dry-run`) and `status` ask the daemon about the targets, bounded by the 10s
pre-flight timeout:

- `bridge`, `host`, `none`, `default`, or unset: nothing to check.
- `container:<x>`: the container must exist and be running. A paused or restarting
  (crash-looping) container counts as not running.
- any other `network_mode`, and every `networks` entry: the network must exist.

Failures (compose prefixes names, so the hints say how to find the real one):

```
makeslop: network_mode: container "proxy" not found — start it first; compose names containers <project>-<service>-1 unless container_name is set (check 'docker ps')
makeslop: network_mode: container "proxy" is not running (stopped, paused or restarting) — start or unpause it (check 'docker ps -a')
makeslop: networks: network "X" not found — create it with 'docker network create X'; compose prefixes networks with <project>_ (check 'docker network ls')
```

Each message starts with the key that named the target: a custom `network_mode: mynet` reports
`network_mode: network "mynet" not found …`.

Any other inspect error (permission denied, ambiguous name, timeout) is shown as-is:

```
makeslop: network_mode: check container "proxy": <error>
makeslop: networks: check network "X": <error>
```

Security implications (a cloned repository's config can pick `host` or join any container) are in
[security.md — Network egress](security.md#network-egress).

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
known residuals. Without a `.makeslop.yaml` there is no read-only bind, so the agent can create
one (including `environments.host` entries) that takes effect on the next run; keep one in every
project (`makeslop init` creates it).

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

Pass `--dry-run` (short: `-n`) to inspect the container launch on stdout. It runs the
home, workspace, config, and secret-scan checks, including those for joins, but skips daemon,
image, and network preflight.

Without joins, output is a paste-ready, backslash-continued `docker run` command. With joins,
blank lines and `# --- project: … ---` / `# --- join: … ---` comments separate mount groups.
Those comments break shell continuation, so joined output is for inspection only. Control
characters in labels are replaced with `?`.

```sh
makeslop run -n > cmd.sh   # masked-file counts go to stderr
```

The output includes resolved `environments.host` values in full, secrets included. Do not keep it
as a CI artifact or log it without redacting them. See
[security.md — Host environment passthrough](security.md#host-environment-passthrough).

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
- Omitted or empty `shell`/`tmp_dir_size` fields fall back to their defaults.
- Unknown keys are ignored and dropped the next time makeslop writes the file.

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
