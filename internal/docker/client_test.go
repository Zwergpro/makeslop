package docker

import (
	"context"
	"errors"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	moby "github.com/moby/moby/client"
)

// Makes the client.go compile-time assertion (*moby.Client satisfies apiClient)
// explicit and visible in test output.
func TestAPIClientSatisfied(t *testing.T) {
	var c interface{} = (*moby.Client)(nil)
	if _, ok := c.(apiClient); !ok {
		t.Fatal("*moby.Client does not satisfy apiClient interface")
	}
}

// newClient() works without a real daemon — the connection is lazy, so it
// succeeds even when DOCKER_HOST is unset or points at a missing socket.
func TestNewClientReturnsNonNil(t *testing.T) {
	c, err := newClient()
	if err != nil {
		t.Fatalf("newClient() returned error: %v", err)
	}
	if c == nil {
		t.Fatal("newClient() returned nil client")
	}
	_ = c.Close()
}

func TestNoopClientPingOK(t *testing.T) {
	var n noopClient
	_, err := n.Ping(context.Background(), moby.PingOptions{})
	if err != nil {
		t.Fatalf("noopClient.Ping() returned error: %v", err)
	}
}

func TestNoopClientImageInspectFound(t *testing.T) {
	var n noopClient
	_, err := n.ImageInspect(context.Background(), "myimage")
	if err != nil {
		t.Fatalf("noopClient.ImageInspect() returned error: %v", err)
	}
}

func TestFakeRunClientPingScripting(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		f := newFakeRunClient(0)
		_, err := f.Ping(context.Background(), moby.PingOptions{})
		if err != nil {
			t.Fatalf("expected ping success, got: %v", err)
		}
	})
	t.Run("daemon-down", func(t *testing.T) {
		f := newFakeRunClient(0)
		f.PingErr = errors.New("connection refused")
		_, err := f.Ping(context.Background(), moby.PingOptions{})
		if err == nil {
			t.Fatal("expected ping error, got nil")
		}
	})
}

func TestFakeRunClientImageInspectScripting(t *testing.T) {
	t.Run("image-present", func(t *testing.T) {
		f := newFakeRunClient(0)
		_, err := f.ImageInspect(context.Background(), "myimage")
		if err != nil {
			t.Fatalf("expected image found, got: %v", err)
		}
	})
	t.Run("image-missing returns not-found", func(t *testing.T) {
		f := newFakeRunClient(0)
		f.ImageMissing = true
		_, err := f.ImageInspect(context.Background(), "myimage")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !cerrdefs.IsNotFound(err) {
			t.Fatalf("expected cerrdefs.IsNotFound to be true, got error: %v", err)
		}
	})
	t.Run("image other-error propagates", func(t *testing.T) {
		f := newFakeRunClient(0)
		f.ImageErr = errors.New("permission denied")
		_, err := f.ImageInspect(context.Background(), "myimage")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if cerrdefs.IsNotFound(err) {
			t.Fatal("other-error should NOT be classified as not-found")
		}
	})
}

func TestFakeBuildClientPingScripting(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		f := newFakeBuildClient(0)
		_, err := f.Ping(context.Background(), moby.PingOptions{})
		if err != nil {
			t.Fatalf("expected ping success, got: %v", err)
		}
	})
	t.Run("daemon-down", func(t *testing.T) {
		f := newFakeBuildClient(0)
		f.PingErr = errors.New("connection refused")
		_, err := f.Ping(context.Background(), moby.PingOptions{})
		if err == nil {
			t.Fatal("expected ping error, got nil")
		}
	})
}

func TestFakeBuildClientImageInspectScripting(t *testing.T) {
	t.Run("image-present", func(t *testing.T) {
		f := newFakeBuildClient(0)
		_, err := f.ImageInspect(context.Background(), "myimage")
		if err != nil {
			t.Fatalf("expected image found, got: %v", err)
		}
	})
	t.Run("image-missing returns not-found", func(t *testing.T) {
		f := newFakeBuildClient(0)
		f.ImageMissing = true
		_, err := f.ImageInspect(context.Background(), "myimage")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !cerrdefs.IsNotFound(err) {
			t.Fatalf("expected cerrdefs.IsNotFound to be true, got error: %v", err)
		}
	})
	t.Run("image other-error propagates", func(t *testing.T) {
		f := newFakeBuildClient(0)
		f.ImageErr = errors.New("permission denied")
		_, err := f.ImageInspect(context.Background(), "myimage")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if cerrdefs.IsNotFound(err) {
			t.Fatal("other-error should NOT be classified as not-found")
		}
	})
}
