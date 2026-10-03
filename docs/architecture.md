# Architecture

The CLI lives in `internal/cli`. It loads settings and project configuration, checks Docker prerequisites, then builds and runs a container spec. `cmd/makeslop/main.go` passes the build version and arguments to `cli.Main`.

## Spec and runtime

`internal/docker/spec.go` builds a pure `Spec` from resolved `Options`. `Args()` and `ShellCommand()` render the dry run; `ContainerConfig()`, `HostConfig()`, and `NetworkingConfig()` render the SDK request. Drift tests keep printed flags and daemon settings aligned. Filesystem checks, host environment lookup, and SDK calls happen before or after spec construction, never inside it.

`projectconfig.Load` parses `environments:` without reading host variables. `internal/cli/run.go` resolves them and sorts the final `KEY=VALUE` pairs before building the spec. Network settings follow the same path: project config to `Options`, then to both CLI and SDK projections.

### Mount order

`Options.Projects[0]` is the writable main project; joins follow in flag order. Mounts are added in this order:

1. Project root, then the global `~/.makeslop` agent config mounts.
2. Main-project policy mounts: a read-only `.makeslop.yaml` bind and a `.git/hooks` tmpfs when applicable.
3. Per-workspace agent and content overlays, controlled by `MountAgentCache` and `MountContentCache`.
4. Main-project secret masks.
5. Each join's root, writable policy mounts, and masks.

Masks must follow the mounts they cover. A `/dev/null` mask for `.makeslop.yaml` is dropped when it would hide the read-only config bind. Read-only joins need no policy mounts, though their config may still be masked. Global and cache mounts apply only to the main project.

Absent `cache:` config enables both overlay groups. Go's zero-value flags disable them, so direct `BuildSpec` callers must opt in. Joined dry runs include group labels for inspection; those labels make the output unsuitable for pasting into a shell.

## Docker boundary

`internal/docker/client.go` defines the narrow `apiClient` subset of the moby SDK. Its compile-time assertion catches signature drift. `docker.New` uses an environment-configured client; tests inject fakes through `WithClient`. When adding an SDK method, update `apiClient` and the fakes in `internal/docker/fakes_test.go` and `run_test.go`.

The CLI uses the consumer-side `dockerAPI` interface in `internal/cli/deps.go`. `dockerDeps` holds one implementation, and tests inject `fakeDocker` through `newRootCmdWithDeps`. If client construction fails, `dockerNewErrStub` defers the error until a Docker command runs, leaving commands such as `config` and `ls` usable.

`Run` creates, attaches, starts, streams, and waits for the container. It registers the wait before start so a fast, auto-removed container still yields an exit status. Details of terminal cleanup and input handling are documented beside the code in `internal/docker/run.go`.

## Preflight and errors

`CheckDaemon`, `ImageExists`, `ContainerRunning`, and `NetworkExists` share one client. Inspect helpers return `false, nil` only for a classified not-found error; transport and daemon errors propagate. A container must be running, unpaused, and not restarting before another container can join its network namespace.

CLI preflight calls have a 10-second timeout; interactive `Run` has no deadline. `networkPreflight` skips unset and built-in modes, inspects `container:` targets and named networks, and reports errors under the corresponding YAML key. Dry runs skip daemon preflight.

`resolveImage` chooses `-i/--image` before the saved image. There is no default or automatic pull. It validates the reference before workspace lookup so an invalid setting fails consistently, including in dry runs.

`runWithExitCode` passes through `docker.ExitError.Code`, including signal-derived codes such as 137. Other errors exit 1; `errSilent` avoids printing a message already shown by the command.

## Configuration and scanning

`~/.makeslop/settings.json` has no schema version. `config.Load` defaults the shell and tmpfs size, while the image remains unset. Unknown keys disappear on the next save, so schema changes should add compatible fields and load-time defaults. Read-modify-write operations use `config.Update` and `config.WithLock`; nesting `WithLock` deadlocks.

`internal/security.Scan` walks the project with basename globs from `.makeslop.yaml`. Empty patterns skip the walk. Matching symlinks are reported but cannot be masked because `WalkDir` does not follow them. Walk errors abort the launch so unreadable paths cannot silently bypass masking. `init` seeds scan defaults; existing project files are never rewritten.

The project targets POSIX systems. TTY and signal tests use package-local `skipNonPOSIX` helpers.

## Build

```sh
go build ./cmd/makeslop
go test -timeout=100s ./...
golangci-lint run
```

Builds without an injected version print `dev`. Release builds set `main.version` with `-ldflags`. Tests use fake Docker clients and need no live daemon.
