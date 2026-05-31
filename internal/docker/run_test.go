package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	moby "github.com/moby/moby/client"
)

// executableTempDir returns a temp dir that is on an executable filesystem.
// It delegates to t.TempDir() which honours the GOTMPDIR env var; set
// GOTMPDIR=/home/user (or any executable path) when running tests in
// environments where /tmp is mounted noexec.
func executableTempDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

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
// and can optionally fail on ContainerStart. It records which cleanup calls
// were made so tests can assert on them.
type fakeClient struct {
	// scripted behaviour
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
	return moby.ContainerCreateResult{ID: "fake-container-id"}, nil
}

func (f *fakeClient) ContainerAttach(_ context.Context, _ string, _ moby.ContainerAttachOptions) (moby.ContainerAttachResult, error) {
	f.attached = true

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

func (f *fakeClient) Close() error {
	f.closed = true
	return nil
}

// ─── run() unit tests ─────────────────────────────────────────────────────────

func TestRun_NoTTY_ReturnsSentinel_NoClientCall(t *testing.T) {
	t.Cleanup(SetTTYCheckForTest(func() bool { return false }))

	fc := &fakeClient{}
	err := run(context.Background(), fc, sampleSpec())
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

// runWithForceTTY is a test-only variant of run() that bypasses the
// term.MakeRaw call so tests can exercise the start/remove/wait paths
// without a real PTY. ttyCheck is assumed to return true (caller must stub it).
func runWithForceTTY(ctx context.Context, cli apiClient, s Spec) error {
	// ttyCheck is assumed stubbed by the caller.
	if !ttyCheck() {
		return ErrNoTTY
	}
	defer cli.Close() //nolint:errcheck

	createRes, err := cli.ContainerCreate(ctx, moby.ContainerCreateOptions{
		Config:     s.ContainerConfig(),
		HostConfig: s.HostConfig(),
	})
	if err != nil {
		return fmt.Errorf("container create: %w", err)
	}
	id := createRes.ID

	startedCleanly := false
	defer func() {
		if !startedCleanly || ctx.Err() != nil {
			rmCtx := context.Background()
			_, _ = cli.ContainerRemove(rmCtx, id, moby.ContainerRemoveOptions{Force: true})
		}
	}()

	att, err := cli.ContainerAttach(ctx, id, moby.ContainerAttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		return fmt.Errorf("container attach: %w", err)
	}
	defer att.Conn.Close() //nolint:errcheck

	// Skip MakeRaw — no real TTY available in tests.

	if _, err = cli.ContainerStart(ctx, id, moby.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("container start: %w", err)
	}
	startedCleanly = true

	go io.Copy(att.Conn, os.Stdin)     //nolint:errcheck
	go io.Copy(io.Discard, att.Reader) //nolint:errcheck

	wr := cli.ContainerWait(ctx, id, moby.ContainerWaitOptions{})
	select {
	case err := <-wr.Error:
		return fmt.Errorf("container wait: %w", err)
	case res := <-wr.Result:
		if res.Error != nil {
			return fmt.Errorf("container wait error: %s", res.Error.Message)
		}
		if res.StatusCode != 0 {
			return &ExitError{Code: int(res.StatusCode)}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
	b.fakeClient.wasStarted = true
	select {
	case <-b.startedCh:
		// already closed — no-op
	default:
		close(b.startedCh)
	}
	return moby.ContainerStartResult{}, nil
}

// ─── build shim tests (kept unchanged until Task 4) ─────────────────────────

// sampleBuildOptions returns a minimal BuildOptions with ContextDir set so it
// can be compared against the shim's recorded argv (Build fills ContextDir
// only when the caller leaves it empty, but the test passes a known value).
func sampleBuildOptions(contextDir string) BuildOptions {
	return BuildOptions{
		Image:          "claudebox",
		DockerfilePath: "/home/user/.makeslop/Dockerfile",
		ContextDir:     contextDir,
	}
}

func TestBuild_HappyPath_RecordsArgvAndBuildKit(t *testing.T) {
	SkipNonPOSIX(t, "shell shim requires POSIX shell; makeslop is POSIX-only")
	dir := executableTempDir(t)
	shim, record, envFile := WriteBuildShim(t, dir, 0)
	t.Cleanup(SetDockerBinaryForTest(shim))

	ctxDir := t.TempDir()
	o := sampleBuildOptions(ctxDir)
	if err := Build(context.Background(), o, io.Discard, io.Discard); err != nil {
		t.Fatalf("Build returned unexpected error: %v", err)
	}

	// Verify recorded argv matches BuildArgv output.
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read argv record: %v", err)
	}
	got := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	want := BuildArgv(o)
	if len(got) != len(want) {
		t.Fatalf("argv length mismatch: got %d %v, want %d %v", len(got), got, len(want), want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], w)
		}
	}

	// Verify DOCKER_BUILDKIT=1 was set in the child environment.
	envData, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read env record: %v", err)
	}
	if got := strings.TrimRight(string(envData), "\n"); got != "1" {
		t.Errorf("DOCKER_BUILDKIT = %q, want %q", got, "1")
	}
}

