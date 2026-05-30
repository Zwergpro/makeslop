package docker

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestRun_HappyPath_RecordsArgv(t *testing.T) {
	SkipNonPOSIX(t, "shell shim requires POSIX shell; makeslop is POSIX-only")
	shim, record := WriteShim(t, t.TempDir(), 0)
	t.Cleanup(SetDockerBinaryForTest(shim))
	t.Cleanup(SetTTYCheckForTest(func() bool { return true }))

	spec := sampleSpec()
	if err := Run(context.Background(), spec); err != nil {
		t.Fatalf("Run returned unexpected error: %v", err)
	}

	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read recorded argv: %v", err)
	}
	got := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	want := spec.Args()
	if len(got) != len(want) {
		t.Fatalf("argv length mismatch: got %d %v, want %d %v", len(got), got, len(want), want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], w)
		}
	}
}

func TestRun_NoTTY_ReturnsSentinel_NoExec(t *testing.T) {
	SkipNonPOSIX(t, "POSIX-only invariant per CLAUDE.md")
	// Stub ttyCheck=false explicitly so the test does not depend on whether the
	// process's stdin/stdout happen to be pipes vs terminals.
	t.Cleanup(SetTTYCheckForTest(func() bool { return false }))
	// Point dockerBinary at a non-existent path: if Run accidentally execs, the
	// returned error would be exec.Error (not ErrNoTTY) and the test would fail.
	t.Cleanup(SetDockerBinaryForTest(filepath.Join(t.TempDir(), "no-such-docker")))

	err := Run(context.Background(), sampleSpec())
	if !errors.Is(err, ErrNoTTY) {
		t.Fatalf("expected ErrNoTTY, got %v", err)
	}
}

func TestRun_NonZeroExit_PropagatesCode(t *testing.T) {
	SkipNonPOSIX(t, "POSIX-only invariant per CLAUDE.md")
	shim, _ := WriteShim(t, t.TempDir(), 7)
	t.Cleanup(SetDockerBinaryForTest(shim))
	t.Cleanup(SetTTYCheckForTest(func() bool { return true }))

	err := Run(context.Background(), sampleSpec())
	if err == nil {
		t.Fatalf("expected error from shim exit 7, got nil")
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected *exec.ExitError, got %T: %v", err, err)
	}
	if got := ee.ExitCode(); got != 7 {
		t.Errorf("exit code = %d, want 7", got)
	}
}

func TestRun_DockerNotFound_ReturnsError(t *testing.T) {
	SkipNonPOSIX(t, "POSIX-only invariant per CLAUDE.md")
	missing := filepath.Join(t.TempDir(), "no-such-docker")
	t.Cleanup(SetDockerBinaryForTest(missing))
	t.Cleanup(SetTTYCheckForTest(func() bool { return true }))

	err := Run(context.Background(), sampleSpec())
	if err == nil {
		t.Fatalf("expected error for missing docker binary, got nil")
	}
	if errors.Is(err, ErrNoTTY) {
		t.Fatalf("must not return ErrNoTTY when TTY stub is true: %v", err)
	}
	// Regression guard for the deferred docker-pre-check item: surface SOME
	// error wrapping exec.Error / os.PathError so users see a real diagnostic.
	var execErr *exec.Error
	var pathErr *os.PathError
	if !errors.As(err, &execErr) && !errors.As(err, &pathErr) {
		t.Errorf("expected error to wrap *exec.Error or *os.PathError, got %T: %v", err, err)
	}
}

func TestRun_ContextCancellation_KillsChild(t *testing.T) {
	SkipNonPOSIX(t, "POSIX-only invariant per CLAUDE.md")
	dir := t.TempDir()
	shim := filepath.Join(dir, "shim")
	started := filepath.Join(dir, "started")
	// The shim signals readiness by touching `started` before the long sleep, so
	// the test can cancel exactly once the child is live (no Sleep races).
	script := "#!/bin/sh\n: > \"" + started + "\"\nsleep 10\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	t.Cleanup(SetDockerBinaryForTest(shim))
	t.Cleanup(SetTTYCheckForTest(func() bool { return true }))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, sampleSpec())
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
			t.Fatalf("expected error when context cancels mid-run, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Run did not return within 5s after context cancellation")
	}
}

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
