package networks

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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
	upstreamAddr := startFakeUpstream(t)
	g := NewGateway(sock, upstreamAddr, "")
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { g.Close() })

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
	g := NewGateway(want, "10.0.0.1:8888", "")
	if got := g.SocketPath(); got != want {
		t.Errorf("SocketPath() = %q, want %q", got, want)
	}
}

func TestCONNECT_TunnelRelaysBytesViaBothDirections(t *testing.T) {
	sock := tempSockPath(t)
	upstreamAddr := startFakeUpstream(t)

	g := NewGateway(sock, upstreamAddr, "")
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer g.Close()

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

	g := NewGateway(sock, upstreamAddr, "")
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer g.Close()

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

	// Start a real upstream so the probe-dial in Start succeeds, then close it
	// immediately so that per-connection dials from the handle goroutine fail.
	probeLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen probe upstream: %v", err)
	}
	t.Cleanup(func() { probeLn.Close() })
	upstreamAddr := probeLn.Addr().String()
	// closed is closed after probeLn.Close() fires so the client dial below
	// cannot connect before the listener is gone. Without this signal the test
	// is racy: handle() may succeed its DialContext against the still-open
	// listener and the conn.Read assertion would pass only via the 3-second
	// deadline rather than via the intended connection-refused error.
	closed := make(chan struct{})
	// Accept the probe connection in the background to unblock Start.
	go func() {
		c, err := probeLn.Accept()
		if err != nil {
			close(closed)
			return
		}
		c.Close()
		probeLn.Close() // stop accepting so subsequent dials fail
		close(closed)
	}()

	g := NewGateway(sock, upstreamAddr, "")
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer g.Close()

	// Wait until the probe listener is closed before dialling so that the
	// handle goroutine's DialContext is guaranteed to fail immediately.
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("probe listener never closed")
	}

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
	upstreamAddr := startFakeUpstream(t)
	g := NewGateway(sock, upstreamAddr, "")
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("socket file missing before Close: %v", err)
	}

	g.Close()

	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("socket file still exists after Close (err=%v)", err)
	}
}

