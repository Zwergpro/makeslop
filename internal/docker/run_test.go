package docker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
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

// closeWriteConn wraps a net.Conn and implements CloseWriter, recording whether
// CloseWrite was called. It is used by fakeClient to assert that the stdin
// goroutine propagates stdin EOF to the container (finding #3 fix).
// The pointer to closeWriteCalled is owned by fakeClient.
type closeWriteConn struct {
	net.Conn
	closeWriteCalled *bool
}

func (c *closeWriteConn) CloseWrite() error {
	*c.closeWriteCalled = true
	return nil
}

// fakeClient scripts the ContainerWait result and can fail ContainerCreate,
// ContainerAttach, or ContainerStart, recording which calls were made and their
// order. Methods without test logic come from the embedded noopClient.
type fakeClient struct {
	noopClient
	createErr        error // if non-nil, ContainerCreate returns this
	attachErr        error // if non-nil, ContainerAttach returns this
	startErr         error // if non-nil, ContainerStart returns this
	waitResult       container.WaitResponse
	waitErr          error  // if non-nil, sent on the error channel
	attachPayload    string // data that appears on the container's stdout
	closeWriteCalled bool   // set when CloseWrite() is called on the attach conn

	// waitCtx is the context captured by ContainerWait. Tests use it to verify
	// that the wait context is cancelled on early-return paths (finding #4 fix).
	waitCtx context.Context

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

	// attachPW is the write end of the net.Pipe used by ContainerAttach. When set,
	// ContainerRemove closes it so the post-remove outputDone drain can complete
	// (finding #5 fix). Without this, the drain would block forever waiting for the
	// attach stream to EOF, since the fake never kills the container on remove.
	attachPW net.Conn

	// keepAttachOpen, when true, prevents ContainerAttach's goroutine from closing
	// pw automatically. The pipe writer stays open until ContainerRemove calls
	// attachPW.Close(). Used by tests that need the attach stream to stay open until
	// a force-remove is observed, to verify the finding #5 fix.
	keepAttachOpen bool

	created     bool
	attached    bool
	wasStarted  bool
	removed     bool
	removeForce bool // true if ContainerRemove was called with Force: true
	closed      bool

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

	// Store pw so ContainerRemove can close it, simulating the daemon killing the
	// container (which causes the attach stream to EOF). Required for finding #5:
	// without this, the post-remove outputDone drain would block forever.
	f.attachPW = pw

	// Wrap pr with a CloseWriter so att.CloseWrite() calls are recorded.
	cwConn := &closeWriteConn{Conn: pr, closeWriteCalled: &f.closeWriteCalled}

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
		// When keepAttachOpen is true, leave pw open — only ContainerRemove will close it.
		// This lets tests verify the finding #5 fix: force-remove → attach EOF → drain done.
		keepOpen := f.keepAttachOpen
		go func() {
			if f.attachPayload != "" {
				_, _ = io.WriteString(pw, f.attachPayload)
			}
			if !keepOpen {
				_ = pw.Close()
			}
		}()
	}

	hr := moby.NewHijackedResponse(cwConn, "")
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

func (f *fakeClient) ContainerWait(ctx context.Context, _ string, _ moby.ContainerWaitOptions) moby.ContainerWaitResult {
	f.callOrder = append(f.callOrder, "ContainerWait")
	f.waitCtx = ctx // capture for test assertions (finding #4)
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
	f.callOrder = append(f.callOrder, "ContainerResize")
	return moby.ContainerResizeResult{}, nil
}

func (f *fakeClient) ContainerRemove(_ context.Context, _ string, opts moby.ContainerRemoveOptions) (moby.ContainerRemoveResult, error) {
	if opts.Force {
		f.removeForce = true
	}
	f.removed = true
	// Close the attach pipe writer to simulate the daemon killing the container on
	// remove. This causes the attach stream (read by the output goroutine) to EOF,
	// which allows the post-remove outputDone drain to complete (finding #5).
	// Idempotent: net.Conn.Close() on an already-closed pipe returns an error that
	// is intentionally ignored here — the second remove call (from the deferred
	// force-remove after the wr.Error path) is harmless.
	if f.attachPW != nil {
		_ = f.attachPW.Close() //nolint:errcheck // idempotent teardown
	}
	return moby.ContainerRemoveResult{}, nil
}

