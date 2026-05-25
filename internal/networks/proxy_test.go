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

// 0666 so container processes with a different UID can connect.
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

func TestCONNECT_TunnelRelaysBytesViaBothDirections(t *testing.T) {
	sock := tempSockPath(t)
	upstreamAddr := startFakeUpstream(t)

	p := NewProxy(sock, upstreamAddr)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial unix socket: %v", err)
	}
	defer conn.Close()

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

	payload := strings.Repeat("x", 64)
	if _, err := fmt.Fprint(conn, payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	if uc, ok := conn.(*net.UnixConn); ok {
		_ = uc.CloseWrite()
	}

	// io.ReadAll returns once the server half-closes (proxy's halfCloseWrite propagates EOF).
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

func TestHandle_DialFailureClosesClientCleanly(t *testing.T) {
	sock := tempSockPath(t)
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

	conn.SetDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1)
	_, err = conn.Read(buf)
	if err == nil {
		t.Error("expected error from read on closed connection, got nil")
	}
}

func TestClose_UnlinksSocket(t *testing.T) {
	sock := tempSockPath(t)
	p := NewProxy(sock, "127.0.0.1:1")
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("socket file missing before Close: %v", err)
	}

	p.Close()

	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("socket file still exists after Close (err=%v)", err)
	}
}

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

func TestClose_BeforeStart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only per CLAUDE.md")
	}
	p := NewProxy("/tmp/nonexistent-makeslop-test.sock", "127.0.0.1:1")
	if err := p.Close(); err != nil {
		t.Errorf("Close before Start returned error: %v", err)
	}
}

func TestClose_DoesNotHangWithInFlightConnection(t *testing.T) {
	sock := tempSockPath(t)
	// blockLn simulates a slow upstream — blocks after accepting so Close must drain in-flight goroutines.
	blockLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen block upstream: %v", err)
	}
	defer blockLn.Close()

	// accepted is closed when upstream Accept fires — proves handle is in-flight
	// before we call Close, avoiding a racy time.Sleep.
	accepted := make(chan struct{})
	go func() {
		c, err := blockLn.Accept()
		if err != nil {
			return
		}
		close(accepted)
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

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	conn.Write([]byte("x")) //nolint:errcheck

	select {
	case <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream never accepted connection")
	}

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

func TestCtxCancellation_StopsAcceptLoop(t *testing.T) {
	sock := tempSockPath(t)
	ctx, cancel := context.WithCancel(context.Background())

	p := NewProxy(sock, "127.0.0.1:1")
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cancel()

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

func TestStart_RemovesStaleSocket(t *testing.T) {
	sock := tempSockPath(t)

	if err := os.WriteFile(sock, []byte("stale"), 0o600); err != nil {
		t.Fatalf("create stale file: %v", err)
	}

	p := NewProxy(sock, "127.0.0.1:1")
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	info, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Errorf("expected socket file, got mode %v", info.Mode())
	}
}
