package docker

// Test-only helpers and fake apiClient types for the docker package.

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

// noopClient is a no-op apiClient. fakeRunClient and fakeBuildClient embed it
// and override only the methods that carry test logic.
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

func (noopClient) Ping(_ context.Context, _ moby.PingOptions) (moby.PingResult, error) {
	return moby.PingResult{}, nil
}

// ImageInspect defaults to a found result.
func (noopClient) ImageInspect(_ context.Context, _ string, _ ...moby.ImageInspectOption) (moby.ImageInspectResult, error) {
	return moby.ImageInspectResult{}, nil
}

func (noopClient) Close() error { return nil }

// fakeRunClient scripts the Run container lifecycle with a given exit code and
// records calls. Set PingErr for daemon-down, ImageMissing/ImageErr for image errors.
type fakeRunClient struct {
	noopClient
	ExitCode     int
	wasStarted   bool
	PingErr      error // if non-nil, Ping returns this
	ImageMissing bool  // if true, ImageInspect returns not-found
	ImageErr     error // if non-nil (and ImageMissing false), ImageInspect returns this

	RemovedContainers       []string
	LastContainerCreateOpts moby.ContainerCreateOptions
	ContainerCreateErr      error // if non-nil, ContainerCreate returns this
	ContainerStartErr       error // if non-nil, ContainerStart returns this

	// Block* make the call block until ctx is cancelled, then return ctx.Err() —
	// lets tests verify a timeout deadline reaches the call site.
	BlockPing         bool
	BlockImageInspect bool
}

// newFakeRunClient returns a fakeRunClient whose ContainerWait reports exitCode.
func newFakeRunClient(exitCode int) *fakeRunClient {
	return &fakeRunClient{ExitCode: exitCode}
}

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

// fakeBuildClient scripts Build, recording the ImageBuildOptions. Set PingErr
// for daemon-down, ImageMissing/ImageErr for image errors.
type fakeBuildClient struct {
	noopClient
	ExitCode int   // non-zero → ImageBuild returns an error
	Err      error // if non-nil, overrides ExitCode: ImageBuild returns this directly
	// lastBuildOptions records the options from the most recent ImageBuild call.
	lastBuildOptions moby.ImageBuildOptions
	PingErr          error // if non-nil, Ping returns this
	ImageMissing     bool  // if true, ImageInspect returns not-found
	ImageErr         error // if non-nil (and ImageMissing false), ImageInspect returns this
}

// newFakeBuildClient returns a fakeBuildClient; exitCode 0 means success.
func newFakeBuildClient(exitCode int) *fakeBuildClient {
	return &fakeBuildClient{ExitCode: exitCode}
}

func (f *fakeBuildClient) Ping(_ context.Context, _ moby.PingOptions) (moby.PingResult, error) {
	if f.PingErr != nil {
		return moby.PingResult{}, f.PingErr
	}
	return moby.PingResult{}, nil
}

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
	// Fail the session dialer deterministically. A pre-closed pipe would race:
	// ImageBuild might not be reached before the session notices the broken pipe.
	return nil, fmt.Errorf("DialHijack: not implemented in fakeBuildClient")
}

// newDockerWithClient builds a *Docker with the fake injected via WithClient and
// registers d.Close via t.Cleanup.
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

// noopMakeRaw is a WithRawMode stub safe without a real PTY (term.Restore(nil) is a no-op).
func noopMakeRaw(_ int) (*term.State, error) { return nil, nil }

func alwaysTTY() bool { return true }

func neverTTY() bool { return false }
