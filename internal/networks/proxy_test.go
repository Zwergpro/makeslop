package networks

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// startEchoTCPServer starts a minimal TCP echo server that echoes every byte
// it receives back to the sender. Returns the listening address.
// Used both as a fake upstream (upstream mode) and as a fake target for CONNECT
// tunnels (gateway mode). The two helper functions that existed previously
// (startFakeUpstream and startFakeTCPTarget) were byte-for-byte identical and
// have been consolidated here.
func startEchoTCPServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen echo TCP server: %v", err)
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

// startFakeUpstream starts a TCP echo server for upstream-mode tests.
func startFakeUpstream(t *testing.T) string {
	t.Helper()
	return startEchoTCPServer(t)
}

// startFakeTCPTarget starts a TCP echo server for gateway-mode CONNECT tests.
func startFakeTCPTarget(t *testing.T) string {
	t.Helper()
	return startEchoTCPServer(t)
}

// dialTCPHTTP creates an http.Client that routes all requests through the
// Gateway's TCP listener at addr. Used for testing gateway mode.
func dialTCPHTTP(addr string) *http.Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		},
	}
	return &http.Client{Transport: transport}
}

// ── TCP bind / port tests ─────────────────────────────────────────────────────

// TestStart_BindsTCPAndReportsNonZeroPort verifies that Start binds a TCP
// listener on 127.0.0.1 and exposes a non-zero port via Port().
func TestStart_BindsTCPAndReportsNonZeroPort(t *testing.T) {
	g := NewGateway("", "") // gateway mode
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { g.Close() })

	port := g.Port()
	if port == 0 {
		t.Fatal("Port() returned 0 after Start — listener did not bind")
	}
	addr := g.Addr()
	if addr == nil {
		t.Fatal("Addr() returned nil after Start")
	}
	// Must be a TCP loopback address.
	ta, ok := addr.(*net.TCPAddr)
	if !ok {
		t.Fatalf("Addr() type = %T, want *net.TCPAddr", addr)
	}
	if !ta.IP.IsLoopback() {
		t.Errorf("Addr() IP = %v, want loopback", ta.IP)
	}
}

// TestAddr_NilBeforeStart verifies that Addr() returns nil before Start.
func TestAddr_NilBeforeStart(t *testing.T) {
	g := NewGateway("", "")
	if g.Addr() != nil {
		t.Error("Addr() should return nil before Start")
	}
	if g.Port() != 0 {
		t.Error("Port() should return 0 before Start")
	}
}

// ── upstream mode tests ───────────────────────────────────────────────────────

