package docker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	moby "github.com/moby/moby/client"
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

// fakeClient scripts the ContainerWait result and can fail ContainerCreate,
// ContainerAttach, or ContainerStart, recording which calls were made and their
// order.
type fakeClient struct {
	createErr     error // if non-nil, ContainerCreate returns this
	attachErr     error // if non-nil, ContainerAttach returns this
	startErr      error // if non-nil, ContainerStart returns this
	waitResult    container.WaitResponse
	waitErr       error  // if non-nil, sent on the error channel
	attachPayload string // data that appears on the container's stdout

	// delayedPayload, when non-empty, is output delivered AFTER the wait result.
	// The fake blocks the attach reader in two stages:
	//   1. writes attachPayload immediately, then signals attachReadyCh (if set)
	//   2. blocks on delayPayloadCh until the test releases it
	//   3. writes the delayed content and closes
	// attachReadyCh lets the test know the early payload has been written before
	// it releases the wait result (via waitAfterAttachCh), ensuring deterministic
	// ordering without time.Sleep.
	delayedPayload string
	// delayPayloadCh is the gate: send to release the delayed content.
	// Set up by ContainerAttach when delayedPayload != "".
	delayPayloadCh chan struct{}
	// attachReadyCh, when non-nil, is closed by the attach goroutine after writing
	// attachPayload (and before blocking on delayPayloadCh). Tests use this to know
	// the early output has been written before releasing the wait result.
	attachReadyCh chan struct{}
	// waitAfterAttachCh, when non-nil, causes ContainerWait to block until this
	// channel is closed before queuing the result. Tests close it after attachReadyCh
	// fires to guarantee: early payload written → wait result sent → delayed payload
	// released. This makes TestRun_OutputDrain deterministic without time.Sleep.
	waitAfterAttachCh chan struct{}

	created    bool
	attached   bool
	wasStarted bool
	removed    bool
	closed     bool

	// callOrder records the names of methods as they are called, in order.
	callOrder []string
}

func (f *fakeClient) ContainerCreate(_ context.Context, _ moby.ContainerCreateOptions) (moby.ContainerCreateResult, error) {
	f.callOrder = append(f.callOrder, "ContainerCreate")
	f.created = true
	if f.createErr != nil {
		return moby.ContainerCreateResult{}, f.createErr
	}
	return moby.ContainerCreateResult{ID: "fake-container-id"}, nil
}

func (f *fakeClient) ContainerAttach(_ context.Context, _ string, _ moby.ContainerAttachOptions) (moby.ContainerAttachResult, error) {
	f.callOrder = append(f.callOrder, "ContainerAttach")
	f.attached = true
	if f.attachErr != nil {
		return moby.ContainerAttachResult{}, f.attachErr
	}

	pr, pw := net.Pipe()

	if f.delayedPayload != "" {
		// Initialise the gate channel if the caller hasn't set one yet.
		if f.delayPayloadCh == nil {
			f.delayPayloadCh = make(chan struct{}, 1)
		}
		ch := f.delayPayloadCh
		readyCh := f.attachReadyCh
		waitAfterAttachCh := f.waitAfterAttachCh
		payload := f.attachPayload
		delayed := f.delayedPayload
		go func() {
			if payload != "" {
				_, _ = io.WriteString(pw, payload)
			}
			// Signal that the early payload has been written.
			if readyCh != nil {
				close(readyCh)
			}
			// Block until the test releases the delayed content.
			<-ch
			// Release the wait result BEFORE writing the tail payload.
			// net.Pipe writes block until the reader consumes them; if we wrote
			// "tail-line" first, io.Copy would consume it before waitAfterAttachCh
			// ever fired, making "tail-line" already in buf when Run receives the
			// wait result.  By firing waitAfterAttachCh first, Run enters the
			// drain select while the pipe is still empty — without the drain it
			// would return immediately (missing "tail-line"); with the drain it
			// waits for io.Copy to finish consuming the tail write below.
			if waitAfterAttachCh != nil {
				close(waitAfterAttachCh)
			}
			_, _ = io.WriteString(pw, delayed)
			_ = pw.Close()
		}()
	} else {
		// Write the scripted payload then close the write side so the pump goroutine ends.
		go func() {
			if f.attachPayload != "" {
				_, _ = io.WriteString(pw, f.attachPayload)
			}
			_ = pw.Close()
		}()
	}

	hr := moby.NewHijackedResponse(pr, "")
	return moby.ContainerAttachResult{HijackedResponse: hr}, nil
}