func TestClose_IsIdempotent(t *testing.T) {
	sock := tempSockPath(t)
	upstreamAddr := startFakeUpstream(t)
	g := NewGateway(sock, upstreamAddr, "")
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	done := make(chan struct{})
	go func() {
		g.Close()
		g.Close()
		g.Close()
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
	g := NewGateway("/tmp/nonexistent-makeslop-test.sock", "127.0.0.1:1", "")
	if err := g.Close(); err != nil {
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

	g := NewGateway(sock, blockLn.Addr().String(), "")
	if err := g.Start(context.Background()); err != nil {
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
		g.Close()
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
	upstreamAddr := startFakeUpstream(t)
	ctx, cancel := context.WithCancel(context.Background())

	g := NewGateway(sock, upstreamAddr, "")
	if err := g.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cancel()

	done := make(chan struct{})
	go func() {
		g.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close hung after context cancellation")
	}
}

// TestStart_UnreachableUpstreamReturnsError verifies that Start fails fast and
// leaves no socket file behind when the upstream is not reachable.
func TestStart_UnreachableUpstreamReturnsError(t *testing.T) {
	sock := tempSockPath(t)
	g := NewGateway(sock, "127.0.0.1:1", "") // port 1 is never open

	err := g.Start(context.Background())
	if err == nil {
		g.Close()
		t.Fatal("Start: expected error for unreachable upstream, got nil")
	}

	if _, statErr := os.Stat(sock); !os.IsNotExist(statErr) {
		t.Errorf("socket file should not exist after failed Start (stat err=%v)", statErr)
	}
}

// TestClose_IdempotentAfterFailedStart verifies that Close does not panic or
// deadlock when called after Start returned an error (i.e. nothing was set up).
func TestClose_IdempotentAfterFailedStart(t *testing.T) {
	sock := tempSockPath(t)
	g := NewGateway(sock, "127.0.0.1:1", "") // unreachable — Start will fail

	if err := g.Start(context.Background()); err == nil {
		g.Close()
		t.Fatal("expected Start to fail; got nil")
	}

	done := make(chan struct{})
	go func() {
		g.Close()
		g.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close deadlocked after failed Start")
	}
}

// TestStart_ReachableUpstreamSucceedsAndTunnels verifies that Start succeeds
// when the upstream is reachable and that bytes are tunnelled end-to-end.
// (This complements the existing tunnel test with an explicit reachability focus.)
func TestStart_ReachableUpstreamSucceedsAndTunnels(t *testing.T) {
	sock := tempSockPath(t)
	upstreamAddr := startFakeUpstream(t)

	g := NewGateway(sock, upstreamAddr, "")
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start with reachable upstream: %v", err)
	}
	defer g.Close()

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial unix socket: %v", err)
	}
	defer conn.Close()

	payload := "probe-ok\n"
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

// TestSplice_CopiesBothDirections verifies that splice relays bytes in both
// directions and propagates EOF via halfCloseWrite so both goroutines terminate.
func TestSplice_CopiesBothDirections(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only per CLAUDE.md")
	}

	// Use two TCP pairs so both sides support CloseWrite (TCP half-close).
	ln1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen1: %v", err)
	}
	defer ln1.Close()

	aClientCh := make(chan net.Conn, 1)
	go func() {
		c, err := net.Dial("tcp", ln1.Addr().String())
		if err != nil {
			aClientCh <- nil
			return
		}
		aClientCh <- c
	}()
	aServer, err := ln1.Accept()
	if err != nil {
		t.Fatalf("accept1: %v", err)
	}
	aClient := <-aClientCh
	if aClient == nil {
		t.Fatal("dial1 failed")
	}
	defer aServer.Close()
	defer aClient.Close()

	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen2: %v", err)
	}
	defer ln2.Close()

	bClientCh := make(chan net.Conn, 1)
	go func() {
		c, err := net.Dial("tcp", ln2.Addr().String())
		if err != nil {
			bClientCh <- nil
			return
		}
		bClientCh <- c
	}()
	bServer, err := ln2.Accept()
	if err != nil {
		t.Fatalf("accept2: %v", err)
	}
	bClient := <-bClientCh
	if bClient == nil {
		t.Fatal("dial2 failed")
	}
	defer bServer.Close()
	defer bClient.Close()

	// splice connects aServer (a) ↔ bClient (b).
	g := &Gateway{}
	spliceFinished := make(chan struct{})
	go func() {
		g.splice(aServer, bClient)
		close(spliceFinished)
	}()

	// Direction a→b: write from aClient, read from bServer.
	aToB := "hello from a"
	if _, err := aClient.Write([]byte(aToB)); err != nil {
		t.Fatalf("write a→b: %v", err)
	}
	if tc, ok := aClient.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}

	bServer.SetDeadline(time.Now().Add(3 * time.Second))
	gotAtoB, err := io.ReadAll(bServer)
	if err != nil {
		t.Fatalf("ReadAll bServer (a→b): %v", err)
	}
	if string(gotAtoB) != aToB {
		t.Errorf("a→b: got %q, want %q", gotAtoB, aToB)
	}

	// Direction b→a: write from bServer, read from aClient.
	bToA := "hello from b"
	if _, err := bServer.Write([]byte(bToA)); err != nil {
		t.Fatalf("write b→a: %v", err)
	}
	if tc, ok := bServer.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}

	aClient.SetDeadline(time.Now().Add(3 * time.Second))
	gotBtoA, err := io.ReadAll(aClient)
	if err != nil {
		t.Fatalf("ReadAll aClient (b→a): %v", err)
	}
	if string(gotBtoA) != bToA {
		t.Errorf("b→a: got %q, want %q", gotBtoA, bToA)
	}

	// Both halves have EOF'd — splice should have returned.
	select {
	case <-spliceFinished:
	case <-time.After(3 * time.Second):
		t.Fatal("splice did not return after both sides closed")
	}
}

func TestStart_RemovesStaleSocket(t *testing.T) {
	sock := tempSockPath(t)
	upstreamAddr := startFakeUpstream(t)

	if err := os.WriteFile(sock, []byte("stale"), 0o600); err != nil {
		t.Fatalf("create stale file: %v", err)
	}

	g := NewGateway(sock, upstreamAddr, "")
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer g.Close()

	info, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Errorf("expected socket file, got mode %v", info.Mode())
	}
}

// ── gateway mode tests ────────────────────────────────────────────────────────

// startFakeTCPTarget starts a minimal TCP server that echoes everything it
// receives back to the caller. This simulates a direct TCP target for CONNECT
// tunnels.
func startFakeTCPTarget(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fake TCP target: %v", err)
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

// dialUnixHTTP dials the unix socket and wraps it with an http.Transport so we
// can use http.Client for testing gateway mode without an actual network call.
func dialUnixHTTP(sock string) *http.Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}
	return &http.Client{Transport: transport}
}

