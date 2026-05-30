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
