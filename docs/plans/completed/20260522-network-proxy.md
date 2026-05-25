# Network Proxy for Container

## Overview

Add an opt-in HTTP forward proxy that lets a network-isolated container reach
the outside world through an upstream proxy (e.g. tinyproxy) running on a remote
host. The data path is:

```
Container app ──unix socket──► host-side Go forward proxy ──TCP──► remote upstream proxy ──► internet
```

The feature is configured per-project in `.makeslop.yaml` under a new
`network:` section. When `network.proxy.address` is set, the `go` subcommand:

1. Starts a host-side forward proxy listening on a per-invocation unix socket
   under `/tmp`.
2. Adds `--network none`, bind-mounts the socket into the container, and sets
   `HTTP_PROXY`/`HTTPS_PROXY` to the in-container socket path.
3. Tears the proxy down (closes listener, unlinks socket) when the container
   exits.

When the section is absent, behavior is identical to today (default bridge
networking, no socket, no env vars) — the feature is fully opt-in and
backwards-compatible.

**Problem it solves**: lets a container run with no direct network access yet
still reach a controlled set of endpoints via a remote egress proxy, without
the container needing any awareness of the upstream's address.

**Scope limitation (read first)**: `HTTP_PROXY`/`HTTPS_PROXY` set to a
`unix://` URL is **not** honored by every HTTP client. Recent `curl`
(`--proxy unix://`) and Node `undici` recognize it; Python `requests`/`urllib3`
and Go's `net/http` do **not** parse `unix://` proxy URLs without a custom
transport. Because the container also runs with `--network none`, any client
that ignores the env var gets **no** network at all (silent functional
failure), not a fallback. This feature is therefore scoped to in-container
clients that support `unix://` forward proxies; tools that don't will be
network-isolated. The acceptance criteria reflect this scope, and the README
must state it prominently.

## Context (from discovery)

- **Project**: `makeslop`, a Go CLI that runs docker-based agent containers with
  per-workspace cache. POSIX-only (Linux/macOS).
- **Files/components involved**:
  - `internal/projectconfig/projectconfig.go` — `.makeslop.yaml` parser; the
    trust boundary for user-supplied YAML. `Load(root)` currently returns
    `(Excludes, error)`.
  - `internal/docker/spec.go` — pure `BuildSpec(Options) Spec`; `Spec.Args()`
    and `Spec.ShellCommand()` projections.
  - `internal/networks/` — **new package** for the host-side forward proxy.
  - `cmd/makeslop/main.go` — cobra wiring; `runGo` orchestrates lookup → scan →
    load → buildspec → run.
- **Related patterns found**:
  - `internal/security` and `internal/projectconfig` are existing "trust
    boundary" packages; `internal/networks` follows the same shape.
  - `BuildSpec` is pure (same Options → same Spec): PID/socket-path impurity
    must stay in the cobra layer, passed in as `Options` fields.
  - Tests are co-located, table-driven, and use an `evalSymlinks(t, t.TempDir())`
    helper to honor the absolute-EvalSymlinks-evaluated path precondition.
  - `docker.SkipNonPOSIX(t, ...)` guards symlink/`/`-path tests.
- **Dependencies identified**: Go stdlib only (`net`, `net/http`, `io`,
  `bufio`, `context`, `sync`). No new third-party deps. `gopkg.in/yaml.v3`
  already vendored.

## Development Approach

- **Testing approach**: Regular (code first, then tests within the same task).
- Complete each task fully before moving to the next.
- Make small, focused changes.
- **Every task MUST include new/updated tests** for code changes in that task
  (success + error/edge scenarios), listed as separate checklist items.
- **All tests must pass before starting the next task** — no exceptions.
- Run `mkdir -p .gocache && GOTMPDIR=$(pwd)/.gocache go test ./...` after each
  change (the `/tmp`-noexec workaround documented in CLAUDE.md).
- Maintain backward compatibility: absent `network:` section ⇒ today's behavior.
- **Update this plan file when scope changes during implementation.**

## Testing Strategy