func (f *fakeClient) ContainerStart(_ context.Context, _ string, _ moby.ContainerStartOptions) (moby.ContainerStartResult, error) {
	f.callOrder = append(f.callOrder, "ContainerStart")
	f.wasStarted = true
	if f.startErr != nil {
		return moby.ContainerStartResult{}, f.startErr
	}
	return moby.ContainerStartResult{}, nil
}

func (f *fakeClient) ContainerWait(_ context.Context, _ string, _ moby.ContainerWaitOptions) moby.ContainerWaitResult {
	f.callOrder = append(f.callOrder, "ContainerWait")
	resultC := make(chan container.WaitResponse, 1)
	errC := make(chan error, 1)
	waitAfterAttachCh := f.waitAfterAttachCh
	if f.waitErr != nil {
		if waitAfterAttachCh != nil {
			go func() { <-waitAfterAttachCh; errC <- f.waitErr }()
		} else {
			errC <- f.waitErr
		}
	} else {
		if waitAfterAttachCh != nil {
			go func() { <-waitAfterAttachCh; resultC <- f.waitResult }()
		} else {
			resultC <- f.waitResult
		}
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

func TestRun_NoTTY_ReturnsSentinel_NoClientCall(t *testing.T) {
	fc := &fakeClient{}
	d := newDockerWithClient(t, fc,
		WithTTYCheck(neverTTY),
		WithRawMode(noopMakeRaw),
	)
	err := d.Run(context.Background(), sampleSpec())
	if !errors.Is(err, ErrNoTTY) {
		t.Fatalf("expected ErrNoTTY, got %v", err)
	}
	if fc.created {
		t.Error("ContainerCreate must not be called when ttyCheck is false")
	}
}

func TestRun_ExitMapping_ZeroCode(t *testing.T) {
	fc := &fakeClient{waitResult: container.WaitResponse{StatusCode: 0}}
	d := newDockerWithClient(t, fc, WithTTYCheck(alwaysTTY), WithRawMode(noopMakeRaw))
	if err := d.Run(context.Background(), sampleSpec()); err != nil {
		t.Errorf("expected nil for StatusCode 0, got %v", err)
	}
}

func TestRun_ExitMapping_NonZero(t *testing.T) {
	fc := &fakeClient{waitResult: container.WaitResponse{StatusCode: 42}}
	d := newDockerWithClient(t, fc, WithTTYCheck(alwaysTTY), WithRawMode(noopMakeRaw))
	err := d.Run(context.Background(), sampleSpec())
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected *ExitError, got %T: %v", err, err)
	}
	if ee.Code != 42 {
		t.Errorf("ExitError.Code = %d, want 42", ee.Code)
	}
}

// 137 (128+SIGKILL) reported by the daemon passes through as ExitError{137}.
func TestRun_ExitMapping_Signal(t *testing.T) {
	fc := &fakeClient{waitResult: container.WaitResponse{StatusCode: 137}}
	d := newDockerWithClient(t, fc, WithTTYCheck(alwaysTTY), WithRawMode(noopMakeRaw))
	err := d.Run(context.Background(), sampleSpec())
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected *ExitError, got %T: %v", err, err)
	}
	if ee.Code != 137 {
		t.Errorf("ExitError.Code = %d, want 137", ee.Code)
	}
}

