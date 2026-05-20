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
- `internal/workspace/` — settings load/save and workspace resolution (ancestor walk, name derivation).

## Invariants

- **POSIX-only.** This project targets POSIX hosts (Linux, macOS). Windows is not supported; any test that exercises symlinks or `/`-paths may skip on Windows with a brief comment referring to this policy.
- `workspace.Workspaces.Lookup` and `workspace.Workspaces.Init` **require** the caller to pass an absolute, `filepath.EvalSymlinks`-evaluated path as `pwd`. The CLI layer (`cmd/makeslop/main.go` → `resolvePwd`) enforces this; any new caller must do the same. The convention is also documented in the godoc on those methods. Tests that drive these functions directly should use `filepath.EvalSymlinks(t.TempDir())` (see `evalSymlinks` helper in `internal/workspace/workspace_test.go` and `cmd/makeslop/main_test.go`) — on macOS-style hosts where `/tmp` is itself a symlink, raw `t.TempDir()` paths violate the precondition.
- `cmd/makeslop/main.go` sets `SilenceErrors: true` on the root command, which **cobra propagates to all subcommands**. To avoid silently swallowing real errors, `main()` prints any non-`errSilent` error from `Execute()` to stderr itself. The `errSilent` sentinel is returned by a `RunE` after it has already written a tailored message (e.g. the "no workspace registered" hint) and signals main() to skip the reprint.

## Comment style

- Comment **why**, not **what**. Readable code does not need narration.
- 1 line by default; 2–3 lines max for genuinely subtle logic.
- Place comments above the non-obvious block or branch they explain.
- Do **not** restate identifiers, paraphrase logic, or document trivial Go.
- Do **not** write long doc blocks. Godoc on exported APIs stays short and contract-focused (preconditions, idempotency, error semantics).
- Tests: rely on test names. Add a comment only when the test guards a specific regression or contract that the name alone can't convey.
