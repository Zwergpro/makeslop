// Package networks owns the host-side HTTP forward proxy that bridges container
// traffic (via the socat-volume unix socket) to either an upstream HTTP CONNECT
// proxy (upstream mode) or directly to the internet (gateway mode).
//
// The host Gateway listens on TCP 127.0.0.1:<ephemeral port>. A socat sidecar
// container (see internal/docker/sidecar.go) re-exposes the TCP endpoint as a
// unix socket on a Docker volume so the app container can reach it via
// HTTP_PROXY=unix:///sockets/proxy.sock while staying on --network none.
//
// # Modes
//
// Gateway mode (proxy == ""): the default. Parses HTTP/HTTPS CONNECT requests
// and forwards connections directly. Plain absolute-form HTTP is forwarded via
// http.Transport.RoundTrip. The container is locked to --network none.
//
// Upstream mode (proxy != ""): dumb byte-pipe splice to the upstream. The
// gateway forwards verbatim without additional HTTP parsing — the payload is
// forwarded as-is to the upstream proxy. The upstream-mode data path is:
//
//	Container app ──volume unix socket──► socat ──TCP──► Gateway ──TCP──► upstream proxy ──► internet
//
// # Half-close protocol
//
// When one copy direction finishes, halfCloseWrite signals EOF to the peer so
// the opposite io.Copy returns rather than blocking wg.Wait.
package networks

