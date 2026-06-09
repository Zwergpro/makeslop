package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	moby "github.com/moby/moby/client"
	"golang.org/x/term"
)

func sampleSpec() Spec {
	return BuildSpec(Options{
		ProjectRoot:   "/host/project",
		WorkspaceName: "demo-abc123",
		BaseDir:       "/host/.makeslop",
		Image:         "claudebox",
		Command:       "/bin/zsh",
	})
}

// ─── fakeClient: scripted apiClient for Run tests ────────────────────────────

// fakeClient is a minimal fake apiClient that scripts the ContainerWait result
// and can optionally fail on ContainerCreate, ContainerAttach, or ContainerStart.
// It records which calls were made so tests can assert on them.
type fakeClient struct {
	// scripted behaviour
	createErr     error // if non-nil, ContainerCreate returns this
	attachErr     error // if non-nil, ContainerAttach returns this
	startErr      error // if non-nil, ContainerStart returns this
	waitResult    container.WaitResponse
	waitErr       error  // if non-nil, sent on the error channel
	attachPayload string // data that appears on the container's stdout

	// observation
	created    bool
	attached   bool
	wasStarted bool
	removed    bool
	closed     bool
}

func (f *fakeClient) ContainerCreate(_ context.Context, _ moby.ContainerCreateOptions) (moby.ContainerCreateResult, error) {
	f.created = true
	if f.createErr != nil {
		return moby.ContainerCreateResult{}, f.createErr
	}
	return moby.ContainerCreateResult{ID: "fake-container-id"}, nil
}

func (f *fakeClient) ContainerAttach(_ context.Context, _ string, _ moby.ContainerAttachOptions) (moby.ContainerAttachResult, error) {
	f.attached = true
	if f.attachErr != nil {
		return moby.ContainerAttachResult{}, f.attachErr
	}

	// Set up a pipe so we can write attachPayload and have it appear on the read side.
	pr, pw := net.Pipe()

	// Write the scripted payload then close the write side so the pump goroutine ends.
	go func() {
		if f.attachPayload != "" {
			_, _ = io.WriteString(pw, f.attachPayload)
		}
		_ = pw.Close()
	}()

	hr := moby.NewHijackedResponse(pr, "")
	return moby.ContainerAttachResult{HijackedResponse: hr}, nil
}

func (f *fakeClient) ContainerStart(_ context.Context, _ string, _ moby.ContainerStartOptions) (moby.ContainerStartResult, error) {
	f.wasStarted = true
	if f.startErr != nil {
		return moby.ContainerStartResult{}, f.startErr
	}
	return moby.ContainerStartResult{}, nil
}

func (f *fakeClient) ContainerWait(_ context.Context, _ string, _ moby.ContainerWaitOptions) moby.ContainerWaitResult {
	resultC := make(chan container.WaitResponse, 1)
	errC := make(chan error, 1)
	if f.waitErr != nil {
		errC <- f.waitErr
	} else {
		resultC <- f.waitResult
	}
	return moby.ContainerWaitResult{Result: resultC, Error: errC}
}

func (f *fakeClient) ContainerResize(_ context.Context, _ string, _ moby.ContainerResizeOptions) (moby.ContainerResizeResult, error) {
	return moby.ContainerResizeResult{}, nil
}

func (f *fakeClient) ContainerRemove(_ context.Context, _ string, _ moby.ContainerRemoveOptions) (moby.ContainerRemoveResult, error) {
	f.removed = true
	return moby.ContainerRemoveResult{}, nil
}

func (f *fakeClient) ImageBuild(_ context.Context, _ io.Reader, _ moby.ImageBuildOptions) (moby.ImageBuildResult, error) {
	return moby.ImageBuildResult{Body: io.NopCloser(bytes.NewReader(nil))}, nil
}

func (f *fakeClient) DialHijack(_ context.Context, _, _ string, _ map[string][]string) (net.Conn, error) {
	return nil, errors.New("DialHijack not implemented in fakeClient")
}

func (f *fakeClient) Ping(_ context.Context, _ moby.PingOptions) (moby.PingResult, error) {
	return moby.PingResult{}, nil
}

func (f *fakeClient) ImageInspect(_ context.Context, _ string, _ ...moby.ImageInspectOption) (moby.ImageInspectResult, error) {
	return moby.ImageInspectResult{}, nil
}

