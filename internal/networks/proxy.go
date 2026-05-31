// Package networks owns the host-side HTTP forward proxy that bridges container
// traffic (via unix socket) to either an upstream HTTP CONNECT proxy (upstream
// mode) or directly to the internet (gateway mode).
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
//	Container app ──unix socket──► Gateway ──TCP──► upstream proxy ──► internet
//
// # Socket-path limit
//
// Unix sockets have a 108-byte sun_path limit. Start returns the bind error if
// exceeded. Use /tmp/makeslop-<12hex>-<pid>.sock (~39 bytes).
//
// # Half-close protocol
//
// When one copy direction finishes, halfCloseWrite signals EOF to the peer so
// the opposite io.Copy returns rather than blocking wg.Wait.
package networks

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

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

// Gateway listens on a unix socket and either:
//   - (gateway mode, proxy == "") acts as a direct HTTP(S) forward proxy; or
//   - (upstream mode, proxy != "") tunnels all traffic to an upstream TCP proxy.
//
// Call Start/Close to manage the lifecycle; Close is idempotent.
type Gateway struct {
	socketPath string
	proxy      string
	logPath    string

	// gateway-mode only; nil in upstream mode
	srv       *http.Server
	transport *http.Transport

	mu       sync.Mutex
	listener net.Listener
	conns    []net.Conn // all in-flight connections (client + upstream halves)
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// NewGateway constructs a Gateway; none of the arguments are validated —
// Start surfaces bind/listen/dial errors.
//
// proxy: upstream address (host:port). Empty string → gateway mode (direct egress).
// logPath: path for the request log file. Empty string → logging disabled (Task 4).
func NewGateway(socketPath, proxy, logPath string) *Gateway {
	g := &Gateway{
		socketPath: socketPath,
		proxy:      proxy,
		logPath:    logPath,
	}
	if proxy == "" {
		// Gateway mode: allocate a shared transport for plain-HTTP forwarding.
		g.transport = &http.Transport{}
	}
	return g
}

// SocketPath returns the unix socket path Start will bind; use it as the
// source for the container bind mount.
func (g *Gateway) SocketPath() string {
	return g.socketPath
}

// Start binds the unix socket (removing any stale file first), chmod 0666,
// and starts serving. ctx cancellation alone does not stop the loop; only
// Close unblocks it. ctx is propagated to in-flight connection handlers.
//
// 0666 is required because container processes may run as a different UID;
// unix socket access is permission-checked against the socket's mode, not the
// bind-mount flags.
//
// A bind error (e.g. path too long) should abort the container launch.
//
// In upstream mode Start performs a single probe dial of the upstream address
// before accepting any connections. If the upstream is unreachable, Start tears
// down the listener and socket and returns the error so the container launch
// aborts loudly rather than silently black-holing traffic. The probe has a
// 5-second timeout; it checks TCP reachability only.
//
// In gateway mode (proxy == "") no probe dial is performed; the listener is
// handed to an http.Server whose ServeHTTP handles CONNECT and absolute-form HTTP.
func (g *Gateway) Start(ctx context.Context) error {
	// ── shared prologue ───────────────────────────────────────────────────────
	// Remove any stale socket left by a previous (crashed) run.
	_ = os.Remove(g.socketPath)

	// Set umask 0 before Listen to avoid a race window on socket mode, then restore.
	oldUmask := syscall.Umask(0)
	ln, err := net.Listen("unix", g.socketPath)
	syscall.Umask(oldUmask)
	if err != nil {
		return err
	}
	if err := os.Chmod(g.socketPath, 0o666); err != nil {
		_ = ln.Close()
		_ = os.Remove(g.socketPath)
		return err
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
		defer probeCancel()
		probe, err := (&net.Dialer{}).DialContext(probeCtx, "tcp", g.proxy)
		if err != nil {
			g.mu.Lock()
			g.cancel = nil
			g.listener = nil
			g.mu.Unlock()
			cancel()
			_ = ln.Close()
			_ = os.Remove(g.socketPath)
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

		srv := g.srv
		g.wg.Add(1)
		go func() {
			defer g.wg.Done()
			// ErrServerClosed is the normal exit when srv.Close() is called.
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
		clientConn, _, err := hj.Hijack()
		if err != nil {
			http.Error(w, "hijack failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		g.trackConn(clientConn)
		defer func() {
			g.untrackConn(clientConn)
			clientConn.Close()
		}()

		// Inform the client the tunnel is established.
		_, _ = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		g.logReq("CONNECT", r.Host)
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
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body) //nolint:errcheck // client disconnect is expected

	g.logReq(r.Method, r.URL.String())
}

// logReq logs a request line. It is a no-op when g.logger is nil (Task 4 wires
// the logger; this stub keeps the build green until then).
func (g *Gateway) logReq(_, _ string) {
	// no-op until Task 4 adds the logger
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
func (g *Gateway) handle(ctx context.Context, client net.Conn) {
	defer client.Close()

	up, err := (&net.Dialer{}).DialContext(ctx, "tcp", g.proxy)
	if err != nil {
		return
	}
	defer up.Close()
	g.trackConn(up)
	defer g.untrackConn(up)

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
//  2. g.srv.Close() (gateway mode) — closes the listener and any idle HTTP conns.
//     The existing ln.Close() below is a harmless double-close (ErrClosed, already _-ignored).
//     Hijacked CONNECT conns are detached from srv and are torn down via the trackConn loop.
//  3. Close all tracked conns — unblocks any in-flight splice goroutines.
//  4. wg.Wait() — drain all goroutines.
//  5. os.Remove(socket) — clean up.
func (g *Gateway) Close() error {
	g.mu.Lock()
	cancel := g.cancel
	ln := g.listener
	srv := g.srv
	conns := make([]net.Conn, len(g.conns))
	copy(conns, g.conns)
	// Clear stored state while holding the lock so concurrent Close calls are
	// harmless.
	g.cancel = nil
	g.listener = nil
	g.srv = nil
	g.conns = nil
	g.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if srv != nil {
		// srv.Close() closes the underlying listener too; the ln.Close() below
		// is a harmless double-close.
		_ = srv.Close()
	}
	if ln != nil {
		_ = ln.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}

	g.wg.Wait()
	_ = os.Remove(g.socketPath)
	return nil
}