// TestUpstreamMode_TunnelRelaysBytesViaBothDirections verifies that upstream
// mode (proxy != "") relays raw bytes bidirectionally without HTTP parsing.
// This is a byte-pipe test for the upstream path, NOT the gateway CONNECT path.
func TestUpstreamMode_TunnelRelaysBytesViaBothDirections(t *testing.T) {
	upstreamAddr := startFakeUpstream(t)

	g := NewGateway(upstreamAddr, "")
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer g.Close()

	addr := g.Addr()
	if addr == nil {
		t.Fatal("Addr() is nil after Start")
	}
	conn, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatalf("dial gateway TCP: %v", err)
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
	upstreamAddr := startFakeUpstream(t)

	g := NewGateway(upstreamAddr, "")
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer g.Close()

	conn, err := net.Dial("tcp", g.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	payload := strings.Repeat("x", 64)
	if _, err := fmt.Fprint(conn, payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
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

	g := NewGateway(upstreamAddr, "")
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

	conn, err := net.Dial("tcp", g.Addr().String())
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

func TestClose_IsIdempotent(t *testing.T) {
	upstreamAddr := startFakeUpstream(t)
	g := NewGateway(upstreamAddr, "")
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
	g := NewGateway("127.0.0.1:1", "")
	if err := g.Close(); err != nil {
		t.Errorf("Close before Start returned error: %v", err)
	}
}

func TestClose_DoesNotHangWithInFlightConnection(t *testing.T) {
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
		// Accept the probe connection first.
		c2, err := blockLn.Accept()
		if err != nil {
			c.Close()
			return
		}
		close(accepted)
		buf := make([]byte, 1024)
		for {
			if _, err := c2.Read(buf); err != nil {
				c2.Close()
				c.Close()
				return
			}
		}
	}()

	g := NewGateway(blockLn.Addr().String(), "")
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	conn, err := net.Dial("tcp", g.Addr().String())
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
	upstreamAddr := startFakeUpstream(t)
	ctx, cancel := context.WithCancel(context.Background())

	g := NewGateway(upstreamAddr, "")
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
// tears down cleanly when the upstream is not reachable.
func TestStart_UnreachableUpstreamReturnsError(t *testing.T) {
	g := NewGateway("127.0.0.1:1", "") // port 1 is never open

	err := g.Start(context.Background())
	if err == nil {
		g.Close()
		t.Fatal("Start: expected error for unreachable upstream, got nil")
	}

	// After a failed Start, Addr() must return nil.
	if g.Addr() != nil {
		t.Error("Addr() should be nil after failed Start")
	}
}

// TestClose_IdempotentAfterFailedStart verifies that Close does not panic or
// deadlock when called after Start returned an error (i.e. nothing was set up).
func TestClose_IdempotentAfterFailedStart(t *testing.T) {
	g := NewGateway("127.0.0.1:1", "") // unreachable — Start will fail

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
	upstreamAddr := startFakeUpstream(t)

	g := NewGateway(upstreamAddr, "")
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start with reachable upstream: %v", err)
	}
	defer g.Close()

	conn, err := net.Dial("tcp", g.Addr().String())
	if err != nil {
		t.Fatalf("dial gateway TCP: %v", err)
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

// ── gateway mode tests ────────────────────────────────────────────────────────

// TestGatewayMode_Start_NoProbeDial verifies that gateway mode (proxy == "")
// starts without requiring any upstream — no probe-dial — and binds a TCP port.
func TestGatewayMode_Start_NoProbeDial(t *testing.T) {
	g := NewGateway("", "") // gateway mode: no upstream
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start (gateway mode): %v", err)
	}
	t.Cleanup(func() { g.Close() })

	// Verify Port() is non-zero.
	if g.Port() == 0 {
		t.Error("Port() is 0 in gateway mode — listener did not bind")
	}
}

// TestGatewayMode_CONNECT_TunnelToDirectTarget verifies that a CONNECT request
// over TCP is forwarded directly to a local TCP target and that bytes round-trip
// correctly.
func TestGatewayMode_CONNECT_TunnelToDirectTarget(t *testing.T) {
	targetAddr := startFakeTCPTarget(t)

	g := NewGateway("", "")
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start (gateway mode): %v", err)
	}
	defer g.Close()

	// Open a raw TCP connection to the gateway.
	conn, err := net.Dial("tcp", g.Addr().String())
	if err != nil {
		t.Fatalf("dial gateway TCP: %v", err)
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
	// Start a local HTTP server that serves a known response.
	const wantBody = "hello from origin"
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, wantBody)
	}))
	defer origin.Close()

	g := NewGateway("", "")
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start (gateway mode): %v", err)
	}
	defer g.Close()

	// Configure an http.Client that sends all requests through the gateway's TCP address.
	client := dialTCPHTTP(g.Addr().String())
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

// TestGatewayMode_Close_IsIdempotent verifies that Close is idempotent in
// gateway mode (calling it multiple times does not deadlock or error).
func TestGatewayMode_Close_IsIdempotent(t *testing.T) {
	g := NewGateway("", "")
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start (gateway mode): %v", err)
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
		t.Fatal("Close is not idempotent in gateway mode — deadlock detected")
	}
}

// TestGatewayMode_CONNECT_UnreachableTarget_Returns502 verifies that a CONNECT
// request to an unreachable target returns 502 Bad Gateway.
func TestGatewayMode_CONNECT_UnreachableTarget_Returns502(t *testing.T) {
	g := NewGateway("", "")
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start (gateway mode): %v", err)
	}
	defer g.Close()

	conn, err := net.Dial("tcp", g.Addr().String())
	if err != nil {
		t.Fatalf("dial gateway TCP: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	// Target port 1 is never open — DialContext will fail immediately.
	fmt.Fprintf(conn, "CONNECT 127.0.0.1:1 HTTP/1.1\r\nHost: 127.0.0.1:1\r\n\r\n")

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("CONNECT to unreachable target: status = %d, want 502", resp.StatusCode)
	}
}

