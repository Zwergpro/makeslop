# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Test command

Run `go test ./...`. In environments where `/tmp` is mounted `noexec`, redirect the Go build cache to a local path:

```
mkdir -p .gocache && GOTMPDIR=$(pwd)/.gocache go test ./...
```

`.gocache/` is gitignored.

## Package layout

- `cmd/makeslop/` — CLI entry point and cobra commands. Bare `makeslop` (registered workspace) launches docker via `internal/docker`; `init` subcommand registers pwd.
- `internal/workspace/` — settings load/save and workspace resolution (ancestor walk, name derivation). `Lookup` returns `(matchedRoot, cacheDir, error)` so callers can mount the registered ancestor — not pwd — into the container.
- `internal/docker/` — container assembly and exec: pure `BuildSpec`/`Spec.Args` for argv, `Run` as thin TTY-checked `docker run` glue.

## Invariants

- **POSIX-only.** This project targets POSIX hosts (Linux, macOS). Windows is not supported; any test that exercises symlinks or `/`-paths may skip on Windows with a brief comment referring to this policy.
- `workspace.Workspaces.Lookup` and `workspace.Workspaces.Init` **require** the caller to pass an absolute, `filepath.EvalSymlinks`-evaluated path as `pwd`. The CLI layer (`cmd/makeslop/main.go` → `resolvePwd`) enforces this; any new caller must do the same. The convention is also documented in the godoc on those methods. Tests that drive these functions directly should use `filepath.EvalSymlinks(t.TempDir())` (see `evalSymlinks` helper in `internal/workspace/workspace_test.go` and `cmd/makeslop/main_test.go`) — on macOS-style hosts where `/tmp` is itself a symlink, raw `t.TempDir()` paths violate the precondition.
- `cmd/makeslop/main.go` sets `SilenceErrors: true` on the root command, which **cobra propagates to all subcommands**. To avoid silently swallowing real errors, `main()` prints any non-`errSilent` error from `Execute()` to stderr itself. The `errSilent` sentinel is returned by a `RunE` after it has already written a tailored message (e.g. the "no workspace registered" hint) and signals main() to skip the reprint.
- `internal/docker.Run` requires both stdin and stdout to be TTYs (returns `ErrNoTTY` otherwise); the cobra layer is the only authorized caller, and is responsible for translating `ErrNoTTY` into a user-facing message. TTY detection uses `golang.org/x/term.IsTerminal` (ioctl-based) so non-terminal char devices like `/dev/null` are correctly rejected.
- `internal/docker.BuildSpec` and `internal/docker.Run` require `Options.ProjectRoot` and `Options.BaseDir` to be absolute, `filepath.EvalSymlinks`-resolved paths. The cobra layer is the only authorized origin: `resolvePwd` produces `ProjectRoot` (via the registered ancestor returned by `workspace.Workspaces.Lookup`, never raw pwd), and `main()` resolves `BaseDir` before calling `runWithExitCode`.
- `main()` (via `runWithExitCode`) propagates `*exec.ExitError.ExitCode()` from a container exit as the host's exit code. Signal-killed containers map to `128+signum` when determinable from the underlying `syscall.WaitStatus`, else `255`. The `*exec.ExitError` branch takes priority over `errSilent`; it does not print a "makeslop:" prefix because docker / the container already wrote its own diagnostic.
- `internal/docker` exposes two test-only swap points (`dockerBinary`, `ttyCheck`) via `SetDockerBinaryForTest` / `SetTTYCheckForTest` in `testing.go`. Tests substitute a script shim and a TTY stub directly — do not introduce `PATH` manipulation. See godoc in `internal/docker/testing.go`. These swaps are process-global; tests that touch them must not call `t.Parallel()`.

## Comment style

- Comment **why**, not **what**. Readable code does not need narration.
- 1 line by default; 2–3 lines max for genuinely subtle logic.
- Place comments above the non-obvious block or branch they explain.
- Do **not** restate identifiers, paraphrase logic, or document trivial Go.
- Do **not** write long doc blocks. Godoc on exported APIs stays short and contract-focused (preconditions, idempotency, error semantics).
- Tests: rely on test names. Add a comment only when the test guards a specific regression or contract that the name alone can't convey.