// A WaitExitError must surface as a plain error, never *ExitError.
func TestRun_ExitMapping_WaitExitError(t *testing.T) {
	fc := &fakeClient{waitResult: container.WaitResponse{
		StatusCode: 1,
		Error:      &container.WaitExitError{Message: "daemon error"},
	}}
	d := newDockerWithClient(t, fc, WithTTYCheck(alwaysTTY), WithRawMode(noopMakeRaw))
	err := d.Run(context.Background(), sampleSpec())
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

// ContainerStart failure must fire the deferred best-effort force-remove.
func TestRun_StartFailure_ForcesRemove(t *testing.T) {
	startErr := errors.New("image not found")
	fc := &fakeClient{startErr: startErr}
	d := newDockerWithClient(t, fc,
		WithTTYCheck(alwaysTTY),
		WithRawMode(noopMakeRaw),
	)

	err := d.Run(context.Background(), sampleSpec())
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

// Context cancellation must fire the deferred force-remove.
func TestRun_CtxCancel_ForcesRemove(t *testing.T) {
	bwc := newBlockingWaitClient()
	d := newDockerWithClient(t, bwc,
		WithTTYCheck(alwaysTTY),
		WithRawMode(noopMakeRaw),
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- d.Run(ctx, sampleSpec())
	}()

	<-bwc.startedCh // cancel only after the container "started"
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error on ctx cancel, got nil")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("d.Run did not return within 5s after cancel")
	}
	if !bwc.removed {
		t.Error("ContainerRemove must be called on ctx cancel")
	}
}

func TestExitError_ErrorString(t *testing.T) {
	e := &ExitError{Code: 137}
	want := "container exited with code 137"
	if e.Error() != want {
		t.Errorf("Error() = %q, want %q", e.Error(), want)
	}
}

func TestRun_ContainerCreate_Failure(t *testing.T) {
	createErr := errors.New("no such image")
	fc := &fakeClient{createErr: createErr}
	d := newDockerWithClient(t, fc,
		WithTTYCheck(alwaysTTY),
		WithRawMode(noopMakeRaw),
	)

	err := d.Run(context.Background(), sampleSpec())
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

func TestRun_ContainerAttach_Failure(t *testing.T) {
	attachErr := errors.New("stream attach refused")
	fc := &fakeClient{attachErr: attachErr}
	d := newDockerWithClient(t, fc,
		WithTTYCheck(alwaysTTY),
		WithRawMode(noopMakeRaw),
	)

	err := d.Run(context.Background(), sampleSpec())
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

// Exercises the wr.Error channel path in run().
func TestRun_WaitErrorChannel(t *testing.T) {
	waitErr := errors.New("daemon connection lost")
	fc := &fakeClient{waitErr: waitErr}
	d := newDockerWithClient(t, fc,
		WithTTYCheck(alwaysTTY),
		WithRawMode(noopMakeRaw),
	)

	err := d.Run(context.Background(), sampleSpec())
	if err == nil {
		t.Fatal("expected error from wr.Error, got nil")
	}
	if !strings.Contains(err.Error(), "container wait") {
		t.Errorf("expected 'container wait' in error, got %v", err)
	}
}

// TestRun_WaitBeforeStart verifies that ContainerWait is registered before
// ContainerStart in the call order (fixing the AutoRemove + wait-after-start race).
func TestRun_WaitBeforeStart(t *testing.T) {
	fc := &fakeClient{waitResult: container.WaitResponse{StatusCode: 0}}
	d := newDockerWithClient(t, fc,
		WithTTYCheck(alwaysTTY),
		WithRawMode(noopMakeRaw),
		WithStreams(bytes.NewReader(nil), io.Discard),
	)
	if err := d.Run(context.Background(), sampleSpec()); err != nil {
		t.Fatalf("Run returned unexpected error: %v", err)
	}

	// Verify the call order: ContainerWait must precede ContainerStart.
	waitIdx := -1
	startIdx := -1
	for i, name := range fc.callOrder {
		switch name {
		case "ContainerWait":
			waitIdx = i
		case "ContainerStart":
			startIdx = i
		}
	}
	if waitIdx == -1 {
		t.Fatal("ContainerWait was never called")
	}
	if startIdx == -1 {
		t.Fatal("ContainerStart was never called")
	}
	if waitIdx >= startIdx {
		t.Errorf("ContainerWait (pos %d) must be called BEFORE ContainerStart (pos %d); order: %v",
			waitIdx, startIdx, fc.callOrder)
	}
}

// TestRun_OutputDrain proves that tail output written after the wait result is
// delivered is still present when Run returns (fixes finding #1 — truncation race).
//
// Ordering is enforced without time.Sleep:
//  1. Attach goroutine writes "early-line\n", closes attachReadyCh.
//  2. Test observes attachReadyCh closed → sends to delayPayloadCh (releasing the goroutine).
//  3. Attach goroutine closes waitAfterAttachCh (unblocking ContainerWait / Run's wait-select),
//     THEN writes "tail-line\n", THEN closes the pipe writer.
//
// Closing waitAfterAttachCh BEFORE writing "tail-line" is deliberate: net.Pipe
// writes are synchronous (block until the reader consumes the data), so writing
// "tail-line" first would cause io.Copy to consume it before the wait result
// fires — the drain would then be a no-op and the test would be a false positive.
// By firing the wait result while the pipe is still empty, Run enters the drain
// select with nothing yet available; "tail-line" arrives only afterwards.
// Without the drain select Run returns immediately (missing "tail-line");
// with the drain it waits for io.Copy to finish → "tail-line" is present.
func TestRun_OutputDrain(t *testing.T) {
	attachReadyCh := make(chan struct{})
	waitReleaseCh := make(chan struct{})
	delayGate := make(chan struct{}, 1)

	fc := &fakeClient{
		waitResult:        container.WaitResponse{StatusCode: 0},
		attachPayload:     "early-line\n",
		delayedPayload:    "tail-line\n",
		delayPayloadCh:    delayGate,
		attachReadyCh:     attachReadyCh,
		waitAfterAttachCh: waitReleaseCh,
	}

	var buf bytes.Buffer
	d := newDockerWithClient(t, fc,
		WithTTYCheck(alwaysTTY),
		WithRawMode(noopMakeRaw),
		WithStreams(bytes.NewReader(nil), &buf),
	)

	done := make(chan error, 1)
	go func() {
		done <- d.Run(context.Background(), sampleSpec())
	}()

	// Wait until the early payload has been written. At this point the attach
	// goroutine is blocked on delayPayloadCh; ContainerWait is blocked on
	// waitReleaseCh. Run is therefore blocked in its ContainerWait select.
	select {
	case <-attachReadyCh:
	case <-time.After(5 * time.Second):
		t.Fatal("attach goroutine did not signal readiness within 5s")
	}

	// Release the delayed payload. The attach goroutine will:
	//   (a) close waitReleaseCh — unblocking ContainerWait so Run receives the wait result
	//       and enters the drain select while the pipe is still empty,
	//   (b) write "tail-line" into the pipe (io.Copy can now consume it),
	//   (c) close the pipe writer (EOF → outputDone fires → Run returns).
	// Without the drain select Run would return after (a) before (b) completes,
	// missing "tail-line".  With the drain it waits for outputDone.
	delayGate <- struct{}{}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s")
	}

	got := buf.String()
	if !strings.Contains(got, "early-line") {
		t.Errorf("stdout missing 'early-line'; got %q", got)
	}
	if !strings.Contains(got, "tail-line") {
		t.Errorf("stdout missing 'tail-line' (drain race); got %q", got)
	}
}

// TestRun_FastExit verifies that a container that exits immediately (wait result
// available before any output delay) still returns the correct exit code.
func TestRun_FastExit(t *testing.T) {
	fc := &fakeClient{waitResult: container.WaitResponse{StatusCode: 7}}
	d := newDockerWithClient(t, fc,
		WithTTYCheck(alwaysTTY),
		WithRawMode(noopMakeRaw),
		WithStreams(bytes.NewReader(nil), io.Discard),
	)

	err := d.Run(context.Background(), sampleSpec())
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected *ExitError, got %T: %v", err, err)
	}
	if ee.Code != 7 {
		t.Errorf("ExitError.Code = %d, want 7", ee.Code)
	}
}

// blockingWaitClient's ContainerWait blocks until the test context is cancelled.
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
	case <-b.startedCh: // already closed — no-op
	default:
		close(b.startedCh)
	}
	return moby.ContainerStartResult{}, nil
}