- **Unit tests**: required for every task.
  - `projectconfig`: YAML parse of the `network:` section, `host:port`
    validation (valid, malformed, missing-port), absent-section ⇒ zero value.
  - `networks`: socket lifecycle (Start binds + chmods, Close unlinks +
    idempotent), CONNECT tunnel relays bytes bidirectionally through a fake
    upstream, dial failure on a bad upstream, ctx cancellation interrupts.
  - `docker`: `BuildSpec` emits `--network none` + env + socket mount **only**
    when proxy fields are set; argv/ShellCommand ordering; purity preserved.
  - `cmd/makeslop`: dry-run argv includes the proxy plumbing when YAML configures
    it; absent section ⇒ unchanged argv.
- **e2e tests**: project has no UI; no Playwright/Cypress. Not applicable.
- Network tests use an in-process fake upstream (a `net.Listen("tcp", "127.0.0.1:0")`
  that speaks the minimal CONNECT handshake) — no external network, no real
  tinyproxy, deterministic and hermetic.

## Progress Tracking

- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix.
- Document issues/blockers with ⚠️ prefix.
- Update plan if implementation deviates from original scope.

## Solution Overview

**High-level approach**: a host-side forward proxy is a dumb byte pipe. After a
client sends `CONNECT host:port`, HTTP is over for that connection — the payload
is opaque TLS — so the proxy dials the upstream, replays the request verbatim,
and `io.Copy`s bytes in both directions until either side closes. No HTTP
parsing, no `httputil.ReverseProxy` (which would wrongly try to interpret TLS
bytes as HTTP). This is both simpler and the only correct shape for CONNECT
tunneling.

**Key design decisions**:

1. **`BuildSpec` stays pure.** The PID-derived socket path and the upstream
   address are computed in `runGo` and passed into `docker.Options` as plain
   strings. `BuildSpec` emits the extra argv only when those fields are
   non-empty. Same-Options → same-Spec invariant preserved.
2. **`internal/networks` is the proxy's owner.** It owns the listener, the
   accept loop, per-connection goroutines, the splice, and socket cleanup —
   mirroring how `internal/security` and `internal/projectconfig` own their
   concerns.
3. **Opt-in gate.** `--network none` + env + socket mount are added **only**
   when `network.proxy.address` is configured. Absent section ⇒ no change.
4. **Per-invocation socket.** `/tmp/makeslop-<hash12>-<pid>.sock`, where
   `<hash12>` is the first 12 hex chars of `sha256(workspaceDir)`. **Do not**
   embed the workspace *name* directly: it is `<dirBasename>-<6hex>` and the
   basename is **unbounded** (a long project dir name would push the socket
   path past the 108-byte `sockaddr_un` limit and make `net.Listen("unix", …)`
   fail with `invalid argument`). The fixed-width hash gives per-project
   uniqueness; `<pid>` gives uniqueness across concurrent `makeslop go` runs of
   the same project. Resulting path is ~39 bytes regardless of project name,
   safely under the limit. If the path ever still overflows, `Start` returns
   the bind error and the launch aborts (intentional, per decision 6).
5. **`Load` returns a second value.** `Load(root) (Excludes, Network, error)`.
   `Network{ProxyAddress string}` is `""` when unconfigured. `Load` validates
   the address is a syntactic `host:port` via `net.SplitHostPort` but performs
   **no** network I/O (no reachability dial) — it stays a pure parse step.
6. **Failure aborts launch (symmetric with Scan/YAML).** A proxy `Start` failure
   aborts before `docker.Run`, consistent with the "any masking-pipeline failure
   ⇒ no container" invariant. (Network isolation depends on the socket existing.)

**How it fits**: the `go` RunE wiring becomes
`security.Scan → projectconfig.Load → merge → config.Load → docker.BuildSpec →
(dry-run check: print + return, NO socket) → (proxy Start if configured) →
(defer proxy.Close) → docker.Run`. The dry-run branch stays immediately before
`proxy.Start`/`docker.Run` so it never binds a socket — same placement
discipline as the existing dry-run invariant.

## Technical Details

### YAML schema (additive)