// TestGatewayMode_Start_NoProbeDial verifies that gateway mode (proxy == "")
// starts without requiring any upstream — no probe-dial.
func TestGatewayMode_Start_NoProbeDial(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only per CLAUDE.md")
	}
	sock := tempSockPath(t)
	g := NewGateway(sock, "", "") // gateway mode: no upstream
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start (gateway mode): %v", err)
	}
	t.Cleanup(func() { g.Close() })

	// Verify socket exists and has the right permissions.
	info, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("socket file not found after gateway Start: %v", err)
	}
	if info.Mode()&0o777 != 0o666 {
		t.Errorf("socket permissions = %o, want 0666", info.Mode()&0o777)
	}
}

// TestGatewayMode_CONNECT_TunnelToDirectTarget verifies that a CONNECT request
// over the unix socket is forwarded directly to a local TCP target and that
// bytes round-trip correctly.
func TestGatewayMode_CONNECT_TunnelToDirectTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only per CLAUDE.md")
	}
	sock := tempSockPath(t)
	targetAddr := startFakeTCPTarget(t)

	g := NewGateway(sock, "", "")
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start (gateway mode): %v", err)
	}
	defer g.Close()

	// Open a raw connection to the unix socket.
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial unix socket: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	// Send a CONNECT request to the gateway.
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", targetAddr, targetAddr)

	// Read the 200 response.
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT response status = %d, want 200", resp.StatusCode)
	}

	// The tunnel is established. Write some data and read the echo.
	payload := "hello gateway CONNECT\n"
	if _, err := fmt.Fprint(conn, payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != payload {
		t.Errorf("got %q, want %q", buf, payload)
	}
}

// TestGatewayMode_PlainHTTP_AbsoluteForm verifies that absolute-form HTTP
// requests are forwarded to the target httptest server and the response is
// returned to the caller.
func TestGatewayMode_PlainHTTP_AbsoluteForm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only per CLAUDE.md")
	}
	sock := tempSockPath(t)

	// Start a local HTTP server that serves a known response.
	const wantBody = "hello from origin"
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, wantBody)
	}))
	defer origin.Close()

	g := NewGateway(sock, "", "")
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start (gateway mode): %v", err)
	}
	defer g.Close()

	// Configure an http.Client that sends all requests through the unix socket.
	client := dialUnixHTTP(sock)
	client.Timeout = 5 * time.Second

	// Make a request to the origin using the proxy.
	resp, err := client.Get(origin.URL + "/test")
	if err != nil {
		t.Fatalf("GET via gateway: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != wantBody {
		t.Errorf("body = %q, want %q", body, wantBody)
	}
}

// TestGatewayMode_Close_RemovesSocket verifies that Close removes the socket
// file in gateway mode.
func TestGatewayMode_Close_RemovesSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only per CLAUDE.md")
	}
	sock := tempSockPath(t)
	g := NewGateway(sock, "", "")
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start (gateway mode): %v", err)
	}

	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("socket file missing before Close: %v", err)
	}

	g.Close()

	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("socket file still exists after Close (err=%v)", err)
	}
}

// ── Task 4: request logging tests ────────────────────────────────────────────

// TestGatewayLogging_CONNECT verifies that a CONNECT request writes a log line.
func TestGatewayLogging_CONNECT(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only per CLAUDE.md")
	}
	sock := tempSockPath(t)
	logFile := sock + ".log"
	t.Cleanup(func() { os.Remove(logFile) })
	targetAddr := startFakeTCPTarget(t)

	g := NewGateway(sock, "", logFile)
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer g.Close()

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial unix socket: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	// Send CONNECT.
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", targetAddr, targetAddr)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}

	// Send some data so the tunnel is exercised, then close.
	fmt.Fprint(conn, "ping\n")
	conn.Close()

	// Allow goroutines to flush and let g.Close() close the file.
	g.Close()

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(data), "CONNECT "+targetAddr) {
		t.Errorf("log does not contain CONNECT entry; got:\n%s", data)
	}
}

// TestGatewayLogging_PlainGET verifies that a plain GET request writes a log line.
func TestGatewayLogging_PlainGET(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only per CLAUDE.md")
	}
	sock := tempSockPath(t)
	logFile := sock + ".log"
	t.Cleanup(func() { os.Remove(logFile) })

	const wantBody = "ok"
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, wantBody)
	}))
	defer origin.Close()

	g := NewGateway(sock, "", logFile)
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer g.Close()

	client := dialUnixHTTP(sock)
	client.Timeout = 5 * time.Second

	resp, err := client.Get(origin.URL + "/logtest")
	if err != nil {
		t.Fatalf("GET via gateway: %v", err)
	}
	resp.Body.Close()

	// Close so log file is flushed/closed before reading.
	g.Close()

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(data), "GET ") {
		t.Errorf("log does not contain GET entry; got:\n%s", data)
	}
}

