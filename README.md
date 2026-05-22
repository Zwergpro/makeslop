# makeslop

`makeslop` is a CLI that maps the current working directory to a per-workspace cache directory under `~/.makeslop/` for use with docker-based workflows.

## Cache layout

```
~/.makeslop/
├── settings.json
└── workspaces/
    └── <basename>-<sha256[:6]>/
```

`settings.json` records each registered workspace keyed by its absolute, symlink-evaluated path. The per-workspace cache directory under `workspaces/` is where future milestones will store docker artifacts, logs, and other build outputs.

## Usage

- `makeslop init` — registers the current working directory as a workspace. If pwd is already a subdirectory of a registered workspace, the existing workspace's cache path is returned (idempotent, no mutation). Otherwise a new entry is added to `settings.json`, the cache directory is created, and its absolute path is printed.
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

### Docker container settings

The image and shell are configurable via `settings.json`. Defaults are `claudebox` and `/bin/zsh`:

```json
{
    "version": 1,
    "image": "claudebox",
    "shell": "/bin/zsh",
    "workspaces": { }
}
```

Omitted or empty `image`/`shell` fields fall back to the defaults; existing `settings.json` files predating these keys keep working unchanged.

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
