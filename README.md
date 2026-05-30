# makeslop

`makeslop` is a CLI that maps the current working directory to a per-workspace cache directory under `~/.makeslop/` for use with docker-based workflows.

## Cache layout

```
~/.makeslop/
├── Dockerfile
├── settings.json
└── workspaces/
    └── <basename>-<sha256[:6]>/
```

`settings.json` records each registered workspace keyed by its absolute, symlink-evaluated path. The per-workspace cache directory under `workspaces/` is where future milestones will store docker artifacts, logs, and other build outputs.

## First-run flow

On a fresh machine or after a binary upgrade, run both setup commands in order:

```
makeslop init     # seeds ~/.makeslop/ (Dockerfile, settings.json, etc.) — safe to re-run
makeslop migrate  # applies any pending migrations (e.g. refreshes the bundled Dockerfile)
```

`init` seeds files that are absent; it never overwrites. `migrate` force-refreshes managed files
whenever the binary ships a newer `MigrationVersion` than what is recorded in `settings.json`. On
subsequent runs `migrate` prints `already up to date` and exits immediately. Running `migrate` without
a prior `init` is also safe — it creates `~/.makeslop/` if it does not exist.

## Usage

- `makeslop init` — registers the current working directory as a workspace and seeds `~/.makeslop/` with initial files (including `Dockerfile`). If pwd is already a subdirectory of a registered workspace, the existing workspace's cache path is returned (idempotent, no mutation). Otherwise a new entry is added to `settings.json`, the cache directory is created, and its absolute path is printed.
- `makeslop migrate` — brings `~/.makeslop/` up to date with the current binary. Compares the binary's `MigrationVersion` constant against the `migrated_version` stored in `settings.json`. When they differ, runs all migration steps (today: force-overwrites `~/.makeslop/Dockerfile` from the embedded asset) and stamps the new version. When already up to date, exits immediately. Does not require a prior `init` and is not subject to the home-directory guard.
- `makeslop go` — from within a registered workspace, launches an interactive, project-scoped docker container with the workspace source tree and per-workspace + global agent config (`.claude/`, `.codex/`, `CLAUDE.md`, `docs/`) mounted in. Exits with the container's exit code. Refuses to launch when stdin or stdout is not a TTY. If no ancestor is registered, exits non-zero with a hint to run `makeslop init`.

### Requirements

