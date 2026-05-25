// Package networks owns the host-side HTTP forward proxy that bridges container
// traffic (via unix socket) to an upstream HTTP CONNECT proxy over TCP.
//
// # Data path
//
//	Container app ──unix socket──► Proxy ──TCP──► upstream proxy ──► internet
//
// The proxy forwards CONNECT tunnels verbatim without HTTP parsing — the
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
)

// Proxy listens on a unix socket and tunnels CONNECT requests to an upstream
// TCP proxy. Call Start/Close to manage the lifecycle; Close is idempotent.
type Proxy struct {
	socketPath string
	upstream   string

	mu       sync.Mutex
	listener net.Listener
	conns    []net.Conn // all in-flight connections (client + upstream halves)
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// NewProxy constructs a Proxy; neither socketPath nor upstream is validated —
// Start surfaces bind/listen errors.
func NewProxy(socketPath, upstream string) *Proxy {
	return &Proxy{
		socketPath: socketPath,
		upstream:   upstream,
	}
}

// SocketPath returns the unix socket path Start will bind; use it as the
// source for the container bind mount.
func (p *Proxy) SocketPath() string {
	return p.socketPath
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
func (p *Proxy) Start(ctx context.Context) error {
	// Remove any stale socket left by a previous (crashed) run.
	_ = os.Remove(p.socketPath)

	// Set umask 0 before Listen to avoid a race window on socket mode, then restore.
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
			// Listener closed by Close() — stop.
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

// handle splices bytes between client and upstream. When one direction
// finishes, halfCloseWrite lets the opposite direction drain rather than block.
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

// halfCloseWrite signals EOF to the peer by closing the write half of c;
// types without CloseWrite are silently ignored.
func halfCloseWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

func (p *Proxy) trackConn(c net.Conn) {
	p.mu.Lock()
	p.conns = append(p.conns, c)
	p.mu.Unlock()
}

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

// Close is idempotent: safe to call before Start or after a failed Start.
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
