# CLAUDE.md — makeslop project notes

## Key architectural patterns

### Pure/impure split
Argv assembly (`internal/docker/spec.go`) is pure and fully table-tested.
Side-effecting exec lives in `internal/docker/run.go`.
Keep these separate: pure functions never touch the filesystem or exec anything.

### testing.go in the production binary (known trade-off)
`internal/docker/testing.go` is compiled into the production binary — it is **not** a `_test.go` file.
This is intentional: `cmd/makeslop/main_test.go` is in `package main`, not `package docker_test`,
so it cannot reach unexported symbols via an `export_test.go` bridge. Shipping the test helpers
(WriteShim, WriteBuildShim, SetDockerBinaryForTest, SetTTYCheckForTest) into the production binary
is the accepted trade-off for testability. The binary size impact is negligible.

### /tmp is noexec in this CI environment
Shell shims (scripts exec'd as docker substitutes) must be written to an **executable** directory.
In this environment `/tmp` is mounted noexec, so shims cannot live there.
The fix: `executableTempDir` (in `run_test.go`) and `executableTempDirForCmd`
(in `main_test.go`) delegate to `t.TempDir()`, which honours the `GOTMPDIR` env var.
Run tests with `GOTMPDIR=/home/user` so that shims land under `/home/user`, which is executable.

Production code (`Build` in `run.go`) uses `os.MkdirTemp("", ...)` for the **build context dir**
(not a shim). The context dir does not need to be executable — docker sends it over the daemon
socket, not via exec — so `/tmp` is fine there.

### Test/production divergence for docker shims vs. build context
- **Shims** (test-only fake docker binaries): must live on an executable filesystem; achieved
  via `t.TempDir()` + `GOTMPDIR=/home/user` (noexec /tmp constraint).
- **Build context dir** (real empty temp dir passed to docker build): lives in `/tmp` via
  `os.MkdirTemp("", ...)` — only needs to exist, not be executable.

### POSIX-only invariant
makeslop targets POSIX systems only. Tests that rely on shell shims call `SkipNonPOSIX` at the top.
Do not add Windows compatibility paths.

### TTY requirement is `go`-only
`makeslop go` requires an interactive TTY (checked via `ttyCheck`).
`makeslop build`, `makeslop init`, `makeslop migrate`, `makeslop config`, and `makeslop version` are CI/pipe-safe and never consult `ttyCheck`.

### Home-directory guard exemptions
`makeslop go` and `makeslop init` enforce the home-directory guard.
`makeslop build`, `makeslop migrate`, `makeslop config`, and `makeslop version` are exempt — they operate on `~/.makeslop/` directly
and do not care about the current working directory.

### MigrationVersion-on-Dockerfile-change rule
Whenever `internal/assets/files/Dockerfile` is modified (e.g. multi-arch support), bump
`MigrationVersion` in `internal/config/config.go` so that existing installs pick up the new
Dockerfile on the next `makeslop migrate`. `CurrentVersion` (settings schema version) is separate
and only changes when the `Settings` struct fields change. The two constants serve different
purposes: `CurrentVersion` gates JSON schema compatibility; `MigrationVersion` gates the one-shot
directory refresh.

### Proxy probe-dial invariant
`Proxy.Start` performs a single TCP probe-dial of the upstream address before accepting any
connections. If the upstream is unreachable, `Start` tears down the listener and socket and returns
the error — the caller (`runGo`) must abort the container launch. This "fail loud" invariant prevents
silent black-holing of container traffic when the upstream proxy is misconfigured. The probe checks
TCP reachability only; it does not validate HTTP CONNECT protocol. Tests that call `Start` must use
a live fake upstream (a `net.Listen("tcp", "127.0.0.1:0")` helper) — passing a dead address such as
`127.0.0.1:1` will cause `Start` to return an error.
