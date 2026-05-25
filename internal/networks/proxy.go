// Package networks owns the host-side HTTP forward proxy that bridges container
// traffic (arriving over a unix domain socket) to an upstream HTTP CONNECT
// proxy reachable over TCP.
//
// # Data path
//
//	Container app ──unix socket──► Proxy ──TCP──► upstream proxy ──► internet
//
// The proxy speaks a verbatim-forward protocol: after the container app sends
// a CONNECT request the proxy dials the upstream, replays bytes in both
// directions without further HTTP parsing, and tears down when either side
// closes.  This is the only correct shape for CONNECT tunnelling because the
// payload is opaque TLS.
//
// # Half-close protocol
//
// Each direction uses a dedicated io.Copy goroutine. When one direction
// finishes copying, halfCloseWrite shuts down the write half of the
// destination connection so the peer's io.Copy sees EOF and returns, rather
// than blocking until a full TCP close. This lets handlers terminate promptly
// and prevents wg.Wait from hanging.
//
// # POSIX-only / 108-byte socket-path limit
//
// Unix domain sockets on Linux and macOS have a maximum path length of 108
// bytes (the sun_path field in sockaddr_un). Callers must ensure the socket
// path is within that limit; Start returns the bind error if it is exceeded.
// The recommended scheme is /tmp/makeslop-<12hex>-<pid>.sock (~39 bytes).
package networks

import (
	"context"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
)

// Proxy is a host-side HTTP forward proxy that listens on a unix domain socket
// and tunnels connections verbatim to an upstream HTTP CONNECT proxy over TCP.
//
// Lifecycle: call Start to begin accepting connections, call Close to
// shut down. Close is idempotent and safe to call before Start or on a
// partially-started Proxy.
type Proxy struct {
	socketPath string
	upstream   string

	mu       sync.Mutex
	listener net.Listener
	conns    []net.Conn // all in-flight connections (client + upstream halves)
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// NewProxy returns a new Proxy that will listen on socketPath (a host-side
// unix socket path) and forward CONNECT tunnels to upstream ("host:port").
// Neither value is validated here; Start surfaces errors on bind/listen.
func NewProxy(socketPath, upstream string) *Proxy {
	return &Proxy{
		socketPath: socketPath,
		upstream:   upstream,
	}
}

// SocketPath returns the host path of the unix socket that Start will bind.
// Use this value as the source path when bind-mounting the socket into a
// container.
func (p *Proxy) SocketPath() string {
	return p.socketPath
}

// Start removes any stale socket file, listens on the configured unix socket
// path, chmods the socket to 0666, and spawns the accept loop in a background
// goroutine. The accept loop runs until Close is called; ctx cancellation alone
// does not stop it because the loop blocks on ln.Accept() — only
// listener.Close() (which Close() calls) unblocks that call. ctx is
// propagated to per-connection handlers so that in-flight dials are
// interrupted when Close is called.
//
// The socket is created with mode 0666 (world-connectable) because container
// processes may run as a different UID than the host user; connecting to a unix
// socket is permission-checked against the socket's mode bits on the host, not
// the bind-mount flags. The path is unique (/tmp/makeslop-<hash12>-<pid>.sock)
// so broad permissions are safe.
//
// Start returns an error if the socket cannot be bound (e.g. path too long,
// permission denied). Such an error should abort the container launch — the
// network isolation invariant requires the socket to exist before the
// container starts.
func (p *Proxy) Start(ctx context.Context) error {
	// Remove any stale socket left by a previous (crashed) run.
	_ = os.Remove(p.socketPath)

	// Set umask to 0 before net.Listen so the socket is created with mode
	// 0777 immediately (no race window where a too-permissive or
	// too-restrictive mode is visible). The path is unique
	// (/tmp/makeslop-<hash12>-<pid>.sock) so broad permissions are
	// acceptable; we then chmod to 0666 explicitly so the socket is
	// world-connectable (required because container processes may run as a
	// different UID — connecting to a unix socket is permission-checked
	// against the socket's mode bits on the host, not the bind-mount flags).
	oldUmask := syscall.Umask(0)
	ln, err := net.Listen("unix", p.socketPath)
	syscall.Umask(oldUmask)
	if err != nil {
		return err
	}
	if err := os.Chmod(p.socketPath, 0o666); err != nil {
		_ = ln.Close()
		_ = os.Remove(p.socketPath)
		return err
	}

	proxyCtx, cancel := context.WithCancel(ctx)

	p.mu.Lock()
	p.listener = ln
	p.cancel = cancel
	p.mu.Unlock()

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.acceptLoop(proxyCtx, ln)
	}()

	return nil
}

// acceptLoop accepts connections from ln until it is closed or ctx is done.
func (p *Proxy) acceptLoop(ctx context.Context, ln net.Listener) {
	for {
		client, err := ln.Accept()
		if err != nil {
			// Listener was closed (via Close or ctx cancellation) — stop.
			return
		}
		p.trackConn(client)
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer p.untrackConn(client)
			p.handle(ctx, client)
		}()
	}
}

// handle dials the upstream proxy and splices bytes between client and
// upstream in both directions. When one direction finishes it calls
// halfCloseWrite on the destination so the opposite direction drains and
// returns rather than blocking.
func (p *Proxy) handle(ctx context.Context, client net.Conn) {
	defer client.Close()

	up, err := (&net.Dialer{}).DialContext(ctx, "tcp", p.upstream)
	if err != nil {
		return
	}
	defer up.Close()
	p.trackConn(up)
	defer p.untrackConn(up)

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

// halfCloseWrite shuts down the write half of c so the peer's io.Copy sees
// EOF and returns. Both *net.UnixConn and *net.TCPConn implement the
// CloseWrite method; other types are silently ignored.
func halfCloseWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// trackConn registers c in the tracked connection list so Close can
// force-close it.
func (p *Proxy) trackConn(c net.Conn) {
	p.mu.Lock()
	p.conns = append(p.conns, c)
	p.mu.Unlock()
}

// untrackConn removes c from the tracked connection list.
func (p *Proxy) untrackConn(c net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, tracked := range p.conns {
		if tracked == c {
			p.conns[i] = p.conns[len(p.conns)-1]
			p.conns = p.conns[:len(p.conns)-1]
			return
		}
	}
}

// Close shuts down the proxy: cancels the proxy-scoped context, closes the
// listener, force-closes every in-flight connection, waits for all goroutines
// to finish, then removes the socket file. Close is idempotent and safe to
// call before Start or on a Proxy whose Start returned an error.
func (p *Proxy) Close() error {
	p.mu.Lock()
	cancel := p.cancel
	ln := p.listener
	conns := make([]net.Conn, len(p.conns))
	copy(conns, p.conns)
	// Clear stored state while holding the lock so concurrent Close calls are
	// harmless.
	p.cancel = nil
	p.listener = nil
	p.conns = nil
	p.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if ln != nil {
		_ = ln.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}

	p.wg.Wait()
	_ = os.Remove(p.socketPath)
	return nil
}
