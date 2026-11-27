# Architecture

The CLI lives in `internal/cli`. It loads settings and project configuration, checks Docker prerequisites, then builds and runs a container spec. `cmd/makeslop/main.go` passes the build version and arguments to `cli.Main`.

## Spec and runtime

`run` and `bg run` share `prepareLaunch` in `internal/cli/run.go`. It returns resolved `docker.BuildInputs` plus a `cache.Plan`: the builders need the mount list, while the CLI keeps the plan for directory creation. Filesystem checks and host environment lookups stay outside the builders.

`BuildForeground` and `BuildBackground` in `internal/docker/create_spec.go` return distinct types accepted by `Run` and `RunBG`, respectively. Each wraps a private `createSpec` with the final command, environment, mounts, and lifecycle flags. Dry runs render that spec as a shell command; real launches use it to build the Docker SDK create request. Neither renderer chooses the launch mode.

`projectconfig.Load` parses `environments:` without reading host variables. `internal/cli/run.go` resolves them and sorts the final `KEY=VALUE` pairs before building the spec. Network settings follow the same path: project config to `BuildInputs`, then to both shell and SDK projections. Image and network preflight read those same inputs.

### Mount order

`BuildInputs.Layout.Main` is the writable main project; joins follow in flag order. The CLI resolves each host and container root once and passes the same `docker.Layout` to cache validation and spec construction. Both reject relative host roots and require clean, absolute container paths. Mounts are added in this order:

1. Main-project root.
2. Main-project policy mounts: a read-only `.makeslop.yaml` bind and a `.git/hooks` tmpfs when applicable.
3. Explicit `BuildInputs.CacheMounts`, ordered shared, local, static.
4. Main-project secret masks.
5. Each join's root, writable policy mounts, and masks.

Masks must follow the mounts they cover. A `/dev/null` mask for `.makeslop.yaml` is dropped when it would hide the read-only config bind. Read-only joins need no policy mounts, though their config may still be masked. Cache mounts apply only to the main project.

### Cache resolution and preparation

`internal/projectconfig` validates cache declarations. The CLI supplies `cache.Resolve` with the project layout, host source locations, and declared excludes, including paths that do not yet exist. `internal/cache` checks source types, managed-directory containment, and destination conflicts without creating anything. `Plan.Mounts()` returns a copy of the validated mounts; absent `cache:` configuration contributes none.

Only real launches call `Plan.Prepare`, after image and network preflight. It rechecks all managed directories before creating any. Dry runs therefore report invalid cache sources or conflicting destinations without populating the cache.

## Docker boundary

`internal/docker/client.go` defines the narrow `apiClient` subset of the moby SDK. Its compile-time assertion catches signature drift. `docker.New` uses an environment-configured client; tests inject fakes through `WithClient`. When adding an SDK method, update `apiClient` and the fakes in `internal/docker/fakes_test.go` and `run_test.go`.

The CLI uses the consumer-side `dockerAPI` interface in `internal/cli/deps.go`. `dockerDeps` holds one implementation, and tests inject `fakeDocker` through `newRootCmdWithDeps`. If client construction fails, `dockerNewErrStub` defers the error until a Docker command runs, leaving commands such as `config` and `ls` usable.

`Run` creates, attaches, starts, streams, and waits for the container. It registers the wait before start so a fast, auto-removed container still yields an exit status. Details of terminal cleanup and input handling are documented beside the code in `internal/docker/run.go`.

### Background identity and lifecycle

`internal/cli/bg.go` reuses foreground preparation. For a preview it supplies an example UUID; for execution it generates a fresh v4 UUID. `BuildBackground` sets the final zmx command and reserved environment directly, without constructing a foreground spec. The create request names the container `makeslop-<uuid>`, labels it with the UUID, project, and shell, and disables auto-removal. `lookupBG` in `internal/docker/bg_identity.go` requires the name and UUID label to agree before acting on the daemon's container ID.

`RunBG` in `internal/docker/bg_lifecycle.go` does not treat a successful Docker start as a ready terminal: it waits for the `agent` zmx session and attempts to remove the newly created container if startup fails. After readiness, CLI prints the UUID to stdout for detached runs. With `--attach`, it prints the UUID to stderr before attaching, even under `--quiet`, so the user can reconnect if attachment fails.

`AttachBG` joins the named terminal via Docker exec; `ExecBG` starts an independent interactive process. Stopping preserves the container and its writable layer, restarting reuses that container, and explicit removal requires it to be stopped.

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
