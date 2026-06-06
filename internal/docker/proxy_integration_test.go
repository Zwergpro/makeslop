//go:build integration

// Package docker — proxy integration test that requires a live Docker daemon.
//
// This test verifies whether HTTP_PROXY=unix:///sockets/proxy.sock is honored
// by clients inside the container image, exercising the full socat-volume proxy
// path end-to-end.
//
// Run with:
//
//	MAKESLOP_DOCKER_IT=1 go test -tags integration ./internal/docker/ -run TestProxy
//
// Skip-on-missing-daemon: if MAKESLOP_DOCKER_IT is not set, tests skip rather
// than fail (suitable for CI that has no daemon reachable).
//
// # Investigation result: unix:// proxy URLs are non-standard
//
// The unix:// URL scheme for HTTP_PROXY is NOT part of any standard (RFC 7235,
// curl manual, or Go net/http). Support is client-library-dependent:
//
//   - curl (7.x+): does NOT natively support unix:// for HTTP_PROXY. The
//     unix:// scheme is only supported for the target URL (--unix-socket), not
//     for proxy configuration via env vars.
//   - Go net/http: does NOT support unix:// in HTTP_PROXY; it parses the proxy
//     URL via url.Parse and dials via net.Dial, which does not recognize unix://
//     as a proxy scheme. Requests bypass the proxy silently.
//   - Python requests: does NOT honor unix:// proxy URLs.
//
// CONSEQUENCE: HTTP_PROXY=unix:///sockets/proxy.sock will be silently ignored
// by most HTTP clients. With --network none, those clients will get a
// "Network is unreachable" error.
//
// RECOMMENDED FOLLOW-UP (Post-Completion):
// Redesign the proxy transport to use a shared internal Docker network with the
// sidecar listening on TCP (TCP-LISTEN), setting HTTP_PROXY=http://sidecar:port
// — a standard URL that all HTTP clients honor uniformly. This is tracked as a
// Post-Completion item in docs/plans/20260606-architecture-fixes.md.
//
// # What this file provides
//
// 1. TestProxy_Integration_UnixSchemeVerification — a live daemon test that
//    documents whether curl in curlimages/curl:latest honors unix:// for HTTP_PROXY.
//    This test RECORDS the outcome (does not fail on "not honored") so operators
//    can confirm the limitation.
//
// 2. TestProxy_Unit_BuildSpecContract — a gated unit assertion that BuildSpec
//    emits unix:// env vars + --network none when ProxySocketVolume is set.
//    (Non-gated equivalents live in spec_test.go; this test exercises the same
//    contract in the integration-tagged file for completeness.)
package docker

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	moby "github.com/moby/moby/client"
)

// proxyTestTimeout is the maximum time allowed for the proxy integration test.
const proxyTestTimeout = 120 * time.Second