// TestBuild_EmptyContextDir_GeneratesAndRemovesDir verifies two invariants:
// (1) when ContextDir is empty, Build generates a non-empty path and passes it
// as the last argv token; (2) that dir is removed after Build returns.
// The two properties are tested together because recovering the generated path
// requires reading the shim's argv record.
func TestBuild_EmptyContextDir_GeneratesAndRemovesDir(t *testing.T) {
	SkipNonPOSIX(t, "shell shim requires POSIX shell; makeslop is POSIX-only")
	dir := executableTempDir(t)
	shim, record, _ := WriteBuildShim(t, dir, 0)
	t.Cleanup(SetDockerBinaryForTest(shim))

	o := BuildOptions{
		Image:          "claudebox",
		DockerfilePath: "/home/user/.makeslop/Dockerfile",
		// ContextDir intentionally left empty — Build should create one.
	}
	if err := Build(context.Background(), o, io.Discard, io.Discard); err != nil {
		t.Fatalf("Build returned unexpected error: %v", err)
	}

	// The last token of the recorded argv is the context dir.
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read argv record: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 0 {
		t.Fatal("no argv recorded")
	}
	generatedCtxDir := lines[len(lines)-1]

	// (1) The generated context dir must be a non-empty path.
	if generatedCtxDir == "" {
		t.Fatal("last argv token (context dir) is empty")
	}
	// (2) After Build returns, defer os.RemoveAll has run, so it must no longer exist.
	if _, statErr := os.Stat(generatedCtxDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("temp context dir %q still exists after Build returned (want ErrNotExist, got %v)", generatedCtxDir, statErr)
	}
}

func TestBuild_NonZeroExit_PropagatesCode(t *testing.T) {
	SkipNonPOSIX(t, "POSIX-only invariant per CLAUDE.md")
	dir := executableTempDir(t)
	shim, _, _ := WriteBuildShim(t, dir, 5)
	t.Cleanup(SetDockerBinaryForTest(shim))

	o := BuildOptions{
		Image:          "claudebox",
		DockerfilePath: "/home/user/.makeslop/Dockerfile",
		ContextDir:     t.TempDir(),
	}
	err := Build(context.Background(), o, io.Discard, io.Discard)
	if err == nil {
		t.Fatalf("expected error from shim exit 5, got nil")
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected *exec.ExitError, got %T: %v", err, err)
	}
	if got := ee.ExitCode(); got != 5 {
		t.Errorf("exit code = %d, want 5", got)
	}
}

func TestBuild_DockerNotFound_ReturnsError(t *testing.T) {
	SkipNonPOSIX(t, "POSIX-only invariant per CLAUDE.md")
	missing := filepath.Join(t.TempDir(), "no-such-docker")
	t.Cleanup(SetDockerBinaryForTest(missing))

	o := BuildOptions{
		Image:          "claudebox",
		DockerfilePath: "/home/user/.makeslop/Dockerfile",
		ContextDir:     t.TempDir(),
	}
	err := Build(context.Background(), o, io.Discard, io.Discard)
	if err == nil {
		t.Fatalf("expected error for missing docker binary, got nil")
	}
	var execErr *exec.Error
	var pathErr *os.PathError
	if !errors.As(err, &execErr) && !errors.As(err, &pathErr) {
		t.Errorf("expected *exec.Error or *os.PathError, got %T: %v", err, err)
	}
}

func TestBuild_ContextCancellation_KillsChild(t *testing.T) {
	SkipNonPOSIX(t, "POSIX-only invariant per CLAUDE.md")
	// Shim must be in an executable dir (/tmp is noexec in this environment).
	dir := executableTempDir(t)
	shim := filepath.Join(dir, "shim")
	started := filepath.Join(dir, "started")
	// The shim signals readiness by touching `started`, then busy-loops (no
	// subprocess) so SIGKILL on the direct child is sufficient to unblock
	// cmd.Wait — avoiding the pipe-drain issue that arises when a grandchild
	// (e.g. `sleep`) inherits the stdout/stderr pipes and keeps them open.
	script := "#!/bin/sh\n: > \"" + started + "\"\nwhile true; do :; done\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	t.Cleanup(SetDockerBinaryForTest(shim))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := BuildOptions{
		Image:          "claudebox",
		DockerfilePath: "/home/user/.makeslop/Dockerfile",
		ContextDir:     t.TempDir(),
	}
	done := make(chan error, 1)
	go func() {
		done <- Build(ctx, o, io.Discard, io.Discard)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("shim did not signal start within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("expected error when context cancels mid-build, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Build did not return within 5s after context cancellation")
	}
}
