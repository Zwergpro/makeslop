package networks

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// tempSockPath returns a unique socket path under /tmp (a tmpfs on Linux that
// supports chmod on unix sockets). t.TempDir() can land on a filesystem that
// does not support chmod on sockets (e.g. the custom FS used by .gocache in
// this environment), so we use /tmp directly.
// The path is guaranteed to be well under the 108-byte sockaddr_un limit.
func tempSockPath(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets required; POSIX-only per CLAUDE.md")
	}
	// Use t.Name() to derive a unique short suffix (replace slashes that appear
	// in subtest names, then truncate so the whole path stays ≤ 108 bytes).
	name := strings.ReplaceAll(t.Name(), "/", "_")
	if len(name) > 30 {
		name = name[:30]
	}
	sock := fmt.Sprintf("/tmp/ms_test_%s_%d.sock", name, os.Getpid())
	t.Cleanup(func() { os.Remove(sock) })
	return sock
}

// startFakeUpstream starts a minimal TCP server that echoes bytes back to the
// caller. Returns the address and a cleanup function.
func startFakeUpstream(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fake upstream: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(c, c) //nolint:errcheck // echo server
			}()
		}
	}()
	return ln.Addr().String()
}

// TestStart_BindsAndChmodSocket verifies that Start creates the socket file and
// sets 0666 permissions (world-connectable so container processes with a
// different UID can connect).
func TestStart_BindsAndChmodSocket(t *testing.T) {
	sock := tempSockPath(t)
	p := NewProxy(sock, "127.0.0.1:1") // upstream address irrelevant for this test
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { p.Close() })

	info, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("socket file not found: %v", err)
	}
	if info.Mode()&0o777 != 0o666 {
		t.Errorf("socket permissions = %o, want 0666", info.Mode()&0o777)
	}
}

// TestSocketPath_Accessor verifies that SocketPath returns the configured path.
func TestSocketPath_Accessor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only per CLAUDE.md")
	}
	const want = "/tmp/makeslop-abc123-999.sock"
	p := NewProxy(want, "10.0.0.1:8888")
	if got := p.SocketPath(); got != want {
		t.Errorf("SocketPath() = %q, want %q", got, want)
	}
}

// TestCONNECT_TunnelRelaysBytesViaBothDirections verifies that the proxy
// relays bytes bidirectionally through an in-process fake upstream (an echo
// server at 127.0.0.1:0).
func TestCONNECT_TunnelRelaysBytesViaBothDirections(t *testing.T) {
	sock := tempSockPath(t)
	upstreamAddr := startFakeUpstream(t)

	p := NewProxy(sock, upstreamAddr)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	// Connect to the proxy via the unix socket.
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial unix socket: %v", err)
	}
	defer conn.Close()

	// Send a payload and expect it echoed back (fake upstream echoes everything).
	payload := "hello proxy\n"
	if _, err := fmt.Fprint(conn, payload); err != nil {
		t.Fatalf("write: %v", err)
	}

	buf := make([]byte, len(payload))
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != payload {
		t.Errorf("got %q, want %q", buf, payload)
	}
}

// TestHandle_HalfCloseTerminatesGoroutines verifies that when one side of a
// tunnel closes its write half, the opposite io.Copy goroutine returns without
// hanging. We test this by running handle directly with a pipe-backed conn.
func TestHandle_HalfCloseTerminatesGoroutines(t *testing.T) {
	sock := tempSockPath(t)
	upstreamAddr := startFakeUpstream(t)

	p := NewProxy(sock, upstreamAddr)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	// Send some data, then close the write half.
	payload := strings.Repeat("x", 64)
	if _, err := fmt.Fprint(conn, payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	// Half-close the client write side so the upstream sees EOF.
	if uc, ok := conn.(*net.UnixConn); ok {
		_ = uc.CloseWrite()
	}

	// Read all echoed data back; io.ReadAll returns after the server side
	// also half-closes (triggered by the proxy's halfCloseWrite call).
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != payload {
		t.Errorf("echoed %q, want %q", got, payload)
	}
	conn.Close()
}

// TestHandle_DialFailureClosesClientCleanly verifies that when the upstream
// is unreachable, handle returns without panicking and the client connection
// is closed.
func TestHandle_DialFailureClosesClientCleanly(t *testing.T) {
	sock := tempSockPath(t)
	// Use a port that is (almost certainly) not listening.
	p := NewProxy(sock, "127.0.0.1:1")
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// The proxy will fail to dial the upstream and close the client conn.
	// We should see an EOF or closed-connection error on the next read.
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1)
	_, err = conn.Read(buf)
	if err == nil {
		t.Error("expected error from read on closed connection, got nil")
	}
}

