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
	if !bytes.HasPrefix(Dockerfile, []byte("FROM ")) {
		t.Fatalf("assets.Dockerfile does not start with \"FROM \"; got prefix %q", Dockerfile[:min(20, len(Dockerfile))])
	}
}
