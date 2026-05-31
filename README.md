# makeslop

A sandboxed runner for Claude Code + Codex: isolates your AI agent in a per-project Docker
container with controlled mounts, secret masking, and optional egress filtering.

## Quickstart

```
makeslop init     # register the project and seed ~/.makeslop/ (Dockerfile, settings.json)
makeslop build    # build the claudebox docker image from ~/.makeslop/Dockerfile
makeslop run      # launch the interactive container from your project root
```

`migrate` is an explicit upgrade step, not part of the normal setup flow. `init` always seeds
`~/.makeslop/` at the latest version, so a freshly initialized directory is never stale.

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

## Setup commands

`init` seeds files that are absent and registers the current directory as a workspace. On a fresh
machine `init` writes `~/.makeslop/` at the current `MigrationVersion`, so the directory is never
reported stale. Running `init` on a directory with an existing but older `~/.makeslop/` prints a
non-blocking nudge and continues without modifying the existing files:

```
note: base config is v<current>, yours is v<old> — run 'makeslop migrate'
```

`migrate` is the explicit upgrade path: it force-refreshes managed files in `~/.makeslop/`
whenever the binary ships a newer `MigrationVersion` than what is recorded in `settings.json`.
On subsequent runs `migrate` prints `already up to date` and exits immediately. Running `migrate`
without a prior `init` is also safe — it creates `~/.makeslop/` if it does not exist.

`build` is safe to run without a prior `init` — it seeds `~/.makeslop/` if absent (self-heal) and
then builds the image. After a `migrate` that refreshes the `Dockerfile`, re-run `build` to pick up
the changes.

## Readiness check

`makeslop status` runs an ordered health check and reports the result:

```
makeslop status        # human-readable, one line per check
makeslop status --json # machine-readable JSON
```

Checks (in order): daemon reachability, base config presence/staleness, image existence, workspace
registration, secret scan summary, proxy configuration. Exit code is 0 when all blocking checks
pass. `status` is CI-safe and does not require a TTY.

## Usage

- `makeslop init` — registers the current working directory as a workspace and seeds `~/.makeslop/`
  with initial files (including `Dockerfile`). On a fresh `~/.makeslop/` the directory is stamped
  at the current `MigrationVersion` (never stale). On an existing but stale directory a non-blocking
  nudge is printed; init continues. If pwd is already a subdirectory of a registered workspace, the
  existing workspace's cache path is returned (idempotent, no mutation). Otherwise a new entry is
  added to `settings.json`, the cache directory is created, and its absolute path is printed to
  stdout. Success message on stderr: `registered <name> — run 'makeslop build' then 'makeslop run'`.
  Flags: `--out-of-home`.
- `makeslop run` — from within a registered workspace, launches an interactive, project-scoped docker
  container with the workspace source tree and per-workspace + global agent config (`.claude/`,
  `.codex/`, `CLAUDE.md`, `docs/`) mounted in. Exits with the container's exit code. Refuses to
  launch when stdin or stdout is not a TTY. If no ancestor is registered, exits non-zero with a hint
  to run `makeslop init`. Before launching, performs two pre-flight checks:
  1. Daemon reachability (`— is docker running?`).
  2. Image existence (`— run 'makeslop build'`). No auto-build.
  `--dry-run` skips both pre-flight checks and the TTY check (printed == executed invariant).
  Flags: `--dry-run` / `-n`, `--out-of-home`.
- `makeslop status` — ordered readiness report. Checks: daemon, base config (presence + staleness),
  image, workspace, secret scan summary, proxy. Each check emits one aligned line with a glyph
  (`✓ ✗ ! –`); a final verdict line names the next action. Blocking checks (daemon, image,
  workspace) set exit code 1 when they fail. Non-blocking checks (stale config, scan summary,
  proxy) emit a warning or info glyph but do not affect readiness. CI-safe; no TTY required.
  Flags: `--json` (emit `{"checks":[{"name","state","detail"}...],"ready":bool}`; exit code still
  reflects readiness).
- `makeslop migrate` — brings `~/.makeslop/` up to date with the current binary. Compares the
  binary's `MigrationVersion` constant against the `migrated_version` stored in `settings.json`.
  When they differ, runs all migration steps (force-overwrites `~/.makeslop/Dockerfile` from the
  embedded asset) and stamps the new version. When already up to date, exits immediately. Does not
  require a prior `init` and is exempt from the home-directory guard.