import (
	"bufio"
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// shutdownTimeout is the maximum time srv.Shutdown waits for in-flight
// ServeHTTP goroutines to drain before force-closing connections. 3 seconds
// is long enough to let legitimate handlers finish a round-trip to a local
// origin (httptest servers in tests, nearby upstreams in production) while
// being short enough not to block container teardown noticeably.
const shutdownTimeout = 3 * time.Second

// hopByHopHeaders is the set of headers that must not be forwarded when
// proxying plain-HTTP requests in gateway mode. Per RFC 7230 §6.1, any header
// named in the Connection header value is also hop-by-hop.
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"TE",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// Gateway listens on TCP 127.0.0.1:<ephemeral port> and either:
//   - (gateway mode, proxy == "") acts as a direct HTTP(S) forward proxy; or
//   - (upstream mode, proxy != "") tunnels all traffic to an upstream TCP proxy.
//
// Call Start/Close to manage the lifecycle; Close is idempotent.
//
// Logging limitation: plain-HTTP keep-alive connections log only the FIRST
// request line per connection in upstream mode (because the upstream peek reads
// one line). CONNECT (HTTPS, the dominant case) is exact. Gateway-mode plain
// HTTP is logged per-request by ServeHTTP.
type Gateway struct {
	proxy   string
	logPath string

	// gateway-mode only; nil in upstream mode
	srv       *http.Server
	transport *http.Transport

	// logging; both fields are nil when logPath == "" or Start has not run
	logFile *os.File
	logger  *log.Logger

	mu       sync.Mutex
	listener net.Listener
	conns    []net.Conn // all in-flight connections (client + upstream halves)
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// NewGateway constructs a Gateway; none of the arguments are validated —
// Start surfaces bind/dial errors.
//
// proxy: upstream address (host:port). Empty string → gateway mode (direct egress).
// logPath: path for the request log file. Empty string → logging disabled.
func NewGateway(proxy, logPath string) *Gateway {
	g := &Gateway{
		proxy:   proxy,
		logPath: logPath,
	}
	if proxy == "" {
		// Gateway mode: allocate a shared transport for plain-HTTP forwarding.
		g.transport = &http.Transport{}
	}
	return g
}

// Addr returns the TCP address the Gateway is listening on after Start returns
// successfully, or nil if Start has not been called.
func (g *Gateway) Addr() net.Addr {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.listener == nil {
		return nil
	}
	return g.listener.Addr()
}

// Port returns the TCP port the Gateway is listening on after Start returns
// successfully, or 0 if Start has not been called.
func (g *Gateway) Port() int {
	addr := g.Addr()
	if addr == nil {
		return 0
	}
	if ta, ok := addr.(*net.TCPAddr); ok {
		return ta.Port
	}
	return 0
}

// Start binds a TCP listener on 127.0.0.1:0 (OS assigns the port) and starts
// serving. ctx cancellation alone does not stop the loop; only Close unblocks
// it. ctx is propagated to in-flight connection handlers.
//
// A bind error should abort the container launch.
//
// In upstream mode Start performs a single probe dial of the upstream address
// before accepting any connections. If the upstream is unreachable, Start tears
// down the listener and returns the error so the container launch aborts loudly
// rather than silently black-holing traffic. The probe has a 5-second timeout;
// it checks TCP reachability only.
//
// In gateway mode (proxy == "") no probe dial is performed; the listener is
// handed to an http.Server whose ServeHTTP handles CONNECT and absolute-form HTTP.
func (g *Gateway) Start(ctx context.Context) error {
	// ── shared prologue ───────────────────────────────────────────────────────
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}

	// ── optional request logging ──────────────────────────────────────────────
	// Open the log file before branching into the mode-specific path so that a
	// bad logPath aborts launch loudly rather than silently skipping logging.
	if g.logPath != "" {
		f, err := os.OpenFile(g.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			_ = ln.Close()
			return err
		}
		g.logFile = f
		g.logger = log.New(f, "", log.LstdFlags)
	}

	proxyCtx, cancel := context.WithCancel(ctx)

	g.mu.Lock()
	g.listener = ln
	g.cancel = cancel
	g.mu.Unlock()

	if g.proxy != "" {
		// ── upstream mode ─────────────────────────────────────────────────────
		// Probe-dial the upstream to catch misconfiguration before any container
		// traffic is routed through the socket. A short timeout avoids blocking
		// the launch for a prolonged period on a bad address.
		probeCtx, probeCancel := context.WithTimeout(ctx, 5*time.Second)
		probe, err := (&net.Dialer{}).DialContext(probeCtx, "tcp", g.proxy)
		probeCancel()
		if err != nil {
			g.mu.Lock()
			g.cancel = nil
			g.listener = nil
			g.mu.Unlock()
			cancel()
			_ = ln.Close()
			// Close the log file opened above so the fd is not leaked on this
			// error path. The caller sees an error and will not call Close().
			if g.logFile != nil {
				_ = g.logFile.Close()
				g.logFile = nil
				g.logger = nil
			}
			return err
		}
		_ = probe.Close()

		g.wg.Add(1)
		go func() {
			defer g.wg.Done()
			g.acceptLoop(proxyCtx, ln)
		}()
	} else {
		// ── gateway mode ──────────────────────────────────────────────────────
		// No probe-dial: the gateway dials targets on demand.
		g.srv = &http.Server{
			Handler: g,
			BaseContext: func(net.Listener) context.Context {
				return proxyCtx
			},
		}

		// Local copy of srv prevents a data race: Close() sets g.srv=nil under
		// the mutex, and this goroutine must not read g.srv after that.
		srv := g.srv
		g.wg.Add(1)
		go func() {
			defer g.wg.Done()
			// ErrServerClosed is the normal exit when srv.Shutdown() is called.
			srv.Serve(ln) //nolint:errcheck // ErrServerClosed is expected
		}()
	}

	return nil
}

// ServeHTTP implements http.Handler for gateway mode (proxy == "").
//
// CONNECT requests: dial the target directly, hijack the client connection,
// respond 200, then splice bytes bidirectionally.
//
// Absolute-form requests (plain HTTP): strip hop-by-hop headers, round-trip
// via g.transport, copy response back (dropping hop-by-hop headers).
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		// CONNECT: set up a direct tunnel to r.Host.
		dst, err := (&net.Dialer{}).DialContext(r.Context(), "tcp", r.Host)
		if err != nil {
			http.Error(w, "upstream unreachable: "+err.Error(), http.StatusBadGateway)
			return
		}
		g.trackConn(dst)
		defer func() {
			g.untrackConn(dst)
			dst.Close()
		}()

		// Hijack the client connection.
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "server does not support hijacking", http.StatusInternalServerError)
			return
		}

		// Add to the gateway WaitGroup before calling Hijack. Once Hijack
		// succeeds, the http.Server no longer tracks this connection in its
		// own shutdown wait — srv.Shutdown() will NOT wait for this goroutine
		// to finish. By incrementing g.wg here (while still inside
		// ServeHTTP, which srv does track), and decrementing it on return,
		// Close()'s wg.Wait() is guaranteed to drain this goroutine before
		// logFile.Close() runs, preventing a write to a closed file descriptor.
		g.wg.Add(1)
		defer g.wg.Done()

		clientConn, _, err := hj.Hijack()
		if err != nil {
			// After a failed Hijack the connection is in an unusable state;
			// calling http.Error would silently fail. Just return — the
			// http.Server will close the underlying connection on its own.
			return
		}
		g.trackConn(clientConn)
		defer func() {
			g.untrackConn(clientConn)
			clientConn.Close()
		}()

		// Log before confirming the tunnel so the entry is durable before
		// the client could race to trigger g.Close().
		g.logReq("CONNECT", r.Host)
		// Inform the client the tunnel is established.
		_, _ = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		g.splice(clientConn, dst)
		return
	}

	// Absolute-form plain-HTTP forwarding.
	// Restore scheme and host: Go's HTTP server strips them from the r.URL
	// when parsing incoming requests, but RoundTrip requires both.
	if r.URL.Scheme == "" {
		r.URL.Scheme = "http"
	}
	if r.URL.Host == "" {
		r.URL.Host = r.Host
	}
	// Strip Proxy-Connection (non-standard but common).
	r.Header.Del("Proxy-Connection")
	// Remove hop-by-hop headers named in Connection.
	for _, h := range strings.Split(r.Header.Get("Connection"), ",") {
		h = strings.TrimSpace(h)
		if h != "" {
			r.Header.Del(h)
		}
	}
	// Remove well-known hop-by-hop headers.
	for _, h := range hopByHopHeaders {
		r.Header.Del(h)
	}
	// RoundTrip requires a nil RequestURI.
	r.RequestURI = ""

	resp, err := g.transport.RoundTrip(r)
	if err != nil {
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy response headers, dropping hop-by-hop set.
	connHeader := resp.Header.Get("Connection")
	for _, h := range strings.Split(connHeader, ",") {
		h = strings.TrimSpace(h)
		if h != "" {
			resp.Header.Del(h)
		}
	}
	for _, h := range hopByHopHeaders {
		resp.Header.Del(h)
	}
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	// Log before writing the response to avoid a race where the client could
	// trigger g.Close() (closing the log file) before this goroutine logs.
	g.logReq(r.Method, r.URL.String())
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body) //nolint:errcheck // client disconnect is expected
}

