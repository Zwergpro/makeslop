package docker

import (
	"context"
	"errors"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	moby "github.com/moby/moby/client"
)

// TestAPIClientSatisfied verifies at compile time (and at test time) that
// *moby.Client satisfies our narrow apiClient interface. The compile-time
// assertion in client.go is the real guard; this test makes the fact explicit
// and visible in test output.
func TestAPIClientSatisfied(t *testing.T) {
	// The compile-time assertion `var _ apiClient = (*moby.Client)(nil)` in
	// client.go already enforces this. The test below is redundant in that
	// sense but documents the intent and fails fast at runtime if the build
	// constraint was somehow bypassed.
	var c interface{} = (*moby.Client)(nil)
	if _, ok := c.(apiClient); !ok {
		t.Fatal("*moby.Client does not satisfy apiClient interface")
	}
}

// TestNewClientReturnsNonNil verifies that newClient() returns a non-nil
// client when called without a real daemon. The connection is lazy so this
// works even when DOCKER_HOST is unset or points at a non-existent socket.
func TestNewClientReturnsNonNil(t *testing.T) {
	c, err := newClient()
	if err != nil {
		t.Fatalf("newClient() returned error: %v", err)
	}
	if c == nil {
		t.Fatal("newClient() returned nil client")
	}
	// Close is a no-op when no connection has been established.
	_ = c.Close()
}

// TestNoopClientPingOK verifies the noopClient Ping returns success.
func TestNoopClientPingOK(t *testing.T) {
	var n noopClient
	_, err := n.Ping(context.Background(), moby.PingOptions{})
	if err != nil {
		t.Fatalf("noopClient.Ping() returned error: %v", err)
	}
}

// TestNoopClientImageInspectFound verifies the noopClient ImageInspect returns
// a result (image "found") with no error.
func TestNoopClientImageInspectFound(t *testing.T) {
	var n noopClient
	_, err := n.ImageInspect(context.Background(), "myimage")
	if err != nil {
		t.Fatalf("noopClient.ImageInspect() returned error: %v", err)
	}
}

// TestFakeRunClientPingScripting verifies that fakeRunClient returns the
// scripted PingErr when set, and success otherwise.
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

// TestFakeRunClientImageInspectScripting verifies the image-present,
// image-missing, and other-error cases.
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

// TestFakeBuildClientPingScripting verifies fakeBuildClient ping scripting.
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

// TestFakeBuildClientImageInspectScripting verifies image scripting on
// fakeBuildClient: present, missing (not-found), and other-error cases.
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