```yaml
exclude:
  dirs: []
  files: []
network:           # optional; absent ⇒ no proxy, default networking
  proxy:
    address: 10.0.0.5:8888   # host:port of the upstream forward proxy
```

`KnownFields(true)` strict decoding already rejects typos. The byte-stable
`Stub` written by `Scaffold` is **unchanged** (proxy is opt-in; scaffold does
not emit the section).

### `projectconfig` additions

```go
// Network is the parsed, validated network config from .makeslop.yaml.
type Network struct {
    ProxyAddress string // "host:port"; "" when unconfigured
}

type yamlSchema struct {
    Exclude struct{ Dirs, Files []string } // unchanged
    Network struct {
        Proxy struct {
            Address string `yaml:"address"`
        } `yaml:"proxy"`
    } `yaml:"network"`
}

func Load(root string) (Excludes, Network, error)
```

Validation: if `Address != ""`, `net.SplitHostPort(Address)` must succeed and
yield a non-empty host and a non-empty port; otherwise return
`fmt.Errorf("projectconfig: invalid network.proxy.address %q: ...", ...)`.

### `internal/networks` package

```go
// Package networks owns the host-side HTTP forward proxy that bridges
// container traffic (over a unix socket) to an upstream HTTP CONNECT proxy.
package networks

type Proxy struct {
    socketPath string       // host path: /tmp/makeslop-<hash12>-<pid>.sock
    upstream   string       // "host:port" of the remote forward proxy
    listener   net.Listener
    cancel     context.CancelFunc // cancels the proxy-scoped ctx in Close
    wg         sync.WaitGroup
}

func NewProxy(socketPath, upstream string) *Proxy
func (p *Proxy) Start(ctx context.Context) error // remove stale, Listen(unix), Chmod 0600, spawn accept loop
func (p *Proxy) Close() error                     // cancel ctx, close listener, wg.Wait, unlink socket; idempotent
func (p *Proxy) SocketPath() string               // accessor for the host path (mount source)
```

**In-flight connection teardown.** `Start` derives a proxy-scoped context
(`ctx, cancel := context.WithCancel(parent)`) and stores `cancel`. Each accepted
connection registers itself so `Close` can force-close it: closing the *listener*
only unblocks `Accept`, it does **not** close already-accepted conns. Without
this, `wg.Wait()` in `Close` would hang whenever the container exits with a live
tunnel. `Close`: call `cancel()`, close the listener, close every tracked conn
(both client and upstream sides), then `wg.Wait()`, then `os.Remove(socketPath)`.
Idempotent and safe after a failed `Start`.

Per-connection handler — explicit half-close so neither `io.Copy` leaks:

```go
func (p *Proxy) handle(ctx context.Context, client net.Conn) {
    defer client.Close()
    up, err := (&net.Dialer{}).DialContext(ctx, "tcp", p.upstream)
    if err != nil { return }
    defer up.Close()

    var io2 sync.WaitGroup
    io2.Add(2)
    go func() { defer io2.Done(); io.Copy(up, client); halfCloseWrite(up) }()
    go func() { defer io2.Done(); io.Copy(client, up); halfCloseWrite(client) }()
    io2.Wait()
}

// halfCloseWrite shuts down the write half so the peer's io.Copy sees EOF and
// returns. Both *net.UnixConn (client) and *net.TCPConn (upstream) implement it.
func halfCloseWrite(c net.Conn) {
    if cw, ok := c.(interface{ CloseWrite() error }); ok {
        _ = cw.CloseWrite()
    }
}
```

The handler forwards bytes verbatim — the container app already speaks
forward-proxy protocol (CONNECT for HTTPS, absolute-URL for plain HTTP), and the
upstream interprets it identically. No request parsing in the data path. The
explicit `CloseWrite()` on each copy's completion lets the opposite direction
drain and return rather than blocking until full TCP close, so handlers
terminate promptly and `wg.Wait()` cannot hang.

### `docker.Options` / `BuildSpec` additions