// d.Close() must propagate to the underlying apiClient to release the transport.
func TestDocker_Close_PropagatedToClient(t *testing.T) {
	fc := &fakeClient{}
	d, err := New(WithClient(fc))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("d.Close() unexpected error: %v", err)
	}
	if !fc.closed {
		t.Error("d.Close() must propagate to the underlying apiClient")
	}
}

// TestWithStreams verifies that WithStreams injects custom stdin/stdout into the Docker struct.
func TestWithStreams_FieldsSet(t *testing.T) {
	in := bytes.NewReader([]byte("hello"))
	var out bytes.Buffer
	fc := &fakeClient{}
	d := newDockerWithClient(t, fc, WithStreams(in, &out))
	if d.stdin != in {
		t.Error("WithStreams did not set stdin field")
	}
	if d.stdout != &out {
		t.Error("WithStreams did not set stdout field")
	}
}

// trackingReadCloser wraps an io.ReadCloser and records whether Close was called.
// Used to verify the stdin goroutine was joined before Run returned.
type trackingReadCloser struct {
	rc      io.ReadCloser
	closed  bool
	closeCh chan struct{} // closed when Close() is invoked
}

func newTrackingReadCloser(rc io.ReadCloser) *trackingReadCloser {
	return &trackingReadCloser{rc: rc, closeCh: make(chan struct{})}
}

