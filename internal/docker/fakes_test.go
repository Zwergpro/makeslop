package docker

// fakes_test.go — test-only helpers and fake types for the docker package.
//
// This file provides:
//   - noopClient: a no-op stub implementing all apiClient methods.
//   - fakeRunClient/newFakeRunClient: fake client for Run tests (scripted exit code,
//     TTY lifecycle, BlockPing/BlockImageInspect, etc.).
//   - fakeBuildClient/newFakeBuildClient: fake client for Build tests.
//   - newDockerWithClient: constructor helper that builds a *Docker with an
//     injected fake via WithClient.
//   - noopMakeRaw, alwaysTTY, neverTTY: stub functions for option injection.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	moby "github.com/moby/moby/client"
	"golang.org/x/term"
)

// noopClient provides no-op stub implementations of all apiClient methods.
// fakeRunClient and fakeBuildClient embed it and override only the methods
// that carry meaningful test logic.
type noopClient struct{}

func (noopClient) ContainerCreate(_ context.Context, _ moby.ContainerCreateOptions) (moby.ContainerCreateResult, error) {
	return moby.ContainerCreateResult{}, nil
}

func (noopClient) ContainerAttach(_ context.Context, _ string, _ moby.ContainerAttachOptions) (moby.ContainerAttachResult, error) {
	return moby.ContainerAttachResult{}, nil
}

func (noopClient) ContainerStart(_ context.Context, _ string, _ moby.ContainerStartOptions) (moby.ContainerStartResult, error) {
	return moby.ContainerStartResult{}, nil
}

func (noopClient) ContainerWait(_ context.Context, _ string, _ moby.ContainerWaitOptions) moby.ContainerWaitResult {
	return moby.ContainerWaitResult{
		Result: make(chan container.WaitResponse),
		Error:  make(chan error),
	}
}

func (noopClient) ContainerResize(_ context.Context, _ string, _ moby.ContainerResizeOptions) (moby.ContainerResizeResult, error) {
	return moby.ContainerResizeResult{}, nil
}

func (noopClient) ContainerRemove(_ context.Context, _ string, _ moby.ContainerRemoveOptions) (moby.ContainerRemoveResult, error) {
	return moby.ContainerRemoveResult{}, nil
}

func (noopClient) ImageBuild(_ context.Context, _ io.Reader, _ moby.ImageBuildOptions) (moby.ImageBuildResult, error) {
	return moby.ImageBuildResult{Body: io.NopCloser(bytes.NewReader(nil))}, nil
}

func (noopClient) DialHijack(_ context.Context, _, _ string, _ map[string][]string) (net.Conn, error) {
	return nil, nil
}

// Ping returns a successful ping result by default.
func (noopClient) Ping(_ context.Context, _ moby.PingOptions) (moby.PingResult, error) {
	return moby.PingResult{}, nil
}

// ImageInspect returns a non-empty result (image "found") by default.
// The variadic opts are accepted and ignored.
func (noopClient) ImageInspect(_ context.Context, _ string, _ ...moby.ImageInspectOption) (moby.ImageInspectResult, error) {
	return moby.ImageInspectResult{}, nil
}

func (noopClient) Close() error { return nil }

// fakeRunClient is a test fake apiClient that instruments Run calls with a
// scripted exit code and records whether the container was started.
//
// To simulate daemon-down, set PingErr. To simulate a missing image, set
// ImageMissing = true. To simulate another image error, set ImageErr.
type fakeRunClient struct {
	noopClient
	ExitCode     int
	wasStarted   bool
	PingErr      error // if non-nil, Ping returns this error
	ImageMissing bool  // if true, ImageInspect returns a not-found error
	ImageErr     error // if non-nil (and ImageMissing false), ImageInspect returns this error

	// Container tracking
	RemovedContainers       []string
	LastContainerCreateOpts moby.ContainerCreateOptions // options from most recent ContainerCreate call
	ContainerCreateErr      error                       // if non-nil, ContainerCreate returns this error
	ContainerStartErr       error                       // if non-nil, ContainerStart returns this error

	// BlockPing, when true, causes Ping to block until ctx is cancelled, then
	// return ctx.Err(). This lets tests verify that a timeout deadline is
	// propagated through to the Ping call site.
	BlockPing bool

	// BlockImageInspect, when true, causes ImageInspect to block until ctx is
	// cancelled, then return ctx.Err(). This lets tests verify that a timeout
	// deadline is propagated through to the ImageInspect call site.
	BlockImageInspect bool
}

// newFakeRunClient creates a fakeRunClient that will return the given exit code
// from ContainerWait.
func newFakeRunClient(exitCode int) *fakeRunClient {
	return &fakeRunClient{ExitCode: exitCode}
}

// Ping returns PingErr if set, otherwise delegates to noopClient (success).
// If BlockPing is true, Ping blocks until ctx is cancelled and returns ctx.Err().
func (f *fakeRunClient) Ping(ctx context.Context, _ moby.PingOptions) (moby.PingResult, error) {
	if f.BlockPing {
		<-ctx.Done()
		return moby.PingResult{}, ctx.Err()
	}
	if f.PingErr != nil {
		return moby.PingResult{}, f.PingErr
	}
	return moby.PingResult{}, nil
}

