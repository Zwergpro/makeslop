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
	// A valid Dockerfile may begin with a "# syntax=" directive before FROM.
	hasFROM := bytes.Contains(Dockerfile, []byte("\nFROM ")) ||
		bytes.HasPrefix(Dockerfile, []byte("FROM "))
	if !hasFROM {
		t.Fatalf("assets.Dockerfile does not contain a FROM instruction; got prefix %q", Dockerfile[:min(40, len(Dockerfile))])
	}
}

// Regression guard: the Dockerfile must resolve arch via dpkg, not hard-code
// amd64, or arm64 / Apple Silicon builds break.
func TestDockerfile_MultiArchDpkgPattern(t *testing.T) {
	if !bytes.Contains(Dockerfile, []byte("dpkg --print-architecture")) {
		t.Fatal("Dockerfile does not contain 'dpkg --print-architecture' — multi-arch arch resolution missing")
	}
	if !bytes.Contains(Dockerfile, []byte("linux-${ARCH}")) {
		t.Fatal("Dockerfile does not reference ${ARCH} in the Go tarball URL — arch variable not used")
	}
}
