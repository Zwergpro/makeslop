package docker

// Test-only helpers, compiled into the production binary so package-main tests
// can access them as exported symbols (export_test.go cannot satisfy this because
// main_test.go is in package main, not package docker_test).

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"iter"
	"net"
	"runtime"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/api/types/volume"
	moby "github.com/moby/moby/client"
	"golang.org/x/term"
)

// SetTTYCheckForTest swaps the TTY-detection predicate Run consults. Same
// caveats as SetClientForTest apply.
func SetTTYCheckForTest(fn func() bool) (restore func()) {
	prev := ttyCheck
	ttyCheck = fn
	return func() { ttyCheck = prev }
}

// SetClientForTest swaps the client factory that Run uses, replacing it with
// one that always returns c. Returns a restore function that callers MUST
// register with t.Cleanup. Concurrent tests that touch this swap point must
// serialize themselves (the package state is process-global).
func SetClientForTest(c apiClient) (restore func()) {
	prev := newClientFn
	newClientFn = func() (apiClient, error) { return c, nil }
	return func() { newClientFn = prev }
}

// SetTermMakeRawForTest swaps the terminal raw-mode function that run() calls.
// Use a no-op stub in tests that have no real PTY:
//
//	t.Cleanup(docker.SetTermMakeRawForTest(func(fd int) (*term.State, error) {
//	    return nil, nil
//	}))
//
// IMPORTANT: when the stub returns nil state, defer term.Restore(fd, nil) is
// called — that is a no-op, so restoring is safe.
func SetTermMakeRawForTest(fn func(fd int) (*term.State, error)) (restore func()) {
	prev := termMakeRaw
	termMakeRaw = fn
	return func() { termMakeRaw = prev }
}

// SkipNonPOSIX skips on non-POSIX hosts per the CLAUDE.md invariant.
func SkipNonPOSIX(t *testing.T, why string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip(why)
	}
}

// noopImagePullResponse is a minimal ImagePullResponse that completes
// immediately with no data. Used by noopClient.ImagePull and by
// FakeRunClient.ImagePull when ImagePullErr is nil.
type noopImagePullResponse struct{}

func (noopImagePullResponse) Read(_ []byte) (int, error)          { return 0, io.EOF }
func (noopImagePullResponse) Close() error                        { return nil }
func (noopImagePullResponse) Wait(_ context.Context) error        { return nil }
func (noopImagePullResponse) JSONMessages(_ context.Context) iter.Seq2[jsonstream.Message, error] {
	return func(yield func(jsonstream.Message, error) bool) {}
}

// noopClient provides no-op stub implementations of all apiClient methods.
// FakeRunClient and FakeBuildClient embed it and override only the methods
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

func (noopClient) ContainerInspect(_ context.Context, _ string, _ moby.ContainerInspectOptions) (moby.ContainerInspectResult, error) {
	return moby.ContainerInspectResult{}, nil
}

func (noopClient) ExecCreate(_ context.Context, _ string, _ moby.ExecCreateOptions) (moby.ExecCreateResult, error) {
	return moby.ExecCreateResult{}, nil
}

func (noopClient) ExecStart(_ context.Context, _ string, _ moby.ExecStartOptions) (moby.ExecStartResult, error) {
	return moby.ExecStartResult{}, nil
}

func (noopClient) ExecInspect(_ context.Context, _ string, _ moby.ExecInspectOptions) (moby.ExecInspectResult, error) {
	return moby.ExecInspectResult{}, nil
}

func (noopClient) VolumeCreate(_ context.Context, _ moby.VolumeCreateOptions) (moby.VolumeCreateResult, error) {
	return moby.VolumeCreateResult{}, nil
}

func (noopClient) VolumeRemove(_ context.Context, _ string, _ moby.VolumeRemoveOptions) (moby.VolumeRemoveResult, error) {
	return moby.VolumeRemoveResult{}, nil
}

func (noopClient) ImagePull(_ context.Context, _ string, _ moby.ImagePullOptions) (moby.ImagePullResponse, error) {
	return noopImagePullResponse{}, nil
}

func (noopClient) Close() error { return nil }