```go
type Options struct {
    // ... existing fields ...

    // ProxySocketHost is the host path of the per-invocation unix socket.
    // When non-empty, BuildSpec emits --network none, a read-only bind mount of
    // the socket into the container, and HTTP(S)_PROXY env pointing at it.
    // Empty ⇒ no networking changes (default bridge, as today).
    ProxySocketHost string
    // ProxySocketContainer is the in-container socket path. Use /tmp
    // (e.g. /tmp/makeslop-proxy.sock): the container already mounts
    // --tmpfs /tmp, so the path is guaranteed writable and not subject to the
    // /var/run -> /run symlink-to-tmpfs ambiguity present in many images.
    // Constant; the env vars reference it.
    ProxySocketContainer string
}
```

`Spec` gains `NetworkMode string` (e.g. `"none"`) and `Env []string`
(`HTTP_PROXY=...`, `HTTPS_PROXY=...`). `Args()` emits `--network <mode>` and
`-e <kv>` when present; `ShellCommand()` mirrors them. The socket mount is a
`Mount{Host: ProxySocketHost, Container: ProxySocketContainer, ReadOnly: true}`.

**`Mount.ReadOnly` extension.** `Mount` today has no read-only support and the
CLAUDE.md `Mount.Type` invariant pins the exact rendering. Add a
`ReadOnly bool` field rendered as a trailing `,readonly` on the bind `--mount`
(and only on bind — tmpfs ignores it). Existing mounts default `ReadOnly:false`
⇒ rendering is byte-identical to today (backwards-compatible). Update the
CLAUDE.md `Mount.Type` invariant note in Task 6 to document the new field.

**Container socket path rationale.** `/tmp/makeslop-proxy.sock` is chosen over
`/var/run/...` because `/var/run` is commonly a symlink to `/run` (a tmpfs) in
modern images, and bind-mounting a socket file under a symlinked dir behaves
unpredictably. `/tmp` is already a tmpfs mount in the spec and definitely
writable. The `unix://` env values therefore read
`unix:///tmp/makeslop-proxy.sock`.

**Mount ordering note**: the proxy socket mount is independent of the
project/agent/masked overlays; append it after the existing mount groups so the
documented overlay ordering is unaffected.

### `runGo` wiring

```go
yamlExcludes, netCfg, err := projectconfig.Load(workspaceRoot)
// ...
var proxy *networks.Proxy
if netCfg.ProxyAddress != "" {
    h := sha256.Sum256([]byte(workspaceDir))
    sock := filepath.Join("/tmp", fmt.Sprintf("makeslop-%x-%d.sock",
        h[:6], os.Getpid())) // h[:6] => 12 hex chars; bounded path length
    opts.ProxySocketHost = sock
    opts.ProxySocketContainer = "/tmp/makeslop-proxy.sock"
    proxy = networks.NewProxy(sock, netCfg.ProxyAddress)
}
spec := docker.BuildSpec(opts)
if dryRun { print; return nil }   // dry-run: prints proxy argv, does NOT bind a socket
if proxy != nil {
    if err := proxy.Start(cmd.Context()); err != nil { return err }
    defer proxy.Close()
}
return docker.Run(cmd.Context(), spec)
```

**Dry-run invariant**: the dry-run branch prints argv and returns **without**
starting the proxy (no socket is bound), but the printed argv **does** include
the proxy plumbing so the printed command matches what would execute. This keeps
the existing "printed argv == executed argv" invariant.

## What Goes Where

- **Implementation Steps** (`[ ]`): all code, tests, and doc/CLAUDE.md updates —
  achievable within this repo.
- **Post-Completion** (no checkboxes): manual end-to-end verification against a
  real tinyproxy, and the `unix://` proxy-URL client-support caveat.

## Implementation Steps

### Task 1: Parse `network.proxy.address` in projectconfig

**Files:**
- Modify: `internal/projectconfig/projectconfig.go`
- Modify: `internal/projectconfig/projectconfig_test.go`
- Modify: `cmd/makeslop/main.go` (the **only** caller of `Load`, verified by
  grep — the docker/spec.go references are godoc only)