// ImageInspect returns a not-found error when ImageMissing is true, ImageErr
// when ImageErr is set, or a found result otherwise.
// If BlockImageInspect is true, ImageInspect blocks until ctx is cancelled.
func (f *fakeRunClient) ImageInspect(ctx context.Context, imageID string, _ ...moby.ImageInspectOption) (moby.ImageInspectResult, error) {
	if f.BlockImageInspect {
		<-ctx.Done()
		return moby.ImageInspectResult{}, ctx.Err()
	}
	if f.ImageMissing {
		return moby.ImageInspectResult{}, fmt.Errorf("image %q: %w", imageID, errdefs.ErrNotFound)
	}
	if f.ImageErr != nil {
		return moby.ImageInspectResult{}, f.ImageErr
	}
	return moby.ImageInspectResult{}, nil
}

// ContainerRemove records the removed container ID.
func (f *fakeRunClient) ContainerRemove(_ context.Context, id string, _ moby.ContainerRemoveOptions) (moby.ContainerRemoveResult, error) {
	f.RemovedContainers = append(f.RemovedContainers, id)
	return moby.ContainerRemoveResult{}, nil
}

func (f *fakeRunClient) ContainerCreate(_ context.Context, opts moby.ContainerCreateOptions) (moby.ContainerCreateResult, error) {
	f.LastContainerCreateOpts = opts
	if f.ContainerCreateErr != nil {
		return moby.ContainerCreateResult{}, f.ContainerCreateErr
	}
	return moby.ContainerCreateResult{ID: "fake-id"}, nil
}

func (f *fakeRunClient) ContainerAttach(_ context.Context, _ string, _ moby.ContainerAttachOptions) (moby.ContainerAttachResult, error) {
	pr, pw := net.Pipe()
	go func() { _ = pw.Close() }()
	hr := moby.NewHijackedResponse(pr, "")
	return moby.ContainerAttachResult{HijackedResponse: hr}, nil
}

func (f *fakeRunClient) ContainerStart(_ context.Context, _ string, _ moby.ContainerStartOptions) (moby.ContainerStartResult, error) {
	if f.ContainerStartErr != nil {
		return moby.ContainerStartResult{}, f.ContainerStartErr
	}
	f.wasStarted = true
	return moby.ContainerStartResult{}, nil
}

func (f *fakeRunClient) ContainerWait(_ context.Context, _ string, _ moby.ContainerWaitOptions) moby.ContainerWaitResult {
	resultC := make(chan container.WaitResponse, 1)
	errC := make(chan error, 1)
	resultC <- container.WaitResponse{StatusCode: int64(f.ExitCode)}
	return moby.ContainerWaitResult{Result: resultC, Error: errC}
}

// fakeBuildClient is a test fake apiClient for Build tests. It records the
// ImageBuildOptions passed to ImageBuild and returns a scripted result.
//
// To simulate daemon-down, set PingErr. To simulate a missing image, set
// ImageMissing = true. To simulate another image error, set ImageErr.
type fakeBuildClient struct {
	noopClient
	// ExitCode is returned as an error from ImageBuild when non-zero.
	ExitCode int
	// Err overrides ExitCode if non-nil: ImageBuild returns this error directly.
	Err error
	// lastBuildOptions records the ImageBuildOptions from the most recent ImageBuild call.
	lastBuildOptions moby.ImageBuildOptions
	PingErr          error // if non-nil, Ping returns this error
	ImageMissing     bool  // if true, ImageInspect returns a not-found error
	ImageErr         error // if non-nil (and ImageMissing false), ImageInspect returns this error
}

// newFakeBuildClient creates a fakeBuildClient. exitCode 0 means success.
func newFakeBuildClient(exitCode int) *fakeBuildClient {
	return &fakeBuildClient{ExitCode: exitCode}
}

// Ping returns PingErr if set, otherwise delegates to noopClient (success).
func (f *fakeBuildClient) Ping(_ context.Context, _ moby.PingOptions) (moby.PingResult, error) {
	if f.PingErr != nil {
		return moby.PingResult{}, f.PingErr
	}
	return moby.PingResult{}, nil
}

// ImageInspect returns a not-found error when ImageMissing is true, ImageErr
// when ImageErr is set, or a found result otherwise.
func (f *fakeBuildClient) ImageInspect(_ context.Context, imageID string, _ ...moby.ImageInspectOption) (moby.ImageInspectResult, error) {
	if f.ImageMissing {
		return moby.ImageInspectResult{}, fmt.Errorf("image %q: %w", imageID, errdefs.ErrNotFound)
	}
	if f.ImageErr != nil {
		return moby.ImageInspectResult{}, f.ImageErr
	}
	return moby.ImageInspectResult{}, nil
}

func (f *fakeBuildClient) ImageBuild(_ context.Context, _ io.Reader, opts moby.ImageBuildOptions) (moby.ImageBuildResult, error) {
	f.lastBuildOptions = opts
	if f.Err != nil {
		return moby.ImageBuildResult{}, f.Err
	}
	if f.ExitCode != 0 {
		return moby.ImageBuildResult{}, fmt.Errorf("build exited with code %d", f.ExitCode)
	}
	// Return an empty body so renderBuildOutput completes cleanly.
	return moby.ImageBuildResult{Body: io.NopCloser(bytes.NewReader(nil))}, nil
}

func (f *fakeBuildClient) DialHijack(_ context.Context, _, _ string, _ map[string][]string) (net.Conn, error) {
	// Return an error so the session dialer fails deterministically.
	// A pre-closed pipe would introduce a race: ImageBuild might not be reached
	// before the session notices the broken pipe.
	return nil, fmt.Errorf("DialHijack: not implemented in fakeBuildClient")
}

// ─── Constructor helpers ──────────────────────────────────────────────────────

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
