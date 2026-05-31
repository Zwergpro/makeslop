// Package networks owns the host-side HTTP forward proxy that bridges container
// traffic (via unix socket) to an upstream HTTP CONNECT proxy over TCP.
//
// # Data path
//
//	Container app ──unix socket──► Gateway ──TCP──► upstream proxy ──► internet
//
// The gateway forwards CONNECT tunnels verbatim without HTTP parsing — the
// payload is opaque TLS and cannot be inspected.
//
// When one copy direction finishes, halfCloseWrite signals EOF to the peer so
// the opposite io.Copy returns rather than blocking wg.Wait.
//
// # Socket-path limit
//
// Unix sockets have a 108-byte sun_path limit. Start returns the bind error if
// exceeded. Use /tmp/makeslop-<12hex>-<pid>.sock (~39 bytes).
package networks

import (
	"context"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
)

// Gateway listens on a unix socket and tunnels CONNECT requests to an upstream
// TCP proxy. Call Start/Close to manage the lifecycle; Close is idempotent.
type Gateway struct {
	socketPath string
	proxy      string

	mu       sync.Mutex
	listener net.Listener
	conns    []net.Conn // all in-flight connections (client + upstream halves)
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// NewGateway constructs a Gateway; neither socketPath nor proxy is validated —
// Start surfaces bind/listen errors.
func NewGateway(socketPath, proxy string) *Gateway {
	return &Gateway{
		socketPath: socketPath,
		proxy:      proxy,
	}
}

// SocketPath returns the unix socket path Start will bind; use it as the
// source for the container bind mount.
func (g *Gateway) SocketPath() string {
	return g.socketPath
}

// Start binds the unix socket (removing any stale file first), chmod 0666,
// and spawns the accept loop. ctx cancellation alone does not stop the loop
// (which blocks on Accept); only Close unblocks it. ctx is propagated to
// in-flight connection handlers.
//
// 0666 is required because container processes may run as a different UID;
// unix socket access is permission-checked against the socket's mode, not the
// bind-mount flags.
//
// A bind error (e.g. path too long) should abort the container launch.
//
// Start performs a single probe dial of the upstream address before accepting
// any connections. If the upstream is unreachable, Start tears down the
// listener and socket and returns the error so the container launch aborts
// loudly rather than silently black-holing traffic. The probe has a 5-second
// timeout; it checks TCP reachability only (a listening socket at the far end)
// and does not validate that the upstream speaks a valid HTTP CONNECT proxy
// protocol.
func (g *Gateway) Start(ctx context.Context) error {
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

	// Probe-dial the upstream to catch misconfiguration before any container
	// traffic is routed through the socket. A short timeout avoids blocking the
	// launch for a prolonged period on a bad address.
	probeCtx, probeCancel := context.WithTimeout(ctx, 5*time.Second)
	defer probeCancel()
	probe, err := (&net.Dialer{}).DialContext(probeCtx, "tcp", g.proxy)
	if err != nil {
		_ = ln.Close()
		_ = os.Remove(g.socketPath)
		return err
	}
	_ = probe.Close()

	proxyCtx, cancel := context.WithCancel(ctx)

	g.mu.Lock()
	g.listener = ln
	g.cancel = cancel
	g.mu.Unlock()

	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		g.acceptLoop(proxyCtx, ln)
	}()

	return nil
}

// acceptLoop accepts connections from ln until it is closed or ctx is done.
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

// handle splices bytes between client and upstream. When one direction
// finishes, halfCloseWrite lets the opposite direction drain rather than block.
func (g *Gateway) handle(ctx context.Context, client net.Conn) {
	defer client.Close()

	up, err := (&net.Dialer{}).DialContext(ctx, "tcp", g.proxy)
	if err != nil {
		return
	}
	defer up.Close()
	g.trackConn(up)
	defer g.untrackConn(up)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(up, client) //nolint:errcheck // EOF / reset are expected
		halfCloseWrite(up)
	}()
	go func() {
		defer wg.Done()
		io.Copy(client, up) //nolint:errcheck // EOF / reset are expected
		halfCloseWrite(client)
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
func (g *Gateway) Close() error {
	g.mu.Lock()
	cancel := g.cancel
	ln := g.listener
	conns := make([]net.Conn, len(g.conns))
	copy(conns, g.conns)
	// Clear stored state while holding the lock so concurrent Close calls are
	// harmless.
	g.cancel = nil
	g.listener = nil
	g.conns = nil
	g.mu.Unlock()

	if cancel != nil {
		cancel()
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
