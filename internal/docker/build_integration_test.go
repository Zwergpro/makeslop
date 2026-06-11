//go:build integration

// Integration tests requiring a live Docker daemon. Run with:
//
//	MAKESLOP_DOCKER_IT=1 go test -tags integration ./internal/docker/
//
// Tests skip (not fail) when MAKESLOP_DOCKER_IT is unset, so CI without a daemon passes.
package docker

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// Runs a real Build against a live daemon with a minimal context-free Dockerfile.
func TestBuild_Integration_BuildKit(t *testing.T) {
	if os.Getenv("MAKESLOP_DOCKER_IT") == "" {
		t.Skip("set MAKESLOP_DOCKER_IT=1 to run integration tests against a live daemon")
	}

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
	d, err := New()
	if err != nil {
		t.Fatalf("New() returned error: %v", err)
	}
	defer d.Close() //nolint:errcheck
	if err := d.Build(ctx, o, io.Discard); err != nil {
		t.Fatalf("Build returned unexpected error: %v", err)
	}
}