// TestGatewayMode_PlainHTTP_RoundTripFailure_Returns502 verifies that when the
// upstream origin refuses the connection, ServeHTTP returns 502 Bad Gateway.
func TestGatewayMode_PlainHTTP_RoundTripFailure_Returns502(t *testing.T) {
	g := NewGateway("", "")
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start (gateway mode): %v", err)
	}
	defer g.Close()

	client := dialTCPHTTP(g.Addr().String())
	client.Timeout = 5 * time.Second

	// Port 1 is never open — RoundTrip will fail and ServeHTTP must return 502.
	resp, err := client.Get("http://127.0.0.1:1/test")
	if err != nil {
		t.Fatalf("GET via gateway: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("RoundTrip failure: status = %d, want 502", resp.StatusCode)
	}
}

// ── request logging tests ─────────────────────────────────────────────────────

// TestGatewayLogging_CONNECT verifies that a CONNECT request writes a log line.
func TestGatewayLogging_CONNECT(t *testing.T) {
	logFile := fmt.Sprintf("/tmp/ms_test_log_connect_%d.log", os.Getpid())
	t.Cleanup(func() { os.Remove(logFile) })
	targetAddr := startFakeTCPTarget(t)

	g := NewGateway("", logFile)
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer g.Close()

	conn, err := net.Dial("tcp", g.Addr().String())
	if err != nil {
		t.Fatalf("dial gateway TCP: %v", err)
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
	logFile := fmt.Sprintf("/tmp/ms_test_log_get_%d.log", os.Getpid())
	t.Cleanup(func() { os.Remove(logFile) })

	const wantBody = "ok"
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, wantBody)
	}))
	defer origin.Close()

	g := NewGateway("", logFile)
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer g.Close()

	client := dialTCPHTTP(g.Addr().String())
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
	// Assert both method and target URL, not just the method, so a log line
	// with an empty target would be caught.
	wantLogFrag := "GET " + origin.URL + "/logtest"
	if !strings.Contains(string(data), wantLogFrag) {
		t.Errorf("log does not contain %q; got:\n%s", wantLogFrag, data)
	}
}

// TestUpstreamLogging_RequestLineLogged verifies that upstream mode logs the
// first request line AND forwards bytes verbatim to the fake upstream.
func TestUpstreamLogging_RequestLineLogged(t *testing.T) {
	logFile := fmt.Sprintf("/tmp/ms_test_log_upstream_%d.log", os.Getpid())
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
		// Accept the probe connection first (from Start's DialContext probe-dial
		// in upstream mode). The probe is performed before Start returns, so by
		// the time the goroutine below dials the gateway TCP address, the probe
		// connection has already been accepted here and the second Accept is
		// ready for the actual data connection. This ordering is guaranteed by
		// the sequential structure of Start: probe-dial succeeds → Start returns
		// → test dials.
		c2, err := ln.Accept()
		if err != nil {
			return
		}
		defer c2.Close()
		data, _ := io.ReadAll(c2)
		received <- data
	}()

	upstreamAddr := ln.Addr().String()
	g := NewGateway(upstreamAddr, logFile)
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer g.Close()

	conn, err := net.Dial("tcp", g.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
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

	// Verify log entry. The log line format is "<METHOD> <target>", so we check
	// for the exact method+URL pair (not just the method) so that an entry with
	// an empty or truncated target would be caught.
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	logStr := string(data)
	if !strings.Contains(logStr, "GET http://example.com/") {
		t.Errorf("log does not contain 'GET http://example.com/'; got:\n%s", logStr)
	}
}