func (t *trackingReadCloser) Read(p []byte) (int, error) {
	return t.rc.Read(p)
}

func (t *trackingReadCloser) Close() error {
	t.closed = true
	close(t.closeCh)
	return t.rc.Close()
}

// TestRun_StdinJoin verifies that the stdin copy goroutine is joined before Run
// returns (fixing finding #3 — goroutine leak). The test uses a pipe-backed
// stdin (implements io.Closer) injected via WithStreams. The pipe read side blocks
// indefinitely — the only way Run can return is if it closes the reader (unblocking
// the copy goroutine) and then waits for stdinDone.
//
// pw (the write end) is kept open for the duration of the test so that the read
// side blocks on a real read rather than returning io.EOF immediately. Closing it
// only after Run returns ensures the only unblock path is runContainer's
// ps.closer.Close() call, not a coincidental write-error from an already-closed pw.
func TestRun_StdinJoin(t *testing.T) {
	fc := &fakeClient{waitResult: container.WaitResponse{StatusCode: 0}}

	// pipe: pr blocks on Read until closed. Keep pw open so stdinDone can only
	// be closed by runContainer closing ps.closer, not by pw going away.
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() }) // release pw after the test
	tracker := newTrackingReadCloser(pr)

	d := newDockerWithClient(t, fc,
		WithTTYCheck(alwaysTTY),
		WithRawMode(noopMakeRaw),
		WithStreams(tracker, io.Discard),
	)

	done := make(chan error, 1)
	go func() {
		done <- d.Run(context.Background(), sampleSpec())
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s")
	}

	// The stdin reader must have been closed before Run returned.
	if !tracker.closed {
		t.Error("stdin reader Close() was not called — stdin goroutine was not joined")
	}
}

// TestNewPollableStdin_DoesNotAlterFd0 is the regression test for the
// frozen-TUI bug: the previous implementation set O_NONBLOCK on fd 0, which —
// because a terminal's fd 0/1/2 share one open file description — silently
// made os.Stdout non-blocking and killed the output pump with EAGAIN on the
// first full-screen redraw. Whatever path newPollableStdin takes (fresh
// /dev/tty handle, or any fallback), fd 0's flags must be untouched.
func TestNewPollableStdin_DoesNotAlterFd0(t *testing.T) {
	getFlags := func() int {
		fl, _, errno := syscall.Syscall(syscall.SYS_FCNTL, 0, syscall.F_GETFL, 0)
		if errno != 0 {
			t.Skipf("fcntl(0, F_GETFL) failed: %v", errno)
		}
		return int(fl)
	}

	before := getFlags()
	ps := newPollableStdin(os.Stdin)
	if ps.closer != nil {
		defer ps.closer.Close() //nolint:errcheck // test cleanup
		if ps.handle == os.Stdin {
			t.Error("pollable handle is os.Stdin itself — closing it would close fd 0")
		}
	}
	if after := getFlags(); after != before {
		t.Errorf("newPollableStdin changed fd 0 flags: before=%#x after=%#x", before, after)
	}
}

// noCloseReader is an io.Reader without io.Closer. Used to test the fallback
// (leaked goroutine) path in newPollableStdin.
type noCloseReader struct{ r io.Reader }

func (n *noCloseReader) Read(p []byte) (int, error) { return n.r.Read(p) }

// TestRun_StdinFallback verifies that Run still returns correctly when the stdin
// reader has no io.Closer (the documented leaked-goroutine fallback path).
// The reader is a bytes.NewReader wrapped to strip io.Closer, so the stdin copy
// goroutine exits naturally on EOF — no join is attempted.
func TestRun_StdinFallback(t *testing.T) {
	fc := &fakeClient{waitResult: container.WaitResponse{StatusCode: 0}}

	// A non-closeable reader that EOFs immediately so the goroutine exits on its own.
	in := &noCloseReader{r: bytes.NewReader(nil)}

	d := newDockerWithClient(t, fc,
		WithTTYCheck(alwaysTTY),
		WithRawMode(noopMakeRaw),
		WithStreams(in, io.Discard),
	)

	done := make(chan error, 1)
	go func() {
		done <- d.Run(context.Background(), sampleSpec())
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run (fallback path) returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s on fallback path")
	}
}
