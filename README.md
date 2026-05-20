# makeslop

`makeslop` is a CLI that maps the current working directory to a per-project cache directory under `~/.makeslop/` for use with docker-based workflows.

## Cache layout

```
~/.makeslop/
├── settings.json
└── projects/
    └── <basename>-<sha256[:6]>/
```

`settings.json` records each registered project keyed by its absolute, symlink-evaluated path. The per-project cache directory under `projects/` is where future milestones will store docker artifacts, logs, and other build outputs.

## Usage

- `makeslop init` — registers the current working directory as a project. If pwd is already a subdirectory of a registered project, the existing project's cache path is returned (idempotent, no mutation). Otherwise a new entry is added to `settings.json`, the cache directory is created, and its absolute path is printed.
- `makeslop` — prints the cache path for the current working directory by walking ancestors and looking up the first registered project. Exits non-zero with a hint to run `makeslop init` if no ancestor is registered. Never writes to disk.

### Exit codes

- `0` — success (path printed to stdout, or `init` registered/reused a project).
- `1` — any failure: no project registered for pwd, corrupt `settings.json`, I/O error, etc. The reason is written to stderr.

### Path resolution

`makeslop` resolves the current working directory through `filepath.EvalSymlinks` before consulting the cache. As a result `/tmp/foo` and `/private/tmp/foo` (the macOS-style symlinked form) map to the same project, and registering via either alias is idempotent. The key stored in `settings.json` is always the fully-resolved path. The same applies to symlinked home directories on Linux hosts.

## Build

```
go build ./cmd/makeslop
go test ./...
```