func (f *fakeClient) DialHijack(_ context.Context, _, _ string, _ map[string][]string) (net.Conn, error) {
	return nil, errors.New("DialHijack not implemented in fakeClient")
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
	rc     io.ReadCloser
	closed bool
}

func newTrackingReadCloser(rc io.ReadCloser) *trackingReadCloser {
	return &trackingReadCloser{rc: rc}
}

func (t *trackingReadCloser) Read(p []byte) (int, error) {
	return t.rc.Read(p)
}

func (t *trackingReadCloser) Close() error {
	t.closed = true
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
// only after Run returns ensures the only unblock path is Run's
// ps.closer.Close() call, not a coincidental write-error from an already-closed pw.
func TestRun_StdinJoin(t *testing.T) {
	fc := &fakeClient{waitResult: container.WaitResponse{StatusCode: 0}}

	// pipe: pr blocks on Read until closed. Keep pw open so stdinDone can only
	// be closed by Run closing ps.closer, not by pw going away.
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

// TestRun_StdinCloseWrite verifies that att.CloseWrite() is called after the
// stdin copy ends (fixing finding #3 — containers reading stdin to EOF hang).
// The test uses a pipe-backed stdin (implements io.Closer) injected via
// WithStreams, so the stdin goroutine is joinable. The pipe write end is closed
// immediately so stdin reaches EOF and the copy goroutine calls CloseWrite
// before closing stdinDone. Because fakeClient wraps pr with closeWriteConn
// (implements CloseWriter), att.CloseWrite() records the call — a no-op conn
// would never set the flag.
func TestRun_StdinCloseWrite(t *testing.T) {
	fc := &fakeClient{waitResult: container.WaitResponse{StatusCode: 0}}

	// pr EOFs immediately when pw is closed; pw is closed right away so the
	// stdin copy goroutine exits naturally and calls CloseWrite.
	pr, pw := io.Pipe()
	_ = pw.Close() // EOF on first Read

	d := newDockerWithClient(t, fc,
		WithTTYCheck(alwaysTTY),
		WithRawMode(noopMakeRaw),
		WithStreams(pr, io.Discard),
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

	if !fc.closeWriteCalled {
		t.Error("att.CloseWrite() was not called — stdin EOF was not propagated to the container")
	}
}

// TestRun_WaitCtx_CancelledOnStartFailure verifies that the wait context is
// cancelled when ContainerStart fails (finding #4 fix). The fakeClient records
// the context passed to ContainerWait; after Run returns the start error the
// test checks that the captured context is Done.
func TestRun_WaitCtx_CancelledOnStartFailure(t *testing.T) {
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

	// The wait context must be captured and cancelled by the time Run returns.
	if fc.waitCtx == nil {
		t.Fatal("fakeClient.waitCtx was never set — ContainerWait not called")
	}
	select {
	case <-fc.waitCtx.Done():
		// expected: waitCancel() fired on start-failure return path
	default:
		t.Error("wait context was not cancelled after ContainerStart failure (finding #4)")
	}
}

// TestRun_WaitCtx_HappyPath verifies that wait context cancellation does not
// affect normal operation: the exit code is still mapped correctly even though
// defer waitCancel() fires after the wait result is received.
func TestRun_WaitCtx_HappyPath(t *testing.T) {
	fc := &fakeClient{waitResult: container.WaitResponse{StatusCode: 42}}
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
	if ee.Code != 42 {
		t.Errorf("ExitError.Code = %d, want 42", ee.Code)
	}
}

// TestRun_StdinCloseWrite_JoinPath verifies the closer-based join path: a
// blocking pipe stdin is closed by Run to unblock the stdin goroutine;
// CloseWrite is still called before stdinDone closes, and Run returns cleanly
// without panic (CloseWrite fires before att.Conn.Close from the deferred close).
func TestRun_StdinCloseWrite_JoinPath(t *testing.T) {
	fc := &fakeClient{waitResult: container.WaitResponse{StatusCode: 0}}

	// pr blocks until Run calls ps.closer.Close(). pw kept open so
	// only Run's closer.Close() unblocks the read.
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })

	d := newDockerWithClient(t, fc,
		WithTTYCheck(alwaysTTY),
		WithRawMode(noopMakeRaw),
		WithStreams(pr, io.Discard),
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

	if !fc.closeWriteCalled {
		t.Error("att.CloseWrite() was not called on the join path")
	}
}

// TestRun_WaitError_ForceRemoveAndReturn verifies finding #5 fix: when
// ContainerWait delivers an error while the attach stream is still open (i.e.
// the container is still running), Run must:
//   - call ContainerRemove with Force: true (kills the container)
//   - have that removal cause the attach stream to EOF (simulated by
//     fakeClient.ContainerRemove closing attachPW)
//   - drain outputDone after the removal (so no goroutine outlives the call)
//   - return the "container wait: …" error promptly, not hang
//
// Without the fix, the drain blocks forever because the attach stream never
// EOFs — the deferred force-remove is also never reached because startedCleanly
// is true and ctx.Err() is nil.
func TestRun_WaitError_ForceRemoveAndReturn(t *testing.T) {
	waitErr := errors.New("daemon connection lost")
	fc := &fakeClient{
		waitErr:        waitErr,
		keepAttachOpen: true, // simulate container still running when wait errors
	}
	d := newDockerWithClient(t, fc,
		WithTTYCheck(alwaysTTY),
		WithRawMode(noopMakeRaw),
		WithStreams(bytes.NewReader(nil), io.Discard),
	)

	done := make(chan error, 1)
	go func() {
		done <- d.Run(context.Background(), sampleSpec())
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error from wr.Error, got nil")
		}
		if !strings.Contains(err.Error(), "container wait") {
			t.Errorf("expected 'container wait' in error, got %v", err)
		}
		if !strings.Contains(err.Error(), "daemon connection lost") {
			t.Errorf("expected original error in message, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s — drain likely blocked (finding #5 not fixed)")
	}

	if !fc.removed {
		t.Error("ContainerRemove must be called on wr.Error path")
	}
	if !fc.removeForce {
		t.Error("ContainerRemove must be called with Force: true on wr.Error path")
	}
}

// TestRun_WaitError_OutputFlushed verifies that output written to the attach
// stream before the wait error arrives is still flushed to stdout (the drain
// must happen even on the error path). The attach pipe is closed before the wait
// error fires — output is in the bufio.Reader by then — and ContainerRemove's
// second close of pw is a harmless no-op.
//
// Ordering (deterministic, no time.Sleep):
//  1. Attach goroutine writes "output-line\n" and closes pw immediately.
//  2. io.Copy (output goroutine) reads it, then gets EOF → outputDone fires.
//  3. Test observes attachReadyCh signal, then closes waitReleaseCh to unblock
//     ContainerWait (fire the wait error).
//  4. wr.Error branch: calls ContainerRemove (pw already closed → no-op),
//     drains outputDone (already closed → immediate), returns error.
//  5. buf must contain "output-line\n".
func TestRun_WaitError_OutputFlushed(t *testing.T) {
	waitErr := errors.New("wait error")
	waitReleaseCh := make(chan struct{})

	// Ordering guarantee without time.Sleep: use a wrapping writer that signals
	// when the first write arrives, then close waitReleaseCh. This ensures the
	// output goroutine has consumed the payload before the wait error fires.
	signalWriter := &signalOnFirstWrite{w: new(bytes.Buffer), ready: make(chan struct{})}

	fc := &fakeClient{
		waitErr:           waitErr,
		attachPayload:     "output-line\n",
		waitAfterAttachCh: waitReleaseCh,
	}

	d := newDockerWithClient(t, fc,
		WithTTYCheck(alwaysTTY),
		WithRawMode(noopMakeRaw),
		WithStreams(bytes.NewReader(nil), signalWriter),
	)

	done := make(chan error, 1)
	go func() {
		done <- d.Run(context.Background(), sampleSpec())
	}()

	// Wait until the output goroutine has written at least one byte to stdout.
	// At this point the payload is in the writer and outputDone is about to fire
	// (or has already fired). Now release the wait error.
	select {
	case <-signalWriter.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("output goroutine did not write to stdout within 5s")
	}
	close(waitReleaseCh)

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error from wr.Error, got nil")
		}
		if !strings.Contains(err.Error(), "container wait") {
			t.Errorf("expected 'container wait' in error, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s")
	}

	if got := signalWriter.w.String(); !strings.Contains(got, "output-line") {
		t.Errorf("output before wait error must be flushed; got %q", got)
	}
}

// signalOnFirstWrite wraps an io.Writer and closes ready on the first Write call.
// Used by TestRun_WaitError_OutputFlushed to know when the output goroutine has
// started copying data — at that point it is safe to release the wait error.
type signalOnFirstWrite struct {
	w     *bytes.Buffer
	ready chan struct{}
	once  bool
}

func (s *signalOnFirstWrite) Write(p []byte) (int, error) {
	if !s.once {
		s.once = true
		close(s.ready)
	}
	return s.w.Write(p)
}

// TestRun_WaitError_StdinJoined verifies that the wr.Error branch joins the stdin
// goroutine when ps.closer is non-nil (i.e. the injected stdin implements io.Closer).
// A blocking io.Pipe read-end is used as stdin so Run can only return if it closes
// the pipe (unblocking the stdin goroutine) and waits for stdinDone. The test also
// asserts Run returns promptly with the wait error and ContainerRemove was called
// with Force: true.
func TestRun_WaitError_StdinJoined(t *testing.T) {
	waitErr := errors.New("wait error: connection lost")
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	trc := newTrackingReadCloser(pr)

	fc := &fakeClient{
		waitErr:        waitErr,
		keepAttachOpen: true, // attach stream stays open; ContainerRemove closes it
	}
	d := newDockerWithClient(t, fc,
		WithTTYCheck(alwaysTTY),
		WithRawMode(noopMakeRaw),
		WithStreams(trc, io.Discard),
	)

	done := make(chan error, 1)
	go func() {
		done <- d.Run(context.Background(), sampleSpec())
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error from wr.Error path, got nil")
		}
		if !strings.Contains(err.Error(), "container wait") {
			t.Errorf("expected 'container wait' in error, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s — stdin join likely blocked")
	}

	if !fc.removed {
		t.Error("ContainerRemove must be called on wr.Error path")
	}
	if !fc.removeForce {
		t.Error("ContainerRemove must be called with Force: true on wr.Error path")
	}
	if !trc.closed {
		t.Error("stdin pipe read-end must be closed to join the stdin goroutine")
	}
}

// TestRun_SIGWINCHGoroutineJoined verifies that the SIGWINCH resize goroutine is
// joined before Run returns (finding #6 fix). The test sends SIGWINCH
// signals in a tight loop while Run is executing and checks:
//  1. Run returns cleanly (no race-detected concurrent access).
//  2. The resize goroutine completed before Run returned, even in CI where
//     term.GetSize fails and ContainerResize is never called.
//
// Join verification: WithResizeGoroutineHook injects a callback that is called
// at the end of the goroutine body (before closing resizeDone). If the defer in
// run.go omits <-resizeDone, Run may return while the goroutine is still
// running — the hook will not have been called yet, and the assertion below will
// fail. This is deterministic regardless of whether ContainerResize is called
// (i.e. regardless of whether term.GetSize succeeds, which it never does in CI).
//
// POSIX-only: SIGWINCH is not defined on Windows.
func TestRun_SIGWINCHGoroutineJoined(t *testing.T) {
	skipNonPOSIX(t, "SIGWINCH is POSIX-only; makeslop is POSIX-only")

	// goroutineDone is closed by the hook when the resize goroutine body finishes.
	// The hook is called before close(resizeDone) inside the goroutine, so if
	// Run's defer correctly waits on <-resizeDone, the hook always fires before
	// Run returns.
	goroutineDone := make(chan struct{})
	hook := sync.Once{}
	hookFn := func() { hook.Do(func() { close(goroutineDone) }) }

	fc := &fakeClient{waitResult: container.WaitResponse{StatusCode: 0}}
	d := newDockerWithClient(t, fc,
		WithTTYCheck(alwaysTTY),
		WithRawMode(noopMakeRaw),
		WithStreams(bytes.NewReader(nil), io.Discard),
		WithResizeGoroutineHook(hookFn),
	)

	// Send SIGWINCH signals in a tight loop until Run returns.
	stopSending := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopSending:
				return
			default:
				_ = syscall.Kill(os.Getpid(), syscall.SIGWINCH)
			}
		}
	}()

	done := make(chan error, 1)
	go func() {
		done <- d.Run(context.Background(), sampleSpec())
	}()

	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s")
	}
	close(stopSending)

	if runErr != nil {
		t.Fatalf("Run returned unexpected error: %v", runErr)
	}

	// Assert the resize goroutine completed before Run returned. goroutineDone is
	// already closed if the hook fired (which it must have if Run joined correctly).
	// A non-blocking select distinguishes "already closed" from "not yet closed".
	select {
	case <-goroutineDone:
		// goroutine finished before Run returned — join is working correctly
	default:
		t.Error("resize goroutine was not joined before Run returned: hook was not called")
	}
}