// FakeRunClient is an exported fake apiClient for use in cmd/makeslop tests.
// It instruments Run calls with a scripted exit code and records whether the
// container was started. TTY check must still be stubbed by the caller via
// SetTTYCheckForTest.
//
// Callers register the fake with:
//
//	t.Cleanup(docker.SetClientForTest(docker.NewFakeRunClient(exitCode)))
//
// To simulate daemon-down, set PingErr. To simulate a missing image, set
// ImageMissing = true. To simulate another image error, set ImageErr.
//
// Volume tracking:
//   - CreatedVolumes records the names of all VolumeCreate calls.
//   - RemovedVolumes records the names of all VolumeRemove calls.
//
// Exec handshake:
//   - ExecExitCode is the exit code returned by ExecInspect.
//   - ExecRunning, when true, causes ExecInspect to report Running=true
//     (still in progress).
//
// Sidecar early-exit simulation:
//   - SidecarExited, when true, causes ContainerInspect to report a non-running
//     exited state (State.Running=false, State.ExitCode=1).
//
// Image pull simulation:
//   - ImagePullCalled records whether ImagePull was invoked.
//   - ImagePullErr, if non-nil, is returned by ImagePull.
//   - SocatImageMissing, when true, causes every ImageInspect call for the socat
//     image to return not-found; used to test pull-on-demand.
type FakeRunClient struct {
	noopClient
	ExitCode     int
	Started      bool
	PingErr      error // if non-nil, Ping returns this error
	ImageMissing bool  // if true, ImageInspect returns a not-found error
	ImageErr     error // if non-nil (and ImageMissing false), ImageInspect returns this error

	// Volume tracking
	CreatedVolumes       []string
	CreatedVolumeLabels  []map[string]string // labels from each VolumeCreate call, parallel to CreatedVolumes
	RemovedVolumes       []string
	VolumeCreateErr      error // if non-nil, VolumeCreate returns this error

	// Container tracking
	RemovedContainers      []string
	LastContainerCreateOpts moby.ContainerCreateOptions // options from most recent ContainerCreate call
	ContainerCreateErr     error                        // if non-nil, ContainerCreate returns this error
	ContainerStartErr      error                        // if non-nil, ContainerStart returns this error

	// Exec handshake
	ExecExitCode        int   // exit code returned by ExecInspect (default 0 = success)
	ExecRunning         bool  // if true, ExecInspect reports Running=true
	ExecCreateErr       error // if non-nil, ExecCreate returns this error
	ExecStartErr        error // if non-nil, ExecStart returns this error
	ExecInspectErr      error // if non-nil, ExecInspect returns this error
	ContainerInspectErr error // if non-nil, ContainerInspect returns this error

	// Sidecar early-exit simulation
	SidecarExited bool // if true, ContainerInspect reports exited state

	// Image pull simulation
	ImagePullCalled bool  // set to true when ImagePull is called
	ImagePullErr    error // if non-nil, ImagePull returns this error

	// SocatImageMissing simulates the socat image being absent; ImageInspect
	// returns not-found for the socat image (identified by matching SocatImage).
	SocatImageMissing bool

	// SocatImageErr, if non-nil, is returned by ImageInspect for the socat image
	// as a non-not-found error (simulating a daemon I/O error specifically for
	// the socat image, while letting the app image inspect succeed).
	SocatImageErr error

	// BlockPing, when true, causes Ping to block until ctx is cancelled, then
	// return ctx.Err(). This lets tests verify that a timeout deadline is
	// propagated through to the Ping call site.
	BlockPing bool

	// BlockImageInspect, when true, causes ImageInspect to block until ctx is
	// cancelled, then return ctx.Err(). This lets tests verify that a timeout
	// deadline is propagated through to the ImageInspect call site.
	BlockImageInspect bool
}

// NewFakeRunClient creates a FakeRunClient that will return the given exit code
// from ContainerWait.
func NewFakeRunClient(exitCode int) *FakeRunClient {
	return &FakeRunClient{ExitCode: exitCode}
}