- The `docker` CLI must be on `PATH`. `makeslop` shells out to it; there is no Go-side docker SDK dependency.
- The `fd` CLI (or its Debian/Ubuntu alias `fdfind`) must be on `PATH`. `makeslop` uses it to scan for `.env` files before launching the container. Install from [https://github.com/sharkdp/fd](https://github.com/sharkdp/fd). If `fd`/`fdfind` is not found, `makeslop` refuses to launch and prints an install hint.

### Container layout

`makeslop go` runs (workdir `/workspace/<name>`, where `<name>` is the registered workspace's cache-dir basename):

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

Security flags inside the container: `--tmpfs /tmp:size=100m`, `--cap-drop ALL`, `--security-opt no-new-privileges`. Mounts are emitted as `--mount type=bind,source=...,target=...` so paths containing `:` do not break parsing.

### TTY policy

`makeslop` is interactive-only. When stdin or stdout is not a TTY it exits non-zero with:

```
makeslop: stdin/stdout must be a TTY; makeslop is interactive-only
```

### Secret masking

Before launching the container, `makeslop` runs `fd` (or `fdfind`) to locate every file whose basename ends in `.env` under the project root. For each match, an additional `--mount type=bind,source=/dev/null,target=<workspace>/<rel>` flag is appended to the docker invocation so the container sees a zero-byte file at that path instead of the real secret.

- Masked: `.env`, `local.env`, `sub/dir/.env` (basename ends exactly in `.env`).
- Not masked: `.env.local`, `.envrc`, `*.pem`, `*.key` (out of scope).
- Excluded from scan: `.git/`, `node_modules/`, `vendor/`, `.venv/` subtrees.
- `.gitignore` is intentionally ignored (`--no-ignore` flag) because most `.env` files are gitignored — that is precisely why the scan is necessary.

When at least one file is masked, `makeslop` prints `makeslop: masked N .env file(s)` to stderr. Zero hits are silent.

Secret masking is **non-negotiable**: if `fd`/`fdfind` is not on `PATH`, `makeslop` refuses to launch:

```
makeslop: fd/fdfind CLI required for secret scanning; install: https://github.com/sharkdp/fd
```

### Project-local exclusions

`makeslop init` creates a `.makeslop.yaml` file at the project root with an empty-list stub:

```yaml
exclude:
  dirs: []
  files: []
```

Edit this file to hide additional directories and files from the container on every `makeslop go` invocation:

- Entries under `exclude.dirs` are mounted as an empty in-memory tmpfs, so the container sees an empty directory at that path instead of the real contents.
- Entries under `exclude.files` are overlaid with `/dev/null`, so the container sees a zero-byte file at that path.

All paths must be relative to the project root. Example:

```yaml
exclude:
  dirs:
    - node_modules        # large build artifact — skip it entirely
    - secrets             # local secrets directory
  files:
    - secrets/local.env   # specific file overlay
```

The existing `.env` auto-scan (see [Secret masking](#secret-masking)) still runs unconditionally. The YAML entries layer on top; if the same path is found by the scan and listed in `exclude.files`, only one overlay mount is emitted. A YAML parse error aborts the launch before docker is invoked — it is symmetric with a scan failure.

**Reserved paths.** The paths `.claude`, `.codex`, `docs`, and `CLAUDE.md` are already mounted by `makeslop go` for agent state. Listing them in `.makeslop.yaml` is rejected with an error (`projectconfig: path %q collides with a reserved agent path`).

### Controlled network access via a remote proxy

By default `makeslop go` uses Docker's default bridge networking, giving the container unrestricted outbound access. You can restrict outbound access to a controlled egress proxy by adding a `network:` section to `.makeslop.yaml`:

```yaml
network:
  proxy:
    address: 10.0.0.5:8888   # host:port of your upstream HTTP forward proxy
```

When `network.proxy.address` is set, `makeslop go`:

1. Starts a host-side forward proxy listening on a per-invocation unix socket under `/tmp`.
2. Runs the container with `--network none` (no direct internet access), bind-mounts the socket read-only into the container at `/tmp/makeslop-proxy.sock`, and sets `HTTP_PROXY` and `HTTPS_PROXY` to `unix:///tmp/makeslop-proxy.sock`.
3. Tears down the proxy (closes listener, unlinks socket) when the container exits.

The upstream address (`10.0.0.5:8888`) is a plain HTTP forward proxy such as [tinyproxy](https://tinyproxy.github.io/). The container app speaks standard forward-proxy protocol (HTTP `CONNECT` for HTTPS, absolute-URL requests for plain HTTP); the unix socket is fully transparent.

**Important — `unix://` proxy URL client support.** Setting `HTTP_PROXY=unix://...` is **not** honored by all HTTP clients:

| Client | `unix://` proxy support |
|--------|------------------------|
| `curl` (recent versions, `--proxy unix://`) | Yes |
| Node.js `undici` | Yes |
| Python `requests` / `urllib3` | **No** |
| Go `net/http` (default transport) | **No** |

With `--network none`, any client that ignores the env var gets **no** network access at all — there is no fallback to direct internet. This feature is therefore scoped to containers whose HTTP clients support `unix://` forward proxy URLs. Verify client support before enabling this feature in production.

Use `--dry-run` to preview the resulting `docker run` command, including all exclusion mounts, before launching:

```
makeslop go --dry-run
```

### Docker container settings

The image and shell are configurable via `settings.json`. Defaults are `claudebox` and `/bin/zsh`:

```json
{
    "version": 1,
    "image": "claudebox",
    "shell": "/bin/zsh",
    "workspaces": { },
    "migrated_version": 1
}
```

Omitted or empty `image`/`shell` fields fall back to the defaults; existing `settings.json` files predating these keys keep working unchanged. `migrated_version` is written by `makeslop migrate` to record which migration generation the directory is at; absent means 0 (pre-migration).

### Dry run

Pass `--dry-run` (short: `-n`) to print the exact `docker run` invocation that makeslop would execute and then exit without launching docker. The output is a multi-line, backslash-continued, paste-ready shell command on stdout. All pre-launch checks still run (home-dir guard, workspace lookup, secret scan, settings load), so the printed command equals the real invocation byte-for-byte.

```
makeslop go --dry-run
makeslop go -n
```

Because the TTY check lives inside `docker run` (which is skipped on dry-run), `--dry-run` succeeds even when stdin/stdout are pipes. This makes it suitable for CI inspection:

```
makeslop go -n > cmd.sh   # capture only the command; masked-file count goes to stderr
```

### Exit codes

- `0` — success (`init` registered/reused a project, or the container exited cleanly).
- container's exit code — `makeslop go` propagates `exit N` from the container as the host's exit code.
- `1` — any other failure: no workspace registered for pwd, no TTY available, corrupt `settings.json`, I/O error, etc. The reason is written to stderr.

### Home-directory guard

By default, `makeslop go` and `makeslop init` refuse to run from any directory outside the user's home directory. This prevents accidentally registering sensitive system paths (e.g. `/`, `/etc`) as workspaces and mounting them into a container. On violation the tool prints:

```
makeslop: refusing to run from <pwd> (outside <home>); pass --out-of-home to override
```

Pass `--out-of-home` (a persistent flag inherited by all subcommands) to bypass this check:

```
makeslop --out-of-home go
makeslop --out-of-home init
```

### Path resolution

`makeslop` resolves the current working directory through `filepath.EvalSymlinks` before consulting the cache. As a result `/tmp/foo` and `/private/tmp/foo` (the macOS-style symlinked form) map to the same workspace, and registering via either alias is idempotent. The key stored in `settings.json` is always the fully-resolved path. The same applies to symlinked home directories on Linux hosts.

## Build

```
go build ./cmd/makeslop
go test ./...
```
