# makeslop

A sandboxed runner for Claude Code and Codex: isolates your AI agent in a per-project Docker
container with controlled mounts and secret masking.

## What & why

makeslop gives each project its own container launched from a single shared base image. The agent
gets your source tree plus its own persistent state directories (`.claude/`, `.codex/`, `docs/`),
but nothing else from your host — no other projects, no ambient host environment (credentials in the shared agent config dirs like `.claude/` are present by design).

Why use it:
- **Isolation** — each project runs in its own container; no credential leakage between projects.
- **Secret masking** — `.env`, PEM keys, and SSH keys are overlaid with `/dev/null` before launch.
- **Reproducible** — one shared `claudebox` image, one Dockerfile, one `makeslop build`.

## Requirements

- Docker daemon reachable (via `DOCKER_HOST` or `/var/run/docker.sock`).
- The `docker` CLI binary is **not** required — makeslop uses the moby/moby Go SDK directly.

## Install

**Go install (latest):**
```
go install github.com/Zwergpro/makeslop/cmd/makeslop@latest
```

**Prebuilt binaries** are published on [GitHub Releases](https://github.com/Zwergpro/makeslop/releases)
for each tagged version (built with [GoReleaser](https://goreleaser.com)).

## Quickstart

```
# 1. From your project directory — register and seed the base config
makeslop init

# 2. Build the claudebox Docker image (once, or after a migrate)
makeslop build

# 3. Launch an interactive agent session
makeslop run
```

That's the normal flow. `migrate` is an explicit upgrade step, not part of first-run setup —
`init` always seeds at the latest version so a freshly initialized directory is never stale.

## How it works

```
┌─ your terminal ──────────────────────────────────────────────────────────┐
│  makeslop run                                                             │
│    │  scans for secrets (masks with /dev/null)                           │
│    │  mounts project root + per-project agent state                      │
│    ▼                                                                      │
│  ┌── claudebox container (--cap-drop ALL, --security-opt no-new-privileges) ─ │
│  │  /workspace/<name>   ← your project root (bind-mounted)               │
│  │  /home/user/.claude/ ← global agent config                            │
│  │  /tmp                ← tmpfs (default 100m, not on disk)              │
│  └──────────────────────────────────────────────────────────────────────  │
└───────────────────────────────────────────────────────────────────────── ┘
```

The container has normal Docker bridge networking and full internet access.

## Configuration

`makeslop init` creates a `.makeslop.yaml` at the project root. Edit it to control secret masking
and directory/file exclusions:

```yaml
exclude:
  scan:
    patterns:
      - "*.env"
      - ".env.*"
      - "*.pem"
      - "*.key"
      - "*.p12"
      - "*.pfx"
      - "*.tfstate"
      - "id_rsa*"
      - "id_ed25519*"
      - ".npmrc"
      - ".netrc"
      - ".git-credentials"
      - ".pypirc"
      - ".htpasswd"
      - "service-account*.json"
      - "kubeconfig"
      - "*.kubeconfig"
    skip-dirs:
      - .git
      - node_modules
      - vendor
      - .venv
  dirs: []    # mount these as empty tmpfs inside the container
  files: []   # overlay these with /dev/null inside the container
```

Inject static environment variables into the container with an `environments:` block:

```yaml
environments:
  HTTP_PROXY: "http://192.168.1.1:11111"
```

Values must be scalars; numbers and booleans are coerced to strings. Absent block = no `-e` flags (backward-compatible). See [docs/reference.md](docs/reference.md#environment-variables-environments-block-in-makeslopya-ml) for the full spec.

Global settings (`~/.makeslop/settings.json`) control the image tag, shell, and `/tmp` size:
```
makeslop config set image claudebox
makeslop config set shell /bin/zsh
makeslop config set tmp_dir_size 100m
```

## Security at a glance

Secret masking is config-driven: patterns in `exclude.scan.patterns` are basename globs; matched
files are overlaid with `/dev/null` so the agent sees a zero-byte file instead of the real secret.
Walk errors are fatal — if makeslop cannot prove a directory is secret-free it refuses to launch.
See [docs/security.md](docs/security.md) for the full masking spec and home-directory guard.

## Commands

| Command | What it does |
|---|---|
| `makeslop init` | Register project, seed `~/.makeslop/` at latest version |
| `makeslop build` | Build (or rebuild) the `claudebox` Docker image |
| `makeslop run` | Launch an interactive agent container (TTY required) |
| `makeslop status` | Ordered readiness check: daemon, config, image, workspace, secrets |
| `makeslop migrate` | Upgrade `~/.makeslop/` when the binary ships a newer migration version |
| `makeslop config` | View or set global settings (`image`, `shell`, `tmp_dir_size`) |
| `makeslop version` | Print the build version |

`makeslop run --dry-run` prints the equivalent `docker run` command without launching.
`makeslop build --refresh` resets `~/.makeslop/Dockerfile` to the embedded shipped version before building (useful after hand-editing).

## Documentation

- [Command & Runtime Reference](docs/reference.md) — all flags, mount table, exit codes, settings schema
- [Security](docs/security.md) — secret masking, egress model, home-directory guard
- [Architecture](docs/architecture.md) — design patterns, module boundaries, contributing

## License

MIT
