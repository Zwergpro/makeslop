package docker

import (
	"testing"

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
