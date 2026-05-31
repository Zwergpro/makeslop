package assets

import (
	"bytes"
	"testing"
)

func TestDockerfile_NotEmpty(t *testing.T) {
	if len(Dockerfile) == 0 {
		t.Fatal("assets.Dockerfile is empty — embed failed")
	}
}

func TestDockerfile_StartsWithFROM(t *testing.T) {
	// A valid Dockerfile may begin with a BuildKit syntax directive
	// (# syntax=docker/...) before the first FROM instruction.
	hasFROM := bytes.Contains(Dockerfile, []byte("\nFROM ")) ||
		bytes.HasPrefix(Dockerfile, []byte("FROM "))
	if !hasFROM {
		t.Fatalf("assets.Dockerfile does not contain a FROM instruction; got prefix %q", Dockerfile[:min(40, len(Dockerfile))])
	}
}

// TestDockerfile_MultiArchDpkgPattern verifies that the embedded Dockerfile
// uses dpkg --print-architecture to resolve the Go tarball arch at build time
// (multi-arch support added in MigrationVersion 2). A regression here would
// hard-code amd64 and break arm64 / Apple Silicon builds.
func TestDockerfile_MultiArchDpkgPattern(t *testing.T) {
	if !bytes.Contains(Dockerfile, []byte("dpkg --print-architecture")) {
		t.Fatal("Dockerfile does not contain 'dpkg --print-architecture' — multi-arch arch resolution missing")
	}
	// The ARCH variable must be referenced in the tarball URL.
	if !bytes.Contains(Dockerfile, []byte("linux-${ARCH}")) {
		t.Fatal("Dockerfile does not reference ${ARCH} in the Go tarball URL — arch variable not used")
	}
}