- [x] add `Network` struct (`ProxyAddress string`) and the `network.proxy.address`
      field to `yamlSchema`
- [x] change `Load` signature to `(Excludes, Network, error)`; populate
      `Network.ProxyAddress` from the parsed schema
- [x] validate non-empty address via `net.SplitHostPort` (non-empty host AND
      port); return a wrapped `projectconfig: invalid network.proxy.address`
      error on failure; **no** network I/O
- [x] update the godoc on `Load` and the package doc to describe the new return
      value and the no-reachability-check contract
- [x] update `cmd/makeslop/main.go` `runGo` to receive the third return value
      (discard with `_` for now; wired in Task 4) so the tree compiles
- [x] write tests: valid `host:port`, IPv4:port, host name:port, missing-port,
      empty-host (`:8888`), absent `network:` section ⇒ zero `Network`,
      malformed (`not a url`)
- [x] write tests: existing `Excludes` behavior unchanged (regression)
- [x] run tests — must pass before Task 2

### Task 2: Implement the forward proxy in internal/networks

**Files:**
- Create: `internal/networks/proxy.go`
- Create: `internal/networks/proxy_test.go`

- [x] create `Proxy` with `NewProxy(socketPath, upstream string) *Proxy`
- [x] implement `Start(ctx)`: derive proxy-scoped ctx + store `cancel`,
      `os.Remove` stale socket, `net.Listen("unix", path)`, `os.Chmod(path, 0600)`,
      spawn accept-loop goroutine; return bind errors
- [x] implement the accept loop + per-connection `handle`: `DialContext` upstream,
      two `io.Copy` goroutines each followed by `halfCloseWrite(dst)`, wait both;
      register each conn so `Close` can force-close in-flight tunnels
- [x] implement `halfCloseWrite` (type-assert `interface{ CloseWrite() error }`)
- [x] implement `Close`: `cancel()`, close listener, force-close tracked conns,
      `wg.Wait()`, `os.Remove(socketPath)`; idempotent (safe after a failed
      `Start` and on double-call)
- [x] add `SocketPath()` accessor
- [x] add package doc + godoc describing the verbatim-forward (no HTTP parse)
      contract, the half-close protocol, and the POSIX-only / 108-byte
      socket-path-length note
- [x] write tests: `Start` binds + sets 0600 perms, socket file exists
- [x] write tests: CONNECT tunnel relays bytes both directions through an
      in-process fake upstream (`127.0.0.1:0`)
- [x] write tests: handler terminates when one side half-closes (no goroutine
      leak / no hang) — assert `handle` returns
- [x] write tests: dial failure on bad upstream closes client cleanly (no panic)
- [x] write tests: `Close` unlinks socket, is idempotent, and does **not** hang
      when a connection is in flight; ctx cancel stops accept loop
- [x] guard symlink/POSIX-only tests with `docker.SkipNonPOSIX` (consistent with
      `projectconfig_test.go`) — accepts the `internal/docker` test import
- [x] run tests — must pass before Task 3

### Task 3: Emit proxy argv from docker.BuildSpec

**Files:**
- Modify: `internal/docker/spec.go`
- Modify: `internal/docker/spec_test.go`

- [x] add `ReadOnly bool` to `Mount`; render trailing `,readonly` on bind mounts
      when true (tmpfs ignores it); existing mounts default false ⇒ byte-identical
- [x] add `ProxySocketHost` and `ProxySocketContainer` to `Options` with godoc
- [x] add `NetworkMode string` and `Env []string` to `Spec`
- [x] in `BuildSpec`: when `ProxySocketHost != ""`, set `NetworkMode = "none"`,
      append `HTTP_PROXY`/`HTTPS_PROXY` env (value
      `unix://<ProxySocketContainer>`), and append the
      `Mount{Host, Container, ReadOnly:true}` socket mount after the existing
      mount groups (independent of the overlay ordering)
- [x] in `Args()`: emit `--network <mode>` and `-e <kv>` (each) when set
- [x] in `ShellCommand()`: mirror `--network` and `-e` in the line-oriented
      output (add to the recognized paired-flag set)