// TestClose_UnlinksSocket verifies that Close removes the socket file.
func TestClose_UnlinksSocket(t *testing.T) {
	sock := tempSockPath(t)
	p := NewProxy(sock, "127.0.0.1:1")
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Verify socket exists.
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("socket file missing before Close: %v", err)
	}

	p.Close()

	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("socket file still exists after Close (err=%v)", err)
	}
}

// TestClose_IsIdempotent verifies that calling Close multiple times does not
// panic or deadlock.
func TestClose_IsIdempotent(t *testing.T) {
	sock := tempSockPath(t)
	p := NewProxy(sock, "127.0.0.1:1")
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	done := make(chan struct{})
	go func() {
		p.Close()
		p.Close()
		p.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close is not idempotent — deadlock detected")
	}
}

// TestClose_BeforeStart verifies that Close on a never-started Proxy is safe.
func TestClose_BeforeStart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only per CLAUDE.md")
	}
	p := NewProxy("/tmp/nonexistent-makeslop-test.sock", "127.0.0.1:1")
	// Must not panic or deadlock.
	if err := p.Close(); err != nil {
		t.Errorf("Close before Start returned error: %v", err)
	}
}

// TestClose_DoesNotHangWithInFlightConnection verifies that Close terminates
// promptly even when a tunnel connection is in progress.
func TestClose_DoesNotHangWithInFlightConnection(t *testing.T) {
	sock := tempSockPath(t)
	// Use a fake upstream that blocks after accepting (simulates a slow upstream).
	blockLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen block upstream: %v", err)
	}
	defer blockLn.Close()

	// accepted is closed when the upstream's Accept() fires, which means the
	// proxy's handle goroutine has successfully established the in-flight
	// connection. This replaces the racy time.Sleep.
	accepted := make(chan struct{})
	go func() {
		c, err := blockLn.Accept()
		if err != nil {
			return
		}
		close(accepted)
		// Block: read forever (or until conn is closed by proxy teardown).
		buf := make([]byte, 1024)
		for {
			if _, err := c.Read(buf); err != nil {
				c.Close()
				return
			}
		}
	}()

	p := NewProxy(sock, blockLn.Addr().String())
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Establish a connection to the proxy (this will connect to the blocking upstream).
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Send a byte to trigger the proxy's handle goroutine to dial upstream.
	conn.Write([]byte("x")) //nolint:errcheck

	// Wait until the upstream has accepted the connection, confirming the
	// in-flight tunnel is established before we call Close.
	select {
	case <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream never accepted connection")
	}

	// Close must return promptly.
	done := make(chan struct{})
	go func() {
		p.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close hung with an in-flight connection")
	}
}

// TestCtxCancellation_StopsAcceptLoop verifies that cancelling the context
// passed to Start causes the accept loop to stop.
func TestCtxCancellation_StopsAcceptLoop(t *testing.T) {
	sock := tempSockPath(t)
	ctx, cancel := context.WithCancel(context.Background())

	p := NewProxy(sock, "127.0.0.1:1")
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Cancel the context — this should stop the accept loop.
	cancel()

	// After cancellation, Close should return promptly (wg already done or fast).
	done := make(chan struct{})
	go func() {
		p.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close hung after context cancellation")
	}
}

// TestStart_RemovesStaleSocket verifies that Start removes a stale socket file
// left from a previous run.
func TestStart_RemovesStaleSocket(t *testing.T) {
	sock := tempSockPath(t)

	// Create a stale socket file.
	if err := os.WriteFile(sock, []byte("stale"), 0o600); err != nil {
		t.Fatalf("create stale file: %v", err)
	}

	p := NewProxy(sock, "127.0.0.1:1")
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	// Verify the socket is now a real socket (not the stale file).
	info, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Errorf("expected socket file, got mode %v", info.Mode())
	}
}
