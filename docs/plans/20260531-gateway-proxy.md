# Socket-by-default Gateway Proxy with Optional Request Logging

## Overview
Rework `internal/networks/proxy.go` so the unix-socket forward proxy becomes the **default** egress path for `makeslop run`, add a `--no-proxy` escape hatch, and add optional file-based request logging.

Today the socket is only wired when `network.proxy.address` is set in `.makeslop.yaml`; otherwise the container gets plain docker bridge networking. This change unifies on the socket as the single egress mechanism so toggling a corporate proxy is just an address change, and locks the container to `--network none` by default.

**Three-state network model:**

| State | Trigger | Behavior |
|---|---|---|
| **Gateway (new default)** | no `network.proxy.address`, no `--no-proxy` | `--network none` + unix socket; goroutine is an HTTP(S) forward proxy that **dials targets directly** (no upstream) |
| **Upstream** | `network.proxy.address` set | **unchanged** — dumb byte-pipe splice to upstream + existing probe-dial fail-loud invariant |
| **Off** | `--no-proxy` flag | skip the socket ⇒ docker bridge networking (escape hatch for non-HTTP traffic) |

Key benefit: uniform egress mechanism; the container has no network interface by default — the only way out is the host-controlled socket.

## Context (from discovery)
- **Files/components involved:**
  - `internal/networks/proxy.go` (~220 lines) — the `Proxy` type + lifecycle (`Start`, `Close`, `acceptLoop`, `handle`, `halfCloseWrite`, conn-tracking).
  - `internal/networks/proxy_test.go` — existing tests with a fake-upstream helper.
  - `internal/projectconfig/projectconfig.go` — `yamlSchema`, `Network` struct, `Stub`, `Load`, `validateEntries`. `network.proxy.address` parsing at ~line 206.
  - `cmd/makeslop/main.go` — `runRun` proxy wiring at ~lines 141–201; flag registration in `newRootCmd` at ~line 230 (`--out-of-home`/`--quiet` are the patterns to mirror).
  - `internal/docker/spec.go` — emits `--network none` + socket + `HTTP_PROXY/HTTPS_PROXY` when `ProxySocketHost != ""` (~line 128). **Leave unchanged.**
  - `cmd/makeslop/status.go` — proxy health-check (check #6, non-blocking) at ~lines 313–325; currently shows `–`/address.
  - `CLAUDE.md` — "Proxy probe-dial invariant" section.
- **Related patterns found:**
  - Pure/impure split: `spec.go` is pure and table-tested; side effects live in `run.go`/`build.go`/`networks`.
  - Fail-loud ethos: bad probe-dial aborts launch; secret-scan walk errors abort `runRun`.
  - Run-only flags registered on the `run` command, not persistent root flags (`--out-of-home`).
  - Table-driven tests; `SkipNonPOSIX` guards TTY/socket tests.
- **Dependencies identified:** std-lib only for the gateway (`net/http`, `bufio`, `log`). No new modules. `spec.go` already provides the `--network none` + socket plumbing.

## Development Approach
- **testing approach:** Regular (code first, then tests within the same task) — matches the repo's table-driven style.
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** — success + error scenarios
- **CRITICAL: all tests must pass before starting the next task**
- run `go test ./...` after each change
- maintain backward compatibility for the **upstream** path: it must be byte-for-byte unchanged when logging is off

## Testing Strategy
- **unit tests:** required for every task (see Development Approach).
- **e2e tests:** none — this is a CLI/library project with no UI e2e harness. The gated Docker integration test (`MAKESLOP_DOCKER_IT=1 go test -tags integration ./internal/docker/`) is unaffected (spec.go unchanged) and is **not** part of the per-task gate.
- **POSIX-only:** keep `SkipNonPOSIX` at the top of tests exercising unix-socket/TTY behavior.

## Progress Tracking
- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope

## Solution Overview
**Option A — branch the serve strategy inside one renamed type.** The shared socket lifecycle (stale-socket removal, `Umask(0)`/`Listen`/`chmod 0666`, conn-tracking, `Close`, `wg`) stays in one place; `Start` branches only on mode:
- **upstream** (`proxy != ""`): unchanged — probe-dial then `acceptLoop` + `handle` (dumb splice).
- **gateway** (`proxy == ""`): no probe-dial; serve the listener with an `http.Server{Handler: g}` whose `ServeHTTP` does CONNECT-hijack→direct-dial→splice and absolute-form plain-HTTP forwarding.

Logging is a file-backed `*log.Logger` owned by the type, written from both modes. The upstream log path is **gated** so when logging is off the upstream code path is identical to today.

Rejected: a single unified forward-proxy handler with a pluggable dialer — it would re-encode CONNECT to the upstream rather than splicing verbatim, changing the upstream behavior we agreed to keep.

## Technical Details
- **Type rename:** `Proxy` → `Gateway`; `NewProxy(socketPath, upstream)` → `NewGateway(socketPath, proxy, logPath string)`. Package stays `networks`. `proxy` empty ⇒ direct-dial gateway; non-empty ⇒ forward to that upstream. `logPath` empty ⇒ logging disabled.
- **New fields:** `proxy string`, `logPath string`, `srv *http.Server` (gateway mode only; nil in upstream), `logFile *os.File`, `logger *log.Logger`, `transport *http.Transport` (shared, gateway mode).
- **Config:** `yamlSchema.Network` gains `Log string` (`yaml:"log"`); `Network` gains `LogPath string` (absolute, resolved + validated under-root, **no stat-drop** since it is an output file). `Stub` gains `log: ""` under `network:`. No `MigrationVersion` bump.
- **Log line format (both modes):** `<METHOD> <target>` e.g. `CONNECT api.example.com:443`, `GET http://host/p`.
- **Documented limitation:** plain-HTTP keep-alive logs only the FIRST request line per connection (upstream peek); `CONNECT` (HTTPS, dominant case) is exact.

## What Goes Where
- **Implementation Steps** (`[ ]`): all code, tests, and doc updates in this repo.
- **Post-Completion** (no checkboxes): manual smoke test of a live `makeslop run` against a real Docker daemon (gateway egress, upstream, `--no-proxy`), which requires an interactive TTY + daemon and cannot run in `go test`.

## Implementation Steps

### Task 1: Rename `Proxy` → `Gateway` (pure rename, no behavior change)

**Files:**
- Modify: `internal/networks/proxy.go`
- Modify: `internal/networks/proxy_test.go`
- Modify: `cmd/makeslop/main.go`

- [x] rename type `Proxy` → `Gateway` and method receivers `p *Proxy` → `g *Gateway` in `internal/networks/proxy.go`
- [x] rename constructor `NewProxy` → `NewGateway` (keep the current 2-arg signature for now: `(socketPath, upstream string)`); rename the `upstream` field to `proxy`
- [x] update all references in `cmd/makeslop/main.go` (`var proxy *networks.Proxy` → `var gw *networks.Gateway`, `networks.NewProxy(...)` → `networks.NewGateway(...)`)
- [x] update all `Proxy`/`NewProxy` references in `internal/networks/proxy_test.go`
- [x] run `go build ./... && go test ./...` — must pass before task 2 (pure rename, all existing tests green)

### Task 2: Extract `splice` helper from `handle`

**Files:**
- Modify: `internal/networks/proxy.go`
- Modify: `internal/networks/proxy_test.go`

- [ ] extract the bidirectional copy in `handle` (the two `io.Copy` + `halfCloseWrite` goroutines + inner `wg.Wait`) into `func (g *Gateway) splice(a, b net.Conn)`
- [ ] rewrite `handle` to dial the upstream, track/untrack `up`, then call `g.splice(client, up)`
- [ ] keep `halfCloseWrite` as-is (used by `splice`)
- [ ] add/adjust a unit test asserting `splice` copies bytes both directions and half-closes on EOF (use `net.Pipe`-backed or local TCP conns)
- [ ] run `go test ./...` — must pass before task 3

### Task 3: Add gateway mode — `NewGateway(…, proxy, logPath)`, `Start`/`Close` branch, `ServeHTTP`

**Files:**
- Modify: `internal/networks/proxy.go`
- Modify: `internal/networks/proxy_test.go`

- [ ] change `NewGateway` to `(socketPath, proxy, logPath string)`; store `proxy`, `logPath`; init `g.transport = &http.Transport{}` (used by plain-HTTP path)
- [ ] in `Start`, keep the shared prologue VERBATIM (stale-socket removal, `Umask(0)`/`net.Listen`/restore, `chmod 0666`); then branch:
  - `proxy != ""`: unchanged — probe-dial (existing 5s fail-loud) then spawn `acceptLoop` in the tracked `wg` goroutine
  - `proxy == ""`: NO probe-dial; build `g.srv = &http.Server{Handler: g, BaseContext: func(net.Listener) context.Context { return proxyCtx }}` and spawn `g.srv.Serve(ln)` in the tracked `wg` goroutine
- [ ] in `Close`, **pin the teardown order**: `cancel()` → (if `g.srv != nil`) `g.srv.Close()` → close tracked conns → `wg.Wait()` → `os.Remove(socket)`. `g.srv.Close()` closing `ln` and the existing `ln.Close()` is a harmless double-close (`ErrClosed`, already `_`-ignored). Confirm `srv.Serve` returning `ErrServerClosed` lets the tracked `wg` goroutine exit so `wg.Wait()` does not hang. Hijacked CONNECT conns are detached from `srv`, so the existing `trackConn` loop is what tears them down — keep it
- [ ] **define a minimal nil-safe `func (g *Gateway) logReq(method, target string)` stub in THIS task** (no-op when `g.logger == nil`) so the build/`go test` gate stays green; Task 4 only adds the logger plumbing + upstream gating
- [ ] implement `func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request)`: CONNECT → direct `DialContext` to `r.Host`, track `dst`, `Hijack()` client, track client, write `HTTP/1.1 200 Connection Established\r\n\r\n`, `g.logReq("CONNECT", r.Host)`, `g.splice(client, dst)`; else (absolute-form HTTP) → strip `Proxy-Connection` from the request, `r.RequestURI = ""`, `g.transport.RoundTrip(r)`, on error `http.Error(w, …, http.StatusBadGateway)` and return, else copy response headers **dropping the hop-by-hop set** (`Connection`, `Keep-Alive`, `Proxy-Authenticate`, `Proxy-Authorization`, `TE`, `Trailer`, `Transfer-Encoding`, `Upgrade`, plus any header named in `Connection`), `w.WriteHeader(resp.StatusCode)`, `io.Copy(w, resp.Body)`; call `g.logReq(r.Method, r.URL.String())`
- [ ] update the package/file doc comment: "forwards verbatim without HTTP parsing" now applies to **upstream mode only**; gateway mode parses HTTP
- [ ] write gateway unit tests (`SkipNonPOSIX`): `NewGateway(sock, "", "")` → connect a client over the unix socket, send a `CONNECT host:port` to a local fake TCP target, assert bytes round-trip; send an absolute-form `GET` to a local fake `httptest` server, assert the response body returns
- [ ] write a `Close` test asserting an in-flight gateway tunnel is torn down (tracked conns closed) and the socket file is removed
- [ ] run `go test ./...` — must pass before task 4

### Task 4: Add request logging (file-backed, both modes, gated upstream)

**Files:**
- Modify: `internal/networks/proxy.go`
- Modify: `internal/networks/proxy_test.go`

- [ ] add fields `logFile *os.File`, `logger *log.Logger`; in `Start`, after binding the socket and BEFORE the mode branch: if `g.logPath != ""` open `O_CREATE|O_APPEND|O_WRONLY` (0644) and build `log.New(file, "", log.LstdFlags)`; **fail loud** — on open error, close+remove the listener/socket and return the error (abort launch)
- [ ] in `Close`, close `g.logFile` if open
- [ ] add `func (g *Gateway) logReq(method, target string)` — no-op when `g.logger == nil`, else `g.logger.Printf("%s %s", method, target)`; wire the two call sites in `ServeHTTP`
- [ ] add `func (g *Gateway) logFirstLine(line string)` — `strings.Fields` → `logReq(method, target)`; log raw trimmed line if malformed (<2 fields)
- [ ] gate the upstream path in `handle`/`splice`: when `g.logger == nil` use today's exact `go io.Copy(up, client)` client→up direction; when `g.logger != nil` use `bufio.NewReader(client)`, `ReadString('\n')`, `g.logFirstLine(line)`, `up.Write([]byte(line))`, then `go io.Copy(up, br)`. (The up→client direction is unchanged.)
- [ ] add a doc comment noting the keep-alive limitation (only first request line logged on plain-HTTP keep-alive; CONNECT is exact)
- [ ] write gateway logging tests: with a temp `logPath`, assert a line lands in the file for both CONNECT and plain GET
- [ ] write upstream logging tests: with logging ON, assert the request line is BOTH logged AND forwarded verbatim to the fake upstream; with logging OFF, assert no parse occurs (bytes forwarded unchanged, file absent)
- [ ] write a fail-loud test: unopenable `logPath` (e.g. path under a non-existent dir, or a dir as the path) → `Start` returns error and removes the socket
- [ ] run `go test ./...` — must pass before task 5

### Task 5: Project config — `network.log` parsing + validation + stub

**Files:**
- Modify: `internal/projectconfig/projectconfig.go`
- Modify: `internal/projectconfig/projectconfig_test.go`

- [ ] add `Log string \`yaml:"log"\`` to `yamlSchema.Network`
- [ ] add `LogPath string` to the `Network` struct (doc: absolute, under-root, "" when unset)
- [ ] in `Load`, when `schema.Network.Log != ""`: validate with the `validateEntries` rules (non-empty, not absolute, `filepath.IsLocal`, not `.`, not a reserved agent path) — but do NOT stat-drop; resolve to `filepath.Join(root, filepath.Clean(log))` and set `netCfg.LogPath`. Wrap errors with `projectconfig: invalid network.log …`
- [ ] add `log: ""` under `network:` in `Stub`, as a sibling of `proxy:` (placement matters for strict decode)
- [ ] update the exported-`Stub`-bytes comparison test (it hardcodes the stub) — **definite, not conditional**
- [ ] add an explicit "Scaffold then Load the stub → no strict-decode error AND `LogPath == ""`" round-trip assertion
- [ ] write table-driven tests: valid relative path → resolved absolute `LogPath`; absolute path → error; `../escape` → error; reserved path (e.g. `.claude`) → error; empty/absent → `LogPath == ""`; missing-but-valid path (no stat-drop) → kept
- [ ] verify strict decode: an existing file lacking `log:` still loads (`LogPath == ""`)
- [ ] run `go test ./...` — must pass before task 6

### Task 6: Wire `runRun` — socket-by-default + `--no-proxy` flag + logPath

**Files:**
- Modify: `cmd/makeslop/main.go`
- Modify: `cmd/makeslop/main_test.go`

- [ ] add a package-level seam `newGatewayFn = networks.NewGateway` in `main.go` (mirrors the existing `docker.newClientFn` pattern) and construct via `newGatewayFn(...)` so tests can capture the `(sockPath, proxy, logPath)` args — `logPath` is a `NewGateway` arg, NOT part of `spec`, so it is invisible to `--dry-run`/`ShellCommand()`; the seam is the only way to assert it
- [ ] add a `noProxy bool` variable (`outOfHomeRun`-style local) and register `--no-proxy` on the `run` command ONLY via `runCmd.Flags().BoolVar(...)` (matches the run-only `--out-of-home` at main.go:343; NOT a persistent root flag); thread it into `runRun`'s signature
- [ ] replace the `if netCfg.ProxyAddress != "" { … }` block (~line 155): UNLESS `noProxy`, ALWAYS compute `sockPath` (existing sha256+pid scheme), set `opts.ProxySocketHost`/`opts.ProxySocketContainer`, and `gw = newGatewayFn(sockPath, netCfg.ProxyAddress, netCfg.LogPath)`. When `noProxy`: leave `ProxySocketHost` empty (so `spec.go` keeps bridge networking) and `gw` nil
- [ ] keep `gw.Start(cmd.Context())` / `defer gw.Close()` guarded by `gw != nil`, in the same place (after image pre-flight)
- [ ] confirm `--no-proxy` is rejected as unknown by `version`/`config`/`migrate`/`build`/`status` (run-only registration ensures this; add an assertion test, mirroring any existing unknown-flag test)
- [ ] write `main_test.go` cases (swap `newGatewayFn` to capture args, like the existing `docker.newClientFn`/`ttyCheck` swaps at main_test.go:25): default (no address, no flag) → spec has `ProxySocketHost` set AND `NetworkMode == "none"`, captured `proxy == ""`; `--no-proxy` → no `ProxySocketHost`, bridge networking (empty `NetworkMode`), `gw` not constructed; address set → captured `proxy == <addr>`; `network.log` set → captured `logPath == <resolved abs path>`
- [ ] run `go test ./...` — must pass before task 7

### Task 7: Update `status` proxy line

**Files:**
- Modify: `cmd/makeslop/status.go`
- Modify: `cmd/makeslop/status_test.go`

- [ ] in check #6, when `netCfg.ProxyAddress == ""` set proxy `Detail` to `gateway (direct egress)` (state stays `checkInfo`); when set, keep showing the address
- [ ] append ` (logging → <LogPath>)` to the proxy `Detail` when `netCfg.LogPath != ""` (both gateway and upstream cases)
- [ ] note in a comment that `status` is config-derived and has no `--no-proxy` knowledge
- [ ] update `status_test.go`: assert `gateway (direct egress)` for no-address; address still shown; logging suffix present when `network.log` set; JSON `detail` matches
- [ ] run `go test ./...` — must pass before task 8

### Task 8: Verify acceptance criteria
- [ ] verify the three-state model: gateway default (socket + `--network none`), upstream unchanged, `--no-proxy` → bridge
- [ ] verify logging works in both modes and is gated off cleanly (upstream byte-for-byte unchanged when off)
- [ ] verify fail-loud: bad `network.log` path aborts `Start`; upstream probe-dial fail-loud preserved
- [ ] verify `spec.go` is unchanged (drift-guard test still green) and `MigrationVersion` not bumped
- [ ] run full suite: `go test ./...`
- [ ] run linter if configured (e.g. `golangci-lint run`) — fix new warnings
- [ ] confirm gated integration test still compiles: `go test -tags integration ./internal/docker/` (skips without daemon)

### Task 9: [Final] Update documentation
- [ ] update `CLAUDE.md` "Proxy probe-dial invariant" section: probe-dial is **upstream-mode-only** now; document the gateway default, `--no-proxy` → bridge, and request logging (`network.log`, both modes, keep-alive limitation, fail-loud on unopenable path)
- [ ] update the `--out-of-home flag scope` and `TTY requirement`/home-guard notes only if affected (they are not — `--no-proxy` is `run`-only like `--out-of-home`); add a one-line note that `--no-proxy` is `run`-only and rejected elsewhere
- [ ] update the `internal/projectconfig` doc/comment for the new `network.log` field
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion
*Items requiring manual intervention or external systems — no checkboxes, informational only*

**Manual verification** (requires interactive TTY + live Docker daemon, cannot run in `go test`):
- `makeslop run` with no `network.proxy.address`: confirm container reaches the internet via the gateway (e.g. `curl https://example.com` inside the container) and that `--network none` is in effect (no direct bridge).
- `makeslop run --no-proxy`: confirm bridge networking restores non-HTTP egress (e.g. `git clone git@github.com:…` / raw TCP).
- `makeslop run` with `network.proxy.address` set: confirm upstream behavior is unchanged and probe-dial still aborts on a dead upstream.
- With `network.log: makeslop-requests.log` set: confirm request lines are written for both gateway and upstream modes; confirm an unwritable path aborts the launch loudly.
