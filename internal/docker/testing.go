package docker

// Test-only helpers, compiled into the production binary so package-main tests
// can access them as exported symbols (export_test.go cannot satisfy this because
// main_test.go is in package main, not package docker_test).

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"runtime"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
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
func (noopClient) ImageInspect(_ context.Context, imageID string, _ ...moby.ImageInspectOption) (moby.ImageInspectResult, error) {
	return moby.ImageInspectResult{}, nil
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
type FakeRunClient struct {
	noopClient
	ExitCode     int
	Started      bool
	PingErr      error // if non-nil, Ping returns this error
	ImageMissing bool  // if true, ImageInspect returns a not-found error
	ImageErr     error // if non-nil (and ImageMissing false), ImageInspect returns this error
}

// NewFakeRunClient creates a FakeRunClient that will return the given exit code
// from ContainerWait.
func NewFakeRunClient(exitCode int) *FakeRunClient {
	return &FakeRunClient{ExitCode: exitCode}
}

// Ping returns PingErr if set, otherwise delegates to noopClient (success).
func (f *FakeRunClient) Ping(_ context.Context, _ moby.PingOptions) (moby.PingResult, error) {
	if f.PingErr != nil {
		return moby.PingResult{}, f.PingErr
	}
	return moby.PingResult{}, nil
}

// ImageInspect returns a not-found error when ImageMissing is true, ImageErr
// when ImageErr is set, or a found result otherwise.
func (f *FakeRunClient) ImageInspect(_ context.Context, imageID string, _ ...moby.ImageInspectOption) (moby.ImageInspectResult, error) {
	if f.ImageMissing {
		return moby.ImageInspectResult{}, fmt.Errorf("image %q: %w", imageID, errdefs.ErrNotFound)
	}
	if f.ImageErr != nil {
		return moby.ImageInspectResult{}, f.ImageErr
	}
	return moby.ImageInspectResult{}, nil
}

func (f *FakeRunClient) ContainerCreate(_ context.Context, _ moby.ContainerCreateOptions) (moby.ContainerCreateResult, error) {
	return moby.ContainerCreateResult{ID: "fake-id"}, nil
}

func (f *FakeRunClient) ContainerAttach(_ context.Context, _ string, _ moby.ContainerAttachOptions) (moby.ContainerAttachResult, error) {
	pr, pw := net.Pipe()
	go func() { _ = pw.Close() }()
	hr := moby.NewHijackedResponse(pr, "")
	return moby.ContainerAttachResult{HijackedResponse: hr}, nil
}

func (f *FakeRunClient) ContainerStart(_ context.Context, _ string, _ moby.ContainerStartOptions) (moby.ContainerStartResult, error) {
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
}

// NewFakeBuildClient creates a FakeBuildClient. exitCode 0 means success.
func NewFakeBuildClient(exitCode int) *FakeBuildClient {
	return &FakeBuildClient{ExitCode: exitCode}
}

// Ping returns PingErr if set, otherwise delegates to noopClient (success).
func (f *FakeBuildClient) Ping(_ context.Context, _ moby.PingOptions) (moby.PingResult, error) {
	if f.PingErr != nil {
		return moby.PingResult{}, f.PingErr
	}
	return moby.PingResult{}, nil
}

// ImageInspect returns a not-found error when ImageMissing is true, ImageErr
// when ImageErr is set, or a found result otherwise.
func (f *FakeBuildClient) ImageInspect(_ context.Context, imageID string, _ ...moby.ImageInspectOption) (moby.ImageInspectResult, error) {
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