// TestUpstreamLogging_Off verifies that when logging is off the upstream path
// forwards bytes unchanged and no log file is created.
func TestUpstreamLogging_Off(t *testing.T) {
	upstreamAddr := startFakeUpstream(t)

	// logPath = "" → no logging
	g := NewGateway(upstreamAddr, "")
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer g.Close()

	conn, err := net.Dial("tcp", g.Addr().String())
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

// TestStart_FailLoud_BadLogPath verifies that Start fails when logPath points
// to a non-existent directory.
func TestStart_FailLoud_BadLogPath(t *testing.T) {
	// Use a path inside a non-existent directory — OpenFile will fail.
	badLogPath := "/tmp/nonexistent_dir_makeslop_test/request.log"

	g := NewGateway("", badLogPath)
	err := g.Start(context.Background())
	if err == nil {
		g.Close()
		t.Fatal("Start: expected error for bad logPath, got nil")
	}

	// After a failed Start, Addr() must return nil.
	if g.Addr() != nil {
		t.Error("Addr() should be nil after failed Start")
	}
}

// TestStart_UpstreamMode_LogPath_ProbeFail_NoFDLeak verifies that when upstream
// mode is used with a logPath set and the probe-dial fails, the log file opened
// before the probe-dial is properly closed on the error path (no fd leak).
//
// This is the critical case identified in the code review: Start opens g.logFile
// before branching into upstream mode, so a probe-dial failure must close it.
func TestStart_UpstreamMode_LogPath_ProbeFail_NoFDLeak(t *testing.T) {
	logFile := fmt.Sprintf("/tmp/ms_test_log_probe_fail_%d.log", os.Getpid())
	t.Cleanup(func() { os.Remove(logFile) })

	// port 1 is never open — probe-dial will fail immediately.
	g := NewGateway("127.0.0.1:1", logFile)
	err := g.Start(context.Background())
	if err == nil {
		g.Close()
		t.Fatal("Start: expected error for unreachable upstream, got nil")
	}

	// g.logFile must be nil after the error path — the file was opened and then
	// closed before returning the error.
	if g.logFile != nil {
		_ = g.logFile.Close() // prevent actual fd leak during test
		t.Error("g.logFile must be nil after Start failure (fd was not closed on error path)")
	}

	// After a failed Start, Addr() must return nil.
	if g.Addr() != nil {
		t.Error("Addr() should be nil after failed Start")
	}
}

// TestGatewayMode_Close_TearDownInFlightTunnel verifies that Close tears down
// an in-flight CONNECT tunnel (the hijacked conn is tracked and force-closed)
// and that Close does not hang.
func TestGatewayMode_Close_TearDownInFlightTunnel(t *testing.T) {
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

	g := NewGateway("", "")
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("Start (gateway mode): %v", err)
	}

	// Establish a CONNECT tunnel.
	conn, err := net.Dial("tcp", g.Addr().String())
	if err != nil {
		t.Fatalf("dial gateway TCP: %v", err)
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
}

// TestLogFirstLine_MalformedInputs verifies that logFirstLine handles edge-case
// inputs (single token, empty line, whitespace-only) without panicking and
// logs a reasonable raw line rather than a structured method+target pair.
func TestLogFirstLine_MalformedInputs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only per CLAUDE.md")
	}
	cases := []struct {
		name  string
		input string
	}{
		{"single_token", "CONNECT\n"},
		{"empty_line", "\n"},
		{"whitespace_only", "   \n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logPath := fmt.Sprintf("/tmp/ms_logfirstline_%s_%d.log", tc.name, os.Getpid())
			t.Cleanup(func() { os.Remove(logPath) })

			f, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
			if err != nil {
				t.Fatalf("open log file: %v", err)
			}
			g := &Gateway{
				logFile: f,
				logger:  log.New(f, "", 0),
			}
			// Must not panic on malformed input.
			g.logFirstLine(tc.input)
			f.Close()

			data, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("read log file: %v", err)
			}
			// For non-whitespace-only inputs something must have been written.
			if strings.TrimSpace(tc.input) != "" && len(strings.TrimSpace(string(data))) == 0 {
				t.Errorf("logFirstLine wrote nothing for non-empty input %q", tc.input)
			}
		})
	}
}