- [x] write tests: configured ⇒ argv contains `--network none`, both env vars,
      and the read-only socket mount (`,readonly`) in the documented position
- [x] write tests: unconfigured (empty fields) ⇒ argv **identical** to today
      (no `--network`, no `-e`, no socket mount) — purity/backwards-compat
- [x] write tests: existing non-readonly mounts render byte-identically (the
      `ReadOnly:false` default adds nothing)
- [x] write tests: `ShellCommand()` renders the new flags correctly
- [x] run tests — must pass before Task 4

### Task 4: Wire the proxy lifecycle into runGo

**Files:**
- Modify: `cmd/makeslop/main.go`
- Modify: `cmd/makeslop/main_test.go`

- [ ] consume the `Network` return from `projectconfig.Load`
- [ ] when `ProxyAddress != ""`: compute the socket path as
      `/tmp/makeslop-<hash12>-<pid>.sock` where `hash12` is the first 12 hex of
      `sha256(workspaceDir)` (bounded length — do **not** use the workspace
      basename), construct `networks.NewProxy`, set both `Options` fields
      (`ProxySocketContainer = "/tmp/makeslop-proxy.sock"`)
- [ ] order: `BuildSpec` → dry-run check (print + return, **no** Start, no
      socket) → `proxy.Start` → `defer proxy.Close()` → `docker.Run`
- [ ] surface `proxy.Start` errors as launch-aborting (return the error, no
      container) — symmetric with Scan/YAML abort
- [ ] write tests: dry-run with a configured `network:` section prints argv
      containing `--network none` + env + read-only socket mount, and does
      **not** create a socket file on disk
- [ ] write tests: dry-run without the section prints today's argv unchanged
- [ ] write tests: generated socket path length ≤ 108 bytes even when the
      project dir basename is very long (guards the `sockaddr_un` limit)
- [ ] run tests — must pass before Task 5

### Task 5: Verify acceptance criteria
- [x] verify all Overview requirements are implemented (opt-in gate, socket
      lifecycle, env vars, `--network none`, teardown), **within the documented
      `unix://` client-support scope**
- [x] verify edge cases: absent section, malformed address, concurrent runs use
      distinct sockets (different PIDs), dry-run does not bind a socket, long
      project name keeps socket path ≤ 108 bytes
- [x] verify teardown: no leaked goroutines/FDs after a tunnel closes; `Close`
      does not hang on an in-flight connection
- [x] run full suite: `mkdir -p .gocache && GOTMPDIR=$(pwd)/.gocache go test ./...`
- [x] run `go vet ./...`
- [x] confirm no third-party deps were added (`git diff go.mod go.sum` empty)

### Task 6: [Final] Update documentation
- [x] update CLAUDE.md: add `internal/networks/` to the package layout; add the
      proxy invariants (opt-in gate, `BuildSpec` purity preserved, dry-run does
      not bind socket, `Start` failure aborts launch, bounded-hash socket path
      scheme); extend the `Mount.Type` invariant note to document the new
      `Mount.ReadOnly` field
- [x] update README (if present) with the `network.proxy.address` example and
      the `unix://` client-support caveat
- [x] move this plan to `docs/plans/completed/`

## Post-Completion
*Items requiring manual intervention or external systems — informational only.*

**Manual verification**:
- End-to-end test against a real tinyproxy: configure `network.proxy.address`,
  run `makeslop go`, confirm a tool inside the container can reach an allowed
  HTTPS endpoint and that **no** direct egress works (`--network none` holds).
- Confirm two concurrent `makeslop go` runs of the same project use distinct
  sockets and do not interfere.

**Client-support caveat** (now scoped in Overview; restate in README):
- `HTTPS_PROXY=unix://...` is **not** honored by all HTTP clients (recent `curl`
  and Node `undici` yes; Python `requests`/`urllib3` and Go `net/http` no). With
  `--network none`, unsupported clients get no network rather than a fallback.
  This is a deliberate scope limitation stated in the Overview and acceptance
  criteria — the README must repeat it prominently so users aren't surprised.