- `makeslop build` — builds (or rebuilds) the base docker image from `~/.makeslop/Dockerfile` via
  the moby SDK. Self-healing: if `~/.makeslop/` has not been initialised yet, `build` seeds it
  first (same as `init`) before building — so `makeslop build` works on a fresh machine with no
  prior `init`. The image tag defaults to `claudebox` (configurable via `settings.json`). Flags:
  - `--no-cache` — bypasses the layer cache (passes `NoCache: true` to the SDK build call).
  - `--build-arg K=V` — repeatable; each value is forwarded to the daemon as a build argument
    (use for proxy settings, version pins, etc.).
  - **Empty context + BuildKit**: `build` passes an empty temporary directory as the docker build
    context (the Dockerfile downloads everything; no local files need shipping) and uses the
    BuildKit API (`Version: "2"` via the moby SDK) so cache mounts (`--mount=type=cache`) work
    correctly. No `DOCKER_BUILDKIT` environment variable is needed.
- `makeslop config` — bare invocation prints all current effective settings as `key = value` lines
  (same as `makeslop config list`). Works without a prior `init`.
- `makeslop config list` — print all current effective settings as `key = value` lines. Works
  without a prior `init`.
- `makeslop config set <key> <value>` — validate and persist a setting. Keys: `image` (docker image
  tag), `shell` (shell to exec in the container), `tmp_dir_size` (size of the `/tmp` tmpfs).
  Accepted `tmp_dir_size` forms: `100m`, `2g`, `512k`, `1048576` (bare number = bytes). Works
  without a prior `init` (self-heals via `Save`'s `MkdirAll`).
- `makeslop version` — print the version string (stamped at build time via
  `-ldflags "-X main.version=$(git describe --tags --always --dirty)"`; prints `dev` when built
  without ldflags, e.g. via a plain `go build`).

### Requirements

- A Docker **daemon** must be reachable (via `DOCKER_HOST` or the default Unix socket
  `/var/run/docker.sock`). `makeslop` uses the moby/moby Go SDK directly; the `docker` CLI binary
  is **not** required.

### Container layout

`makeslop run` runs (workdir `/workspace/<name>`, where `<name>` is the registered workspace's
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

Security flags inside the container: `--tmpfs /tmp:size=<tmp_dir_size>` (default `100m`,
configurable via `makeslop config set tmp_dir_size`), `--cap-drop ALL`,
`--security-opt no-new-privileges`. Mounts are emitted as
`--mount type=bind,source=...,target=...` so paths containing `:` do not break parsing.

### Host UID

The container runs as uid 1000. This works transparently on Docker Desktop (macOS) and on Linux
hosts where the running user is uid 1000. Full uid remapping is deferred to post-1.0.

### TTY policy

`makeslop run` is interactive-only. When stdin or stdout is not a TTY it exits non-zero with:

```
makeslop: stdin/stdout must be a TTY — run in an interactive terminal
```

`makeslop build`, `makeslop init`, `makeslop migrate`, `makeslop version`, `makeslop config`, and
`makeslop status` do not require a TTY and work correctly in CI pipelines and non-interactive
shells.

### Secret masking

Before launching the container, `makeslop` scans for secret files under the project root using a
native Go `filepath.WalkDir` walk driven entirely by the project's `.makeslop.yaml`. Each matched
file is overlaid with `/dev/null` inside the container — the agent sees a zero-byte file at that
path instead of the real credential.

Secret masking is **opt-in and config-driven**: if `exclude.scan` is absent (or `patterns` is
empty) in `.makeslop.yaml`, no scan is performed and nothing is masked. `makeslop init` seeds the
default patterns and skip-dirs as active values in the generated `.makeslop.yaml`, so new projects
are safe by default.

The default `exclude.scan` block covers the common secret-file shapes:

```yaml
exclude:
  scan:
    patterns:
      - "*.env"
      - ".env.*"
      - "*.pem"
      - "*.key"
      - "id_rsa*"
      - "id_ed25519*"
      - ".npmrc"
      - ".netrc"
      - ".git-credentials"
    skip-dirs:
      - .git
      - node_modules
      - vendor
      - .venv
```

Patterns are basename globs (`filepath.Match`). Files matching a pattern are masked; symlinks are
silently dropped (not followed). Directories named in `skip-dirs` are pruned entirely during the
walk.

Walk errors (e.g. unreadable subdirectories) are propagated immediately and abort the launch. This
matches the no-secret-leak invariant: if a directory cannot be read, we cannot prove it is
secret-free.

`.gitignore` is intentionally ignored because most `.env` files are gitignored — that is precisely
why the scan is necessary.

When at least one file is masked, `makeslop` prints `makeslop: masked N secret file(s)` to stderr.
Zero hits are silent.

**Pre-existing projects:** if your `.makeslop.yaml` was generated before this change, it will not
contain an `exclude.scan` block — secret masking will not run. Copy the `exclude.scan` block above
into your existing `.makeslop.yaml` to restore masking.

### Project-local exclusions

`makeslop init` creates a `.makeslop.yaml` file at the project root. The generated file includes
the default `exclude.scan` block (patterns + skip-dirs for the secret scan) and empty `files`/`dirs`
lists:

```yaml
exclude:
  scan:
    patterns:
      - "*.env"
      - ".env.*"
      - "*.pem"
      - "*.key"
      - "id_rsa*"
      - "id_ed25519*"
      - ".npmrc"
      - ".netrc"
      - ".git-credentials"
    skip-dirs:
      - .git
      - node_modules
      - vendor
      - .venv
  files: []
  dirs: []
network:
  proxy:
    address: ""
```

Edit this file to control scanning and hide additional directories and files from the container on
every `makeslop run` invocation:

- Entries under `exclude.scan.patterns` are basename globs; files whose name matches are masked
  with `/dev/null`. Remove all patterns to disable secret masking entirely.
- Entries under `exclude.scan.skip-dirs` are bare directory names pruned during the walk.
- Entries under `exclude.dirs` are mounted as an empty in-memory tmpfs, so the container sees an
  empty directory at that path instead of the real contents.
- Entries under `exclude.files` are overlaid with `/dev/null`, so the container sees a zero-byte
  file at that path.

All paths under `exclude.dirs` and `exclude.files` must be relative to the project root. Example:

```yaml
exclude:
  scan:
    patterns:
      - "*.env"
      - ".env.*"
      - "*.pem"
      - "*.key"
      - "id_rsa*"
      - "id_ed25519*"
      - ".npmrc"
      - ".netrc"
      - ".git-credentials"
    skip-dirs:
      - ".git"
      - "node_modules"
      - "vendor"
      - ".venv"
  dirs:
    - node_modules        # large build artifact — skip it entirely
    - secrets             # local secrets directory
  files:
    - secrets/local.env   # specific file overlay
```

The scan results and the `exclude.files` entries are merged; if the same path is found by the scan
and listed in `exclude.files`, only one overlay mount is emitted. A YAML parse error aborts the
launch before docker is invoked.

**Reserved paths.** The paths `.claude`, `.codex`, `docs`, and `CLAUDE.md` are already mounted by
`makeslop run` for agent state. Listing them in `.makeslop.yaml` is rejected with an error
(`projectconfig: path %q collides with a reserved agent path`).

### Controlled network access via a remote proxy

By default `makeslop run` uses Docker's default bridge networking, giving the container unrestricted
outbound access. You can restrict outbound access to a controlled egress proxy by adding a
`network:` section to `.makeslop.yaml`:

```yaml
network:
  proxy:
    address: 10.0.0.5:8888   # host:port of your upstream HTTP forward proxy
```

When `network.proxy.address` is set, `makeslop run`:

1. Starts a host-side forward proxy listening on a per-invocation unix socket under `/tmp`.
   The upstream address is **probe-dialed at launch time** — if it is not reachable, `makeslop`
   aborts with an error before starting docker. This prevents silent black-holing of requests.
2. Runs the container with `--network none` (no direct internet access), bind-mounts the socket
   read-only into the container at `/tmp/makeslop-proxy.sock`, and sets `HTTP_PROXY` and
   `HTTPS_PROXY` to `unix:///tmp/makeslop-proxy.sock`.
3. Tears down the proxy (closes listener, unlinks socket) when the container exits.

The upstream address (`10.0.0.5:8888`) is a plain HTTP forward proxy such as
[tinyproxy](https://tinyproxy.github.io/). The container app speaks standard forward-proxy
protocol (HTTP `CONNECT` for HTTPS, absolute-URL requests for plain HTTP); the unix socket is
fully transparent.

**Scope: Node/undici clients only.** Setting `HTTP_PROXY=unix://...` is honored by Node.js
`undici` (which Claude Code and Codex both use). Other HTTP clients (Python `requests`, Go
`net/http`, curl without explicit `--proxy`) do not support `unix://` proxy URLs. With
`--network none`, any client that ignores the env var gets no network access at all — there is no
fallback to direct internet. Enable this feature only when the container's HTTP clients are
Node/undici-based.

Use `--dry-run` to preview the resulting container launch command (printed as an equivalent
`docker run` invocation), including all exclusion mounts, before launching:

```
makeslop run --dry-run
```

### Docker container settings

The image, shell, and `/tmp` tmpfs size are configurable via `makeslop config set` or by editing
`~/.makeslop/settings.json` directly. Defaults are `claudebox`, `/bin/zsh`, and `100m`:

```json
{
    "version": 1,
    "image": "claudebox",
    "shell": "/bin/zsh",
    "tmp_dir_size": "100m",
    "workspaces": { },
    "migrated_version": 2
}
```

Omitted or empty fields fall back to their defaults; existing `settings.json` files predating
these keys keep working unchanged. `migrated_version` is written by `makeslop migrate` (or stamped
by `makeslop init` on a fresh seed) to record which migration generation the directory is at;
absent means 0 (pre-migration).

`tmp_dir_size` accepts a positive integer with an optional suffix: `k`/`K` (kibibytes), `m`/`M`
(mebibytes), `g`/`G` (gibibytes), or no suffix (bytes). Example: `100m`, `2g`, `512k`, `1048576`.
A bare number without a suffix is interpreted by docker as **bytes** — `512` means 512 bytes, not
512 MB.

### Dry run

Pass `--dry-run` (short: `-n`) to print the equivalent shell command for the container launch that
makeslop would execute and then exit without launching the container. The output is a multi-line, backslash-continued,
paste-ready shell command on stdout. All pre-launch checks still run (home-dir guard, workspace
lookup, secret scan, settings load), so the printed command equals the real invocation byte-for-byte.
Daemon and image pre-flight checks are skipped on `--dry-run`.

```
makeslop run --dry-run
makeslop run -n
```

Because the TTY check is skipped on dry-run, `--dry-run` succeeds even when stdin/stdout are pipes.
This makes it suitable for CI inspection:

```
makeslop run -n > cmd.sh   # capture only the command; masked-file count goes to stderr
```

### Exit codes

- `0` — success (`init` registered/reused a project, or the container exited cleanly, or `build`
  completed successfully, or `status` found all blocking checks passing).
- container's exit code — `makeslop run` propagates `exit N` from the container as the host's
  exit code.
- `1` — `makeslop build` exits 1 on any build failure (the docker SDK returns an error; there is no
  child docker process to propagate an exit code from).
- `1` — `makeslop status` exits 1 when any blocking check (daemon, image, workspace) fails.
- `1` — any other failure: no workspace registered for pwd, no TTY available, corrupt
  `settings.json`, upstream proxy unreachable, I/O error, etc. The reason is written to stderr.

### Home-directory guard

By default, `makeslop run` and `makeslop init` refuse to run from any directory outside the user's
home directory. This prevents accidentally registering sensitive system paths (e.g. `/`, `/etc`)
as workspaces and mounting them into a container. On violation the tool prints:

```
makeslop: refusing to run from <pwd> (outside <home>) — pass --out-of-home to override
```

Pass `--out-of-home` to bypass this check. The flag is scoped to `init` and `run` only:

```
makeslop init --out-of-home
makeslop run --out-of-home
```

`makeslop build`, `makeslop migrate`, `makeslop config`, `makeslop version`, and `makeslop status`
are **exempt** from the home-directory guard — they operate on `~/.makeslop/` directly and do not
consult the current working directory. `--out-of-home` is not a valid flag on these commands.

### Output conventions

- **stdout**: machine result only (paths, values, container output, `--json` output).
- **stderr**: progress, `masked N` notice, nudges, errors.
- Actionable errors follow the form `makeslop: <what failed> — <remedy>`.
- `--quiet` (inherited by all subcommands): silences stderr chrome (notices, nudges, progress)
  while keeping errors. Useful in scripts that parse stdout.

### Path resolution

`makeslop` resolves the current working directory through `filepath.EvalSymlinks` before
consulting the cache. As a result `/tmp/foo` and `/private/tmp/foo` (the macOS-style symlinked
form) map to the same workspace, and registering via either alias is idempotent. The key stored in
`settings.json` is always the fully-resolved path. The same applies to symlinked home directories
on Linux hosts.

## Build

```
go build ./cmd/makeslop
go test ./...
```

Tests no longer use shell shims, so there is no `noexec`/`GOTMPDIR` constraint. The `GOTMPDIR`
prefix (`GOTMPDIR=/home/user go test ./...`) remains harmless if you have it in muscle memory, but
is no longer required.
