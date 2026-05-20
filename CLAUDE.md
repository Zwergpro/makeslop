# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Test command

Run `go test ./...`. In environments where `/tmp` is mounted `noexec`, redirect the Go build cache to a local path:

```
mkdir -p .gocache && GOTMPDIR=$(pwd)/.gocache go test ./...
```

`.gocache/` is gitignored.

## Package layout

- `cmd/makeslop/` — CLI entry point and cobra commands (root lookup, `init` subcommand).
- `internal/cache/` — settings load/save and project resolution (ancestor walk, name derivation).

## Invariants

- **POSIX-only.** This project targets POSIX hosts (Linux, macOS). Windows is not supported; any test that exercises symlinks or `/`-paths may skip on Windows with a brief comment referring to this policy.
- `cache.Lookup` and `cache.Init` **require** the caller to pass an absolute, `filepath.EvalSymlinks`-evaluated path as `pwd`. The CLI layer (`cmd/makeslop/main.go` → `resolvePwd`) enforces this; any new caller must do the same. The convention is also documented in the godoc on those methods. Tests that drive these functions directly should use `filepath.EvalSymlinks(t.TempDir())` (see `evalSymlinks` helper in `internal/cache/cache_test.go` and `cmd/makeslop/main_test.go`) — on macOS-style hosts where `/tmp` is itself a symlink, raw `t.TempDir()` paths violate the precondition.
- `cmd/makeslop/main.go` sets `SilenceErrors: true` on the root command, which **cobra propagates to all subcommands**. To avoid silently swallowing real errors, `main()` prints any non-`errSilent` error from `Execute()` to stderr itself. The `errSilent` sentinel is returned by a `RunE` after it has already written a tailored message (e.g. the "no project registered" hint) and signals main() to skip the reprint.