// logReq logs a single request line in the format "<METHOD> <target>".
// It is a no-op when g.logger is nil (logging disabled).
func (g *Gateway) logReq(method, target string) {
	if g.logger == nil {
		return
	}
	g.logger.Printf("%s %s", method, target)
}

// logFirstLine parses the first line of a raw HTTP request (e.g. "GET http://host/p HTTP/1.1")
// and delegates to logReq. If the line is malformed (fewer than 2 fields) the
// raw trimmed line is logged as-is.
//
// Limitation (upstream mode): plain-HTTP keep-alive connections log only the
// FIRST request line per connection because the upstream peek reads exactly one
// line. CONNECT (HTTPS) is exact.
//
// Precondition: g.logger != nil (callers guard this before calling).
func (g *Gateway) logFirstLine(line string) {
	fields := strings.Fields(line)
	if len(fields) >= 2 {
		g.logReq(fields[0], fields[1])
		return
	}
	// Malformed line — log raw to aid debugging.
	// Caller guards g.logger != nil before invoking logFirstLine.
	g.logger.Printf("%s", strings.TrimSpace(line))
}

// acceptLoop accepts connections from ln until it is closed or ctx is done.
// Used by upstream mode only.
func (g *Gateway) acceptLoop(ctx context.Context, ln net.Listener) {
	for {
		client, err := ln.Accept()
		if err != nil {
			// Listener closed by Close() — stop.
			return
		}
		g.trackConn(client)
		g.wg.Add(1)
		go func() {
			defer g.wg.Done()
			defer g.untrackConn(client)
			g.handle(ctx, client)
		}()
	}
}

// handle dials the upstream, tracks the connection, then splices bytes between
// client and upstream. Used by upstream mode only.
//
// When logging is enabled (g.logger != nil), the first request line from the
// client is peeked and logged before being forwarded verbatim to the upstream.
// When logging is disabled the path is identical to a plain io.Copy — no parse,
// no overhead.
func (g *Gateway) handle(ctx context.Context, client net.Conn) {
	defer client.Close()

	up, err := (&net.Dialer{}).DialContext(ctx, "tcp", g.proxy)
	if err != nil {
		return
	}
	defer up.Close()
	g.trackConn(up)
	defer g.untrackConn(up)

	if g.logger != nil {
		// Peek the first request line, log it, then forward it followed by the
		// remaining bytes from the buffered reader.
		br := bufio.NewReader(client)
		// ReadString error is intentionally discarded: even a partial line (e.g.
		// a short write, or a pipelined request without a trailing '\n') still
		// contains the method and target we want to log, and the raw bytes must
		// be forwarded verbatim to the upstream regardless of parse quality.
		line, _ := br.ReadString('\n')
		g.logFirstLine(line)
		// Write the peeked line back to upstream, then splice remaining bytes.
		_, _ = up.Write([]byte(line))

		// Replace the client with a conn-like reader that re-injects the buffered
		// remainder (anything after the first line that bufio already read).
		// We do this by splicing (br, client) → up and up → client using a
		// custom splice that reads from br first.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			io.Copy(up, br) //nolint:errcheck // reads br (buffered) then EOF
			halfCloseWrite(up)
		}()
		go func() {
			defer wg.Done()
			io.Copy(client, up) //nolint:errcheck
			halfCloseWrite(client)
		}()
		wg.Wait()
		return
	}

	g.splice(client, up)
}