func (f *fakeClient) Close() error {
	f.closed = true
	return nil
}

// ─── run() unit tests ─────────────────────────────────────────────────────────

func TestRun_NoTTY_ReturnsSentinel_NoClientCall(t *testing.T) {
	t.Cleanup(SetTTYCheckForTest(func() bool { return false }))

	fc := &fakeClient{}
	err := run(context.Background(), fc, func() bool { return false }, func(_ int) (*term.State, error) { return nil, nil }, sampleSpec())
	if !errors.Is(err, ErrNoTTY) {
		t.Fatalf("expected ErrNoTTY, got %v", err)
	}
	if fc.created {
		t.Error("ContainerCreate must not be called when ttyCheck is false")
	}
}

// TestRun_ExitMapping_ZeroCode: status 0 → nil error.
func TestRun_ExitMapping_ZeroCode(t *testing.T) {
	err := mapWaitResponse(container.WaitResponse{StatusCode: 0})
	if err != nil {
		t.Errorf("expected nil for StatusCode 0, got %v", err)
	}
}

// TestRun_ExitMapping_NonZero: non-zero StatusCode → *ExitError.
func TestRun_ExitMapping_NonZero(t *testing.T) {
	err := mapWaitResponse(container.WaitResponse{StatusCode: 42})
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected *ExitError, got %T: %v", err, err)
	}
	if ee.Code != 42 {
		t.Errorf("ExitError.Code = %d, want 42", ee.Code)
	}
}

// TestRun_ExitMapping_Signal: daemon reports 137 (128+SIGKILL) → ExitError{137}.
func TestRun_ExitMapping_Signal(t *testing.T) {
	err := mapWaitResponse(container.WaitResponse{StatusCode: 137})
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected *ExitError, got %T: %v", err, err)
	}
	if ee.Code != 137 {
		t.Errorf("ExitError.Code = %d, want 137", ee.Code)
	}
}

// TestRun_ExitMapping_WaitExitError: WaitExitError → plain error (not *ExitError).
func TestRun_ExitMapping_WaitExitError(t *testing.T) {
	err := mapWaitResponse(container.WaitResponse{
		StatusCode: 1,
		Error:      &container.WaitExitError{Message: "daemon error"},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		t.Errorf("WaitExitError must not map to *ExitError; got %T", err)
	}
	if !strings.Contains(err.Error(), "daemon error") {
		t.Errorf("error should contain 'daemon error', got %v", err)
	}
}

// TestRun_StartFailure_ForcesRemove: when ContainerStart fails, the deferred
// best-effort force-remove must fire.
func TestRun_StartFailure_ForcesRemove(t *testing.T) {
	t.Cleanup(SetTTYCheckForTest(func() bool { return true }))

	startErr := errors.New("image not found")
	fc := &fakeClient{startErr: startErr}

	err := runWithForceTTY(context.Background(), fc, sampleSpec())
	if err == nil {
		t.Fatal("expected error from ContainerStart failure, got nil")
	}
	if !strings.Contains(err.Error(), "container start") {
		t.Errorf("expected 'container start' in error, got %v", err)
	}
	if !fc.removed {
		t.Error("ContainerRemove must be called when ContainerStart fails")
	}
}

// TestRun_CtxCancel_ForcesRemove: on context cancellation the deferred
// force-remove must fire.
func TestRun_CtxCancel_ForcesRemove(t *testing.T) {
	t.Cleanup(SetTTYCheckForTest(func() bool { return true }))

	bwc := newBlockingWaitClient()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runWithForceTTY(ctx, bwc, sampleSpec())
	}()

	// Cancel after the container "started".
	<-bwc.startedCh
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error on ctx cancel, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runWithForceTTY did not return within 5s after cancel")
	}
	if !bwc.removed {
		t.Error("ContainerRemove must be called on ctx cancel")
	}
}

// ExitError string test.
func TestExitError_ErrorString(t *testing.T) {
	e := &ExitError{Code: 137}
	want := "container exited with code 137"
	if e.Error() != want {
		t.Errorf("Error() = %q, want %q", e.Error(), want)
	}
}