// TestUpstreamLogging_RequestLineLogged verifies that upstream mode logs the
// first request line AND forwards bytes verbatim to the fake upstream.
func TestUpstreamLogging_RequestLineLogged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only per CLAUDE.md")
	}
	sock := tempSockPath(t)
	logFile := sock + ".log"
	t.Cleanup(func() { os.Remove(logFile) })

	// Start a recording upstream that captures everything it receives.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen upstream: %v", err)
	}
	defer ln.Close()
	received := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		// Accept probe connection first (from Start).
		// Second connection is the actual data.
		c2, err := ln.Accept()
		if err != nil {
			return
		}
		defer c2.Close()
		data, _ := io.ReadAll(c2)
		received <- data
	}()

	upstreamAddr := ln.Addr().String()
	g := NewGateway(sock, upstreamAddr, logFile)
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer g.Close()

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial unix socket: %v", err)
	}
	payload := "GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n"
	conn.Write([]byte(payload)) //nolint:errcheck
	conn.Close()

	// Wait for upstream to receive.
	var upstreamGot []byte
	select {
	case upstreamGot = <-received:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream never received data")
	}

	g.Close()

	// Verify data forwarded verbatim.
	if !strings.Contains(string(upstreamGot), "GET http://example.com/") {
		t.Errorf("upstream did not receive forwarded request; got: %q", upstreamGot)
	}

	// Verify log entry.
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(data), "GET http://example.com/") {
		t.Errorf("log does not contain request line; got:\n%s", data)
	}
}

// TestUpstreamLogging_Off verifies that when logging is off the upstream path
// forwards bytes unchanged and no log file is created.
func TestUpstreamLogging_Off(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only per CLAUDE.md")
	}
	sock := tempSockPath(t)
	upstreamAddr := startFakeUpstream(t)

	// logPath = "" → no logging
	g := NewGateway(sock, upstreamAddr, "")
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer g.Close()

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	payload := "hello logging off\n"
	conn.Write([]byte(payload)) //nolint:errcheck

	buf := make([]byte, len(payload))
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != payload {
		t.Errorf("got %q, want %q", buf, payload)
	}

	// Verify g.logger is nil (no logging wired).
	if g.logger != nil {
		t.Error("logger should be nil when logPath is empty")
	}
}

// TestStart_FailLoud_BadLogPath verifies that Start fails and removes the socket
// when logPath points to a non-existent directory.
func TestStart_FailLoud_BadLogPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only per CLAUDE.md")
	}
	sock := tempSockPath(t)

	// Use a path inside a non-existent directory — OpenFile will fail.
	badLogPath := "/tmp/nonexistent_dir_makeslop_test/request.log"

	g := NewGateway(sock, "", badLogPath)
	err := g.Start(context.Background())
	if err == nil {
		g.Close()
		t.Fatal("Start: expected error for bad logPath, got nil")
	}

	// Socket must be cleaned up.
	if _, statErr := os.Stat(sock); !os.IsNotExist(statErr) {
		t.Errorf("socket file should not exist after failed Start (stat err=%v)", statErr)
	}
}

// TestGatewayMode_Close_TearDownInFlightTunnel verifies that Close tears down
// an in-flight CONNECT tunnel (the hijacked conn is tracked and force-closed)
// and that Close does not hang.
func TestGatewayMode_Close_TearDownInFlightTunnel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only per CLAUDE.md")
	}
	sock := tempSockPath(t)

	// blockLn simulates a slow target that accepts a connection but blocks.
	blockLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen block target: %v", err)
	}
	defer blockLn.Close()

	targetAccepted := make(chan struct{})
	go func() {
		c, err := blockLn.Accept()
		if err != nil {
			return
		}
		close(targetAccepted)
		// Block until the connection is closed by Close().
		buf := make([]byte, 64)
		for {
			if _, err := c.Read(buf); err != nil {
				c.Close()
				return
			}
		}
	}()

	g := NewGateway(sock, "", "")
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start (gateway mode): %v", err)
	}

	// Establish a CONNECT tunnel.
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial unix: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	targetAddr := blockLn.Addr().String()
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", targetAddr, targetAddr)

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT response = %d, want 200", resp.StatusCode)
	}

	// Wait for the target to accept the connection (tunnel is in-flight).
	select {
	case <-targetAccepted:
	case <-time.After(3 * time.Second):
		t.Fatal("target never accepted connection")
	}

	// Close must tear down the tunnel and not hang.
	done := make(chan struct{})
	go func() {
		g.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung with an in-flight CONNECT tunnel")
	}

	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("socket file still exists after Close (err=%v)", err)
	}
}
