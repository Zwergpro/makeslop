package docker

// fakes_test.go — test-only constructor helpers for the docker package.
//
// The fake types (noopClient, FakeRunClient, FakeBuildClient, SkipNonPOSIX)
// still live in testing.go for now (they are used by cmd/makeslop tests via
// exported names). This file provides constructor helpers that build *Docker
// instances with injected fakes, used by the migrated internal/docker tests.
//
// When Task 4 migrates cmd/makeslop tests away from testing.go and Task 5
// deletes testing.go, the type definitions will move here.

import (
	"testing"

	"golang.org/x/term"
)

// newDockerWithClient constructs a *Docker with the given fake apiClient
// injected via WithClient. Registers d.Close via t.Cleanup.
func newDockerWithClient(t *testing.T, c apiClient, opts ...Option) *Docker {
	t.Helper()
	allOpts := append([]Option{WithClient(c)}, opts...)
	d, err := New(allOpts...)
	if err != nil {
		t.Fatalf("New(WithClient(fake)): %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// noopMakeRaw is a WithRawMode stub that returns (nil, nil) — safe to use in
// tests without a real PTY (term.Restore with nil state is a no-op).
func noopMakeRaw(_ int) (*term.State, error) { return nil, nil }

// alwaysTTY is a WithTTYCheck stub that always returns true.
func alwaysTTY() bool { return true }

// neverTTY is a WithTTYCheck stub that always returns false.
func neverTTY() bool { return false }