// TestProxy_Integration_UnixSchemeVerification starts a throwaway HTTP proxy
// listener, launches the socat sidecar, runs a container in proxy mode
// (--network none, HTTP_PROXY=unix:///sockets/proxy.sock) with curl, and
// records whether the unix:// proxy env var is honored by curl.
//
// This is a documentation/verification test. The outcome is printed to the
// test log regardless of pass/fail. The test fails only on infrastructure
// errors (sidecar won't start, daemon unreachable, etc.) — not on "curl
// ignored the proxy", which is the expected result documented in the package
// comment above.
func TestProxy_Integration_UnixSchemeVerification(t *testing.T) {
	if os.Getenv("MAKESLOP_DOCKER_IT") == "" {
		t.Skip("set MAKESLOP_DOCKER_IT=1 to run integration tests against a live daemon")
	}

	ctx, cancel := context.WithTimeout(context.Background(), proxyTestTimeout)
	defer cancel()

	// ── Step 1: start a minimal HTTP proxy listener on the host ──────────────
	//
	// We listen on 127.0.0.1:0 and serve a minimal HTTP proxy that records
	// whether it was contacted. The socat sidecar will forward unix socket →
	// TCP-CONNECT to this address over bridge networking.
	//
	// On native Linux, 127.0.0.1 is reachable from a bridge-networked container
	// via the host's loopback. This test targets Linux CI.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for proxy: %v", err)
	}
	defer listener.Close() //nolint:errcheck

	proxyAddr := listener.Addr().String()
	t.Logf("throwaway proxy listening at %s", proxyAddr)

	// Track whether the proxy was contacted.
	proxyCalled := make(chan struct{}, 10)

	proxyServer := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			select {
			case proxyCalled <- struct{}{}:
			default:
			}
			// Return 502 so curl exits non-zero but we detect the contact.
			http.Error(w, "proxy contacted (test stub)", http.StatusBadGateway)
		}),
	}
	go proxyServer.Serve(listener) //nolint:errcheck
	defer proxyServer.Shutdown(context.Background()) //nolint:errcheck

	// ── Step 2: start the socat sidecar against the host proxy ───────────────
	volName := fmt.Sprintf("makeslop-proxy-it-%d", os.Getpid())
	sc := NewSidecar(false, os.Stderr)
	if err := sc.Start(ctx, proxyAddr, volName); err != nil {
		t.Fatalf("sidecar Start: %v — is %s available? (docker pull %s)", err, SocatImage, SocatImage)
	}
	defer func() {
		if cerr := sc.Close(); cerr != nil {
			t.Logf("sidecar Close (best-effort): %v", cerr)
		}
	}()

	// ── Step 3: check that alpine:latest is locally present ───────────────────
	//
	// We do not pull images in integration tests; if the image is absent, skip.
	const testImage = "curlimages/curl:latest"
	cli, err := newClientFn()
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close() //nolint:errcheck

	if _, inspectErr := cli.ImageInspect(ctx, testImage); inspectErr != nil {
		t.Skipf("%s not present locally (skipping proxy verification): %v", testImage, inspectErr)
	}

	// ── Step 4: run a container in proxy mode with curl ───────────────────────
	//
	// The container uses --network none and HTTP_PROXY=unix:///sockets/proxy.sock.
	// curl is asked to fetch http://example.com/ through the proxy.
	//
	// If curl honors unix:// for HTTP_PROXY, it routes via socat → our listener
	// (proxyRequests > 0). If curl does NOT honor unix://, it tries a direct
	// TCP connect (which fails with --network none) and our listener is never
	// contacted (proxyRequests == 0).
	proxyEnvURL := "unix://" + proxySocketDir + "/" + proxySocketName

	createRes, err := cli.ContainerCreate(ctx, moby.ContainerCreateOptions{
		Config: &container.Config{
			Image: testImage,
			// curl --max-time 5: prevents hanging if curl ignores the proxy and tries DNS.
			// curlimages/curl has curl as its entrypoint; only args are passed here.
			Cmd: []string{
				"--silent", "--show-error", "--max-time", "5",
				"http://example.com/",
			},
			Env: []string{
				"HTTP_PROXY=" + proxyEnvURL,
				"HTTPS_PROXY=" + proxyEnvURL,
			},
		},
		HostConfig: &container.HostConfig{
			NetworkMode: container.NetworkMode("none"),
			AutoRemove:  false, // we remove manually after inspecting the exit code
			Mounts: []mount.Mount{
				{
					Type:     mount.TypeVolume,
					Source:   volName,
					Target:   proxySocketDir,
					ReadOnly: true,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	containerID := createRes.ID
	defer func() {
		_, _ = cli.ContainerRemove(
			context.Background(), containerID,
			moby.ContainerRemoveOptions{Force: true},
		)
	}()

	if _, err := cli.ContainerStart(ctx, containerID, moby.ContainerStartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}

	waitRes := cli.ContainerWait(ctx, containerID, moby.ContainerWaitOptions{
		Condition: container.WaitConditionNotRunning,
	})
	var exitCode int64
	select {
	case res := <-waitRes.Result:
		exitCode = res.StatusCode
	case waitErr := <-waitRes.Error:
		t.Fatalf("ContainerWait: %v", waitErr)
	case <-ctx.Done():
		t.Fatalf("ContainerWait timed out: %v", ctx.Err())
	}

	// ── Step 5: give the proxy a moment to receive any in-flight connection ───
	// (connections may arrive slightly after the container exits)
	time.Sleep(200 * time.Millisecond)

	// ── Step 6: record and document the outcome ───────────────────────────────
	wasProxyCalled := len(proxyCalled) > 0

	t.Logf("=== proxy unix:// verification result ===")
	t.Logf("proxy env var: HTTP_PROXY=%s", proxyEnvURL)
	t.Logf("curl exit code: %d", exitCode)
	t.Logf("proxy listener was contacted: %v", wasProxyCalled)

	if wasProxyCalled {
		t.Logf("RESULT: unix:// IS honored by curl as HTTP_PROXY — proxy mode functional")
	} else {
		// Expected outcome: curl does not support unix:// for HTTP_PROXY.
		// We log it but do NOT fail the test — the purpose is documentation.
		t.Logf("RESULT: unix:// is NOT honored by curl as HTTP_PROXY (expected)")
		t.Logf("  Clients with --network none that ignore unix:// proxy env vars will")
		t.Logf("  get 'Network is unreachable'. See package doc for follow-up plan.")
	}

	// The test passes regardless of whether the proxy was contacted.
	// Infrastructure errors (sidecar, image, daemon) cause t.Fatal above.
}

// TestProxy_Unit_BuildSpecContract asserts that BuildSpec emits the unix://
// proxy env vars and --network none when ProxySocketVolume is set.
// This guards the spec contract regardless of daemon availability.
// Equivalent assertions live in spec_test.go (non-integration-tagged);
// this copy runs in the integration build to exercise the contract alongside
// the live verification test without requiring a live daemon.
func TestProxy_Unit_BuildSpecContract(t *testing.T) {
	o := Options{
		ProjectRoot:       "/home/me/code/myproj",
		WorkspaceName:     "myproj-abc123",
		BaseDir:           "/home/me/.makeslop",
		Image:             "claudebox",
		Command:           "/bin/zsh",
		TmpDirSize:        "100m",
		MountAgentCache:   true,
		MountContentCache: true,
		ProxySocketVolume: "makeslop-sock-abc123-42",
	}
	spec := BuildSpec(o)

	// Network mode must be "none".
	if spec.NetworkMode != "none" {
		t.Errorf("NetworkMode = %q, want \"none\"", spec.NetworkMode)
	}

	// Env must contain unix:// proxy URLs.
	wantHTTP := "HTTP_PROXY=unix:///sockets/proxy.sock"
	wantHTTPS := "HTTPS_PROXY=unix:///sockets/proxy.sock"
	foundHTTP, foundHTTPS := false, false
	for _, e := range spec.Env {
		switch e {
		case wantHTTP:
			foundHTTP = true
		case wantHTTPS:
			foundHTTPS = true
		}
	}
	if !foundHTTP {
		t.Errorf("Env missing %q; got %v", wantHTTP, spec.Env)
	}
	if !foundHTTPS {
		t.Errorf("Env missing %q; got %v", wantHTTPS, spec.Env)
	}

	// Args must contain --network none and the proxy env flags.
	args := spec.Args()
	foundNetwork := false
	foundHTTPArg := false
	foundHTTPSArg := false
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--network" && args[i+1] == "none" {
			foundNetwork = true
		}
		if args[i] == "-e" && args[i+1] == wantHTTP {
			foundHTTPArg = true
		}
		if args[i] == "-e" && args[i+1] == wantHTTPS {
			foundHTTPSArg = true
		}
	}
	if !foundNetwork {
		t.Errorf("Args missing --network none; args: %v", args)
	}
	if !foundHTTPArg {
		t.Errorf("Args missing -e %s; args: %v", wantHTTP, args)
	}
	if !foundHTTPSArg {
		t.Errorf("Args missing -e %s; args: %v", wantHTTPS, args)
	}

	// Last mount must be the proxy volume, read-only.
	if len(spec.Mounts) == 0 {
		t.Fatal("no mounts in spec")
	}
	last := spec.Mounts[len(spec.Mounts)-1]
	if last.Type != "volume" {
		t.Errorf("last mount type = %q, want \"volume\"", last.Type)
	}
	if last.Host != "makeslop-sock-abc123-42" {
		t.Errorf("last mount Host = %q, want \"makeslop-sock-abc123-42\"", last.Host)
	}
	if last.Container != "/sockets" {
		t.Errorf("last mount Container = %q, want \"/sockets\"", last.Container)
	}
	if !last.ReadOnly {
		t.Error("proxy volume mount must be ReadOnly")
	}
}