// Ping returns PingErr if set, otherwise delegates to noopClient (success).
// If BlockPing is true, Ping blocks until ctx is cancelled and returns ctx.Err().
// This allows tests to verify that a timeout deadline is propagated to Ping.
func (f *FakeRunClient) Ping(ctx context.Context, _ moby.PingOptions) (moby.PingResult, error) {
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
// If BlockImageInspect is true, ImageInspect blocks until ctx is cancelled and
// returns ctx.Err(). This allows tests to verify that a timeout deadline is
// propagated to ImageInspect.
// If SocatImageMissing is true, calls for the socat image (identified by
// matching imageID against SocatImage) return not-found. Other images are
// unaffected by SocatImageMissing, allowing tests to model the case where the
// app image is present but alpine/socat has not been pulled yet.
// If SocatImageErr is non-nil, calls for the socat image return that error
// (non-not-found, simulating daemon I/O errors specifically for the socat image).
func (f *FakeRunClient) ImageInspect(ctx context.Context, imageID string, _ ...moby.ImageInspectOption) (moby.ImageInspectResult, error) {
	if f.BlockImageInspect {
		<-ctx.Done()
		return moby.ImageInspectResult{}, ctx.Err()
	}
	if f.ImageMissing {
		return moby.ImageInspectResult{}, fmt.Errorf("image %q: %w", imageID, errdefs.ErrNotFound)
	}
	if f.SocatImageMissing && imageID == SocatImage {
		return moby.ImageInspectResult{}, fmt.Errorf("image %q: %w", imageID, errdefs.ErrNotFound)
	}
	if f.SocatImageErr != nil && imageID == SocatImage {
		return moby.ImageInspectResult{}, f.SocatImageErr
	}
	if f.ImageErr != nil {
		return moby.ImageInspectResult{}, f.ImageErr
	}
	return moby.ImageInspectResult{}, nil
}

// ContainerInspect returns an exited (non-running) state when SidecarExited is
// true, an error when ContainerInspectErr is set, otherwise returns a running state.
func (f *FakeRunClient) ContainerInspect(_ context.Context, _ string, _ moby.ContainerInspectOptions) (moby.ContainerInspectResult, error) {
	if f.ContainerInspectErr != nil {
		return moby.ContainerInspectResult{}, f.ContainerInspectErr
	}
	if f.SidecarExited {
		return moby.ContainerInspectResult{
			Container: container.InspectResponse{
				State: &container.State{
					Running:  false,
					ExitCode: 1,
					Status:   container.StateExited,
				},
			},
		}, nil
	}
	return moby.ContainerInspectResult{
		Container: container.InspectResponse{
			State: &container.State{
				Running: true,
				Status:  container.StateRunning,
			},
		},
	}, nil
}

// ExecCreate records the exec creation and returns a fake exec ID.
// If ExecCreateErr is set, it is returned instead.
func (f *FakeRunClient) ExecCreate(_ context.Context, _ string, _ moby.ExecCreateOptions) (moby.ExecCreateResult, error) {
	if f.ExecCreateErr != nil {
		return moby.ExecCreateResult{}, f.ExecCreateErr
	}
	return moby.ExecCreateResult{ID: "fake-exec-id"}, nil
}

// ExecStart is a no-op (blocks in production until exec completes; Detach: false).
// If ExecStartErr is set, it is returned instead.
func (f *FakeRunClient) ExecStart(_ context.Context, _ string, _ moby.ExecStartOptions) (moby.ExecStartResult, error) {
	if f.ExecStartErr != nil {
		return moby.ExecStartResult{}, f.ExecStartErr
	}
	return moby.ExecStartResult{}, nil
}

// ExecInspect returns scripted Running/ExitCode values.
// If ExecInspectErr is set, it is returned instead.
func (f *FakeRunClient) ExecInspect(_ context.Context, _ string, _ moby.ExecInspectOptions) (moby.ExecInspectResult, error) {
	if f.ExecInspectErr != nil {
		return moby.ExecInspectResult{}, f.ExecInspectErr
	}
	return moby.ExecInspectResult{
		Running:  f.ExecRunning,
		ExitCode: f.ExecExitCode,
	}, nil
}

// VolumeCreate records the created volume name and labels, then returns it.
// If VolumeCreateErr is set, it is returned without recording the volume.
func (f *FakeRunClient) VolumeCreate(_ context.Context, opts moby.VolumeCreateOptions) (moby.VolumeCreateResult, error) {
	if f.VolumeCreateErr != nil {
		return moby.VolumeCreateResult{}, f.VolumeCreateErr
	}
	f.CreatedVolumes = append(f.CreatedVolumes, opts.Name)
	f.CreatedVolumeLabels = append(f.CreatedVolumeLabels, opts.Labels)
	return moby.VolumeCreateResult{Volume: volume.Volume{Name: opts.Name}}, nil
}

// VolumeRemove records the removed volume name.
func (f *FakeRunClient) VolumeRemove(_ context.Context, volumeID string, _ moby.VolumeRemoveOptions) (moby.VolumeRemoveResult, error) {
	f.RemovedVolumes = append(f.RemovedVolumes, volumeID)
	return moby.VolumeRemoveResult{}, nil
}

// ImagePull records the call and returns ImagePullErr if set.
func (f *FakeRunClient) ImagePull(_ context.Context, _ string, _ moby.ImagePullOptions) (moby.ImagePullResponse, error) {
	f.ImagePullCalled = true
	if f.ImagePullErr != nil {
		return nil, f.ImagePullErr
	}
	return noopImagePullResponse{}, nil
}

// ContainerRemove records the removed container ID.
func (f *FakeRunClient) ContainerRemove(_ context.Context, id string, _ moby.ContainerRemoveOptions) (moby.ContainerRemoveResult, error) {
	f.RemovedContainers = append(f.RemovedContainers, id)
	return moby.ContainerRemoveResult{}, nil
}

func (f *FakeRunClient) ContainerCreate(_ context.Context, opts moby.ContainerCreateOptions) (moby.ContainerCreateResult, error) {
	f.LastContainerCreateOpts = opts
	if f.ContainerCreateErr != nil {
		return moby.ContainerCreateResult{}, f.ContainerCreateErr
	}
	return moby.ContainerCreateResult{ID: "fake-id"}, nil
}

func (f *FakeRunClient) ContainerAttach(_ context.Context, _ string, _ moby.ContainerAttachOptions) (moby.ContainerAttachResult, error) {
	pr, pw := net.Pipe()
	go func() { _ = pw.Close() }()
	hr := moby.NewHijackedResponse(pr, "")
	return moby.ContainerAttachResult{HijackedResponse: hr}, nil
}

func (f *FakeRunClient) ContainerStart(_ context.Context, _ string, _ moby.ContainerStartOptions) (moby.ContainerStartResult, error) {
	if f.ContainerStartErr != nil {
		return moby.ContainerStartResult{}, f.ContainerStartErr
	}
	f.Started = true
	return moby.ContainerStartResult{}, nil
}

func (f *FakeRunClient) ContainerWait(_ context.Context, _ string, _ moby.ContainerWaitOptions) moby.ContainerWaitResult {
	resultC := make(chan container.WaitResponse, 1)
	errC := make(chan error, 1)
	resultC <- container.WaitResponse{StatusCode: int64(f.ExitCode)}
	return moby.ContainerWaitResult{Result: resultC, Error: errC}
}

// FakeBuildClient is an exported fake apiClient for use in cmd/makeslop build
// tests. It records the ImageBuildOptions passed to ImageBuild and returns a
// scripted result. Unlike FakeRunClient it does not simulate a TTY or
// container lifecycle — it only instruments the Build path.
//
// Usage:
//
//	fbc := docker.NewFakeBuildClient(0)  // 0 = success
//	t.Cleanup(docker.SetClientForTest(fbc))
//	// ... call Build ...
//	opts := fbc.LastBuildOptions  // inspect what was passed
//
// To simulate daemon-down, set PingErr. To simulate a missing image, set
// ImageMissing = true. To simulate another image error, set ImageErr.
type FakeBuildClient struct {
	noopClient
	// ExitCode is returned as an error from ImageBuild when non-zero.
	ExitCode int
	// Err overrides ExitCode if non-nil: ImageBuild returns this error directly.
	Err error
	// LastBuildOptions records the ImageBuildOptions from the most recent
	// ImageBuild call.
	LastBuildOptions moby.ImageBuildOptions
	PingErr          error // if non-nil, Ping returns this error
	ImageMissing     bool  // if true, ImageInspect returns a not-found error
	ImageErr         error // if non-nil (and ImageMissing false), ImageInspect returns this error

	// BlockPing, when true, causes Ping to block until ctx is cancelled and
	// return ctx.Err(). Lets tests verify timeout propagation.
	BlockPing bool

	// BlockImageInspect, when true, causes ImageInspect to block until ctx is
	// cancelled and return ctx.Err(). Lets tests verify timeout propagation.
	BlockImageInspect bool
}

// NewFakeBuildClient creates a FakeBuildClient. exitCode 0 means success.
func NewFakeBuildClient(exitCode int) *FakeBuildClient {
	return &FakeBuildClient{ExitCode: exitCode}
}

// Ping returns PingErr if set, otherwise delegates to noopClient (success).
// If BlockPing is true, Ping blocks until ctx is cancelled and returns ctx.Err().
func (f *FakeBuildClient) Ping(ctx context.Context, _ moby.PingOptions) (moby.PingResult, error) {
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
// If BlockImageInspect is true, ImageInspect blocks until ctx is cancelled and
// returns ctx.Err().
func (f *FakeBuildClient) ImageInspect(ctx context.Context, imageID string, _ ...moby.ImageInspectOption) (moby.ImageInspectResult, error) {
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

func (f *FakeBuildClient) ImageBuild(_ context.Context, _ io.Reader, opts moby.ImageBuildOptions) (moby.ImageBuildResult, error) {
	f.LastBuildOptions = opts
	if f.Err != nil {
		return moby.ImageBuildResult{}, f.Err
	}
	if f.ExitCode != 0 {
		return moby.ImageBuildResult{}, fmt.Errorf("build exited with code %d", f.ExitCode)
	}
	// Return an empty body so renderBuildOutput completes cleanly.
	return moby.ImageBuildResult{Body: io.NopCloser(bytes.NewReader(nil))}, nil
}

func (f *FakeBuildClient) DialHijack(_ context.Context, _, _ string, _ map[string][]string) (net.Conn, error) {
	// Return a closed pipe so the session dialer fails gracefully.
	c, _ := net.Pipe()
	_ = c.Close()
	return c, nil
}
