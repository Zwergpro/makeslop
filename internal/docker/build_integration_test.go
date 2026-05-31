//go:build integration

// Package docker — integration tests that require a live Docker daemon.
//
// Run with:
//
//	MAKESLOP_DOCKER_IT=1 go test -tags integration ./internal/docker/
//
// Skip-on-missing-daemon: if MAKESLOP_DOCKER_IT is not set, tests skip
// rather than fail (suitable for CI that has no daemon reachable).
package docker

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestBuild_Integration_BuildKit runs a real `makeslop build` against a live
// daemon. It creates a minimal Dockerfile (no COPY, just FROM scratch) and
// verifies that Build completes without error.
func TestBuild_Integration_BuildKit(t *testing.T) {
	if os.Getenv("MAKESLOP_DOCKER_IT") == "" {
		t.Skip("set MAKESLOP_DOCKER_IT=1 to run integration tests against a live daemon")
	}

	// Write a minimal Dockerfile that doesn't require any context files.
	dir := t.TempDir()
	dockerfilePath := filepath.Join(dir, "Dockerfile")
	content := "FROM scratch\nLABEL test=makeslop-integration\n"
	if err := os.WriteFile(dockerfilePath, []byte(content), 0o644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}

	o := BuildOptions{
		Image:          "makeslop-integration-test:latest",
		DockerfilePath: dockerfilePath,
		// ContextDir empty: Build will create and remove a temp dir.
	}

	ctx := context.Background()
	if err := Build(ctx, o, io.Discard, io.Discard); err != nil {
		t.Fatalf("Build returned unexpected error: %v", err)
	}
}
