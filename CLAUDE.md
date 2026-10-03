# Contributor notes

makeslop runs Claude Code or Codex in a per-project Docker container. Users supply the image; makeslop never builds or pulls it. Docker calls use the moby Go SDK, not the Docker CLI. User-facing behavior is documented in `docs/`; [architecture](docs/architecture.md) covers the module boundaries.

## Commands

```sh
go build ./cmd/makeslop                         # version defaults to dev
make build                                      # install with git-describe version
go test -timeout=100s ./...                     # CI suite
go test ./internal/docker -run TestBuildSpec    # focused test
golangci-lint run                               # CI pins v2.12.2
```

`cmd/makeslop/main.go` passes `main.version` and args to `cli.Main`. Release builds override the version with `-ldflags`.

## Code map

- `internal/cli`: cobra commands, dependency injection, preflight, and exit codes.
- `internal/docker`: pure spec construction and SDK calls. `spec.go` renders both dry-run and SDK forms; drift tests keep them aligned.
- `internal/config`: settings, bootstrap, and file locking.
- `internal/projectconfig`: strict `.makeslop.yaml` parsing and validation.
- `internal/workspace`: project registration and cache directories.
- `internal/security`: basename-glob secret scan.
- `examples/claudebox/Dockerfile`: optional image users build themselves.

## Invariants

- `BuildSpec` has no filesystem or process side effects. Resolve paths, host environment values, and policy flags in the CLI first. The main project is `Options.Projects[0]`; joins follow flag order. Masks come after the mounts they cover. `Spec.Sections` affects display only; joined dry-run output has labels and is not paste-ready.
- The main project's global mounts are always present. `MountAgentCache` and `MountContentCache` control per-workspace overlays; their Go zero values are `false`. Absent `cache:` YAML defaults both to `true`.
- Add new SDK calls to `apiClient` and its fakes in `internal/docker/fakes_test.go`. CLI Docker dependencies belong in `dockerAPI`, `dockerNewErrStub`, and `fakeDocker`. Build command tests with `newRootCmdWithDeps`. A failed `docker.New()` must leave non-Docker commands usable.
- `runWithExitCode` wraps execution in a signal-cancellable context. Every `RunE` uses `cmd.Context()`. Preflight has a 10-second deadline; interactive `Run` does not. `ImageExists` returns `(false, nil)` only for a classified not-found error. `*docker.ExitError` passes the container's status through; `errSilent` avoids printing an already shown error.
- Image resolution is flag, then setting, then an error. There is no default or automatic pull. `run` resolves the image before workspace lookup. `status -i` can inspect an image even when settings are unreadable.
- Every settings read-modify-write uses `config.Update` or `config.WithLock`. Never nest `WithLock`, including inside an `Update` callback; it self-deadlocks.
- `.makeslop.yaml` must be a regular file. Strict YAML decoding rejects unknown keys. `environments:` and `networks:` use raw nodes for targeted validation; aliases and duplicate keys need explicit handling. Environment errors must not print values. `projectconfig.Load` never reads the process environment; `resolveEnv` handles host lookup and final sorting.
- `network_mode` and `networks` are mutually exclusive. Unset and built-in modes skip network inspection. Named networks and `container:` targets need preflight; joins contribute only `exclude:` settings. `LoadExisting` prevents a removed join config from silently disabling masks.
- `security.Scan` has no built-in patterns. Empty patterns skip the walk; walk errors abort launch. Matching symlinks are reported but not masked. Warnings about degraded masking bypass `--quiet`.
- The TTY check applies only to `run`. The home guard applies to `init`, `run`, and each join; `--out-of-home` covers all roots. `--quiet` suppresses stderr notices, never errors or stdout. Keep commands other than `run` pipe-safe.
- This project targets POSIX systems. Tests that depend on TTY or signals use package-local `skipNonPOSIX(t, why)`.