// splice bidirectionally copies bytes between a and b. When one copy direction
// finishes (EOF or error), halfCloseWrite signals EOF to the peer so the
// opposite direction drains rather than blocking.
func (g *Gateway) splice(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(b, a) //nolint:errcheck // EOF / reset are expected
		halfCloseWrite(b)
	}()
	go func() {
		defer wg.Done()
		io.Copy(a, b) //nolint:errcheck // EOF / reset are expected
		halfCloseWrite(a)
	}()
	wg.Wait()
}

// halfCloseWrite signals EOF to the peer by closing the write half of c;
// types without CloseWrite are silently ignored.
func halfCloseWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

func (g *Gateway) trackConn(c net.Conn) {
	g.mu.Lock()
	g.conns = append(g.conns, c)
	g.mu.Unlock()
}

func (g *Gateway) untrackConn(c net.Conn) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, tracked := range g.conns {
		if tracked == c {
			g.conns[i] = g.conns[len(g.conns)-1]
			g.conns = g.conns[:len(g.conns)-1]
			return
		}
	}
}

// Close is idempotent: safe to call before Start or after a failed Start.
//
// Teardown order:
//  1. cancel() — signals the proxyCtx; stops new work.
//  2. g.srv.Shutdown() (gateway mode) — drains in-flight ServeHTTP goroutines
//     before closing the listener so that plain-HTTP handlers cannot write to
//     g.logFile after logFile.Close() runs. Hijacked CONNECT conns are detached
//     from srv after Hijack() returns, but they call g.wg.Add(1) before Hijack(),
//     so wg.Wait() (step 4) still catches them.
//     The existing ln.Close() below is a harmless double-close (ErrClosed, already _-ignored).
//  3. Close all tracked conns — unblocks any in-flight splice goroutines.
//  4. wg.Wait() — drain all goroutines.
//  5. Close g.logFile — safe because:
//     - gateway mode: srv.Shutdown() (step 2) waits for all non-hijacked plain-HTTP
//       ServeHTTP goroutines to return before it returns, so they cannot write to
//       logFile after this point. CONNECT handler goroutines call g.wg.Add(1) before
//       Hijack(), so wg.Wait() (step 4) ensures they have also fully exited.
//     - upstream mode: all handler goroutines are tracked by wg; wg.Wait() (step 4)
//       ensures they have fully exited before logFile.Close() runs.
func (g *Gateway) Close() error {
	g.mu.Lock()
	cancel := g.cancel
	ln := g.listener
	srv := g.srv
	logFile := g.logFile
	conns := make([]net.Conn, len(g.conns))
	copy(conns, g.conns)
	// Clear stored state while holding the lock so concurrent Close calls are
	// harmless. g.logFile is captured and cleared here so a second concurrent
	// Close() call cannot race on the nil-check + Close() + nil-assignment below.
	g.cancel = nil
	g.listener = nil
	g.srv = nil
	g.conns = nil
	g.logFile = nil
	g.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if srv != nil {
		// Shutdown drains in-flight ServeHTTP goroutines (plain-HTTP handlers)
		// so they cannot write to g.logFile after logFile.Close(). A short
		// deadline prevents a hung handler from blocking the teardown forever.
		// The underlying listener is closed by Shutdown; ln.Close() below is a
		// harmless double-close (ErrClosed is _-ignored).
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
		_ = srv.Shutdown(shutdownCtx)
		shutdownCancel() // release the 3s timer immediately; Shutdown has already returned
	}
	if ln != nil {
		_ = ln.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}

	g.wg.Wait()

	// Release idle keep-alive connections held by the gateway-mode transport so
	// they are not leaked in tests or across back-to-back Close/Start cycles.
	// transport is non-nil only in gateway mode (proxy == "").
	if g.transport != nil {
		g.transport.CloseIdleConnections()
	}

	// Close the log file last. By this point:
	// - gateway mode: srv.Shutdown() has returned (all plain-HTTP handlers done),
	//   and CONNECT handler goroutines have called g.wg.Done() (tracked via g.wg.Add
	//   before Hijack); no goroutine can write logFile.
	// - upstream mode: wg.Wait() has returned; all handler goroutines have exited.
	// logFile was captured and cleared from g.logFile under the mutex above so
	// concurrent Close() calls do not race on this field.
	if logFile != nil {
		_ = logFile.Close()
	}

	return nil
}