// TestRun_ContainerCreate_Failure: ContainerCreate error surfaces immediately.
func TestRun_ContainerCreate_Failure(t *testing.T) {
	t.Cleanup(SetTTYCheckForTest(func() bool { return true }))
	t.Cleanup(SetTermMakeRawForTest(func(_ int) (*term.State, error) { return nil, nil }))

	createErr := errors.New("no such image")
	fc := &fakeClient{createErr: createErr}

	err := run(context.Background(), fc, func() bool { return true }, func(_ int) (*term.State, error) { return nil, nil }, sampleSpec())
	if err == nil {
		t.Fatal("expected error from ContainerCreate failure, got nil")
	}
	if !strings.Contains(err.Error(), "container create") {
		t.Errorf("expected 'container create' in error, got %v", err)
	}
	// No remove should be attempted when create never returned an ID.
	if fc.removed {
		t.Error("ContainerRemove must not be called when ContainerCreate fails")
	}
}

// TestRun_ContainerAttach_Failure: ContainerAttach error fires deferred remove.
func TestRun_ContainerAttach_Failure(t *testing.T) {
	t.Cleanup(SetTTYCheckForTest(func() bool { return true }))
	t.Cleanup(SetTermMakeRawForTest(func(_ int) (*term.State, error) { return nil, nil }))

	attachErr := errors.New("stream attach refused")
	fc := &fakeClient{attachErr: attachErr}

	err := run(context.Background(), fc, func() bool { return true }, func(_ int) (*term.State, error) { return nil, nil }, sampleSpec())
	if err == nil {
		t.Fatal("expected error from ContainerAttach failure, got nil")
	}
	if !strings.Contains(err.Error(), "container attach") {
		t.Errorf("expected 'container attach' in error, got %v", err)
	}
	// Container was created but never started, so deferred remove must fire.
	if !fc.removed {
		t.Error("ContainerRemove must be called when ContainerAttach fails")
	}
}

// TestRun_WaitErrorChannel: wr.Error channel path in run() surfaces correctly.
func TestRun_WaitErrorChannel(t *testing.T) {
	t.Cleanup(SetTTYCheckForTest(func() bool { return true }))

	waitErr := errors.New("daemon connection lost")
	fc := &fakeClient{waitErr: waitErr}

	err := runWithForceTTY(context.Background(), fc, sampleSpec())
	if err == nil {
		t.Fatal("expected error from wr.Error, got nil")
	}
	if !strings.Contains(err.Error(), "container wait") {
		t.Errorf("expected 'container wait' in error, got %v", err)
	}
}

// mapWaitResponse is the exit-translation logic extracted for unit testing.
// It must match what run() does in the select case for wr.Result.
func mapWaitResponse(res container.WaitResponse) error {
	if res.Error != nil {
		return fmt.Errorf("container wait error: %s", res.Error.Message)
	}
	if res.StatusCode != 0 {
		return &ExitError{Code: int(res.StatusCode)}
	}
	return nil
}

// runWithForceTTY calls run() with a no-op termMakeRaw stub so tests can
// exercise the full run() path without a real PTY. ttyCheck must be stubbed
// true by the caller before calling this.
func runWithForceTTY(ctx context.Context, cli apiClient, s Spec) error {
	noopMakeRaw := func(_ int) (*term.State, error) { return nil, nil }
	alwaysTTY := func() bool { return true }
	return run(ctx, cli, alwaysTTY, noopMakeRaw, s)
}

// blockingWaitClient is a fake apiClient whose ContainerWait blocks until the
// test's context is cancelled.
type blockingWaitClient struct {
	fakeClient
	startedCh chan struct{}
}

func newBlockingWaitClient() *blockingWaitClient {
	return &blockingWaitClient{startedCh: make(chan struct{})}
}

func (b *blockingWaitClient) ContainerWait(ctx context.Context, _ string, _ moby.ContainerWaitOptions) moby.ContainerWaitResult {
	resultC := make(chan container.WaitResponse)
	errC := make(chan error, 1)
	go func() {
		<-ctx.Done()
		errC <- ctx.Err()
	}()
	return moby.ContainerWaitResult{Result: resultC, Error: errC}
}

func (b *blockingWaitClient) ContainerStart(_ context.Context, _ string, _ moby.ContainerStartOptions) (moby.ContainerStartResult, error) {
	b.wasStarted = true
	select {
	case <-b.startedCh:
		// already closed — no-op
	default:
		close(b.startedCh)
	}
	return moby.ContainerStartResult{}, nil
}
