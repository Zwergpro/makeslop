package security

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// skipNonPOSIX skips the calling test on non-POSIX hosts per the CLAUDE.md
// invariant. why becomes the skip reason so failure logs explain the gate.
func skipNonPOSIX(t *testing.T, why string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip(why)
	}
}

// evalSymlinks resolves symlinks for a temp dir path, matching the precondition
// documented on Scan (and workspace.Lookup). On macOS-style hosts /tmp is
// itself a symlink, so raw t.TempDir() paths violate the precondition.
func evalSymlinks(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("eval symlinks %s: %v", dir, err)
	}
	return resolved
}

// TestScan_FdMissing_ReturnsErrFdMissing verifies that pointing fdBinary to a
// nonexistent path causes Scan to return ErrFdMissing.
func TestScan_FdMissing_ReturnsErrFdMissing(t *testing.T) {
	skipNonPOSIX(t, "shim scripts require POSIX shell; security package is POSIX-only per CLAUDE.md")
	t.Cleanup(SetFdBinaryForTest("/nonexistent/fd-binary"))

	root := evalSymlinks(t, t.TempDir())
	_, err := Scan(context.Background(), root)
	if !errors.Is(err, ErrFdMissing) {
		t.Errorf("Scan with missing fd binary: got err=%v, want errors.Is(err, ErrFdMissing)", err)
	}
}

// TestScan_TwoPaths_ReturnsSorted verifies that two paths returned by the shim
// come back lexicographically sorted regardless of shim output order.
func TestScan_TwoPaths_ReturnsSorted(t *testing.T) {
	skipNonPOSIX(t, "shim scripts require POSIX shell; security package is POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	// Emit paths in reverse alphabetical order; Scan must sort them.
	pathB := filepath.Join(root, "z.env")
	pathA := filepath.Join(root, "a.env")

	shimDir := t.TempDir()
	shim, _ := WriteFdShim(t, shimDir, []string{pathB, pathA})
	t.Cleanup(SetFdBinaryForTest(shim))

	got, err := Scan(context.Background(), root)
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	want := []string{pathA, pathB}
	if len(got) != len(want) {
		t.Fatalf("len(got)=%d, want %d; got=%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

// TestScan_OutsideRootPathDropped verifies that a path outside root is silently
// dropped — Scan is the trust boundary for the external process.
func TestScan_OutsideRootPathDropped(t *testing.T) {
	skipNonPOSIX(t, "shim scripts require POSIX shell; security package is POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())
	outside := evalSymlinks(t, t.TempDir()) // different dir; not under root

	outsidePath := filepath.Join(outside, ".env")
	insidePath := filepath.Join(root, ".env")

	shimDir := t.TempDir()
	shim, _ := WriteFdShim(t, shimDir, []string{outsidePath, insidePath})
	t.Cleanup(SetFdBinaryForTest(shim))

	got, err := Scan(context.Background(), root)
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 path (inside root only), got %d: %v", len(got), got)
	}
	if got[0] != insidePath {
		t.Errorf("got[0]=%q, want %q", got[0], insidePath)
	}
}

// TestScan_EmptyStdout_ReturnsEmptySlice verifies that an empty shim response
// yields a nil/empty slice with no error.
func TestScan_EmptyStdout_ReturnsEmptySlice(t *testing.T) {
	skipNonPOSIX(t, "shim scripts require POSIX shell; security package is POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	shimDir := t.TempDir()
	shim, _ := WriteFdShim(t, shimDir, nil)
	t.Cleanup(SetFdBinaryForTest(shim))

	got, err := Scan(context.Background(), root)
	if err != nil {
		t.Fatalf("Scan returned error for empty output: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty result, got %v", got)
	}
}

// TestScan_NonZeroExit_ReturnsWrappedError verifies that a non-zero shim exit
// produces a wrapped error that is NOT ErrFdMissing.
func TestScan_NonZeroExit_ReturnsWrappedError(t *testing.T) {
	skipNonPOSIX(t, "shim scripts require POSIX shell; security package is POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	// Write a shim that exits 1 — this represents an fd runtime error.
	shimDir := t.TempDir()
	shimPath := filepath.Join(shimDir, "fd-fail")
	script := "#!/bin/sh\nexit 1\n"
	if err := os.WriteFile(shimPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write failing shim: %v", err)
	}
	t.Cleanup(SetFdBinaryForTest(shimPath))

	_, err := Scan(context.Background(), root)
	if err == nil {
		t.Fatal("expected error from non-zero exit shim, got nil")
	}
	if errors.Is(err, ErrFdMissing) {
		t.Errorf("non-zero exit error must not be ErrFdMissing; got %v", err)
	}
	if !strings.Contains(err.Error(), "security: run fd:") {
		t.Errorf("error missing expected prefix; got %q", err.Error())
	}
}

// TestScan_ArgvShape verifies the exact flag sequence passed to the fd binary.
func TestScan_ArgvShape(t *testing.T) {
	skipNonPOSIX(t, "shim scripts require POSIX shell; security package is POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	shimDir := t.TempDir()
	shim, record := WriteFdShim(t, shimDir, nil)
	t.Cleanup(SetFdBinaryForTest(shim))

	if _, err := Scan(context.Background(), root); err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}

	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read argv record: %v", err)
	}
	got := strings.Split(strings.TrimRight(string(data), "\n"), "\n")

	want := []string{
		"--hidden",
		"--no-ignore",
		"--type", "f",
		"--exclude", ".git",
		"--exclude", "node_modules",
		"--exclude", "vendor",
		"--exclude", ".venv",
		"--print0",
		"--regex", `\.env$`,
		"--",
		root,
	}
	if len(got) != len(want) {
		t.Fatalf("argv length mismatch: got %d %v, want %d %v", len(got), got, len(want), want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], w)
		}
	}
}

// TestScan_RealFd_MatchesExpectedFiles runs against the real fd binary (if
// available) and asserts the exact set of .env files returned. Requires "fd"
// on PATH; skips otherwise.
func TestScan_RealFd_MatchesExpectedFiles(t *testing.T) {
	// Try "fd" first, then "fdfind".
	var fdExe string
	for _, name := range []string{"fd", "fdfind"} {
		if resolved, err := exec.LookPath(name); err == nil {
			fdExe = resolved
			break
		}
	}
	if fdExe == "" {
		t.Skip("neither fd nor fdfind found on PATH; skipping real-binary test")
	}

	// Pin the resolved binary path so Scan uses it directly, consistent with
	// the project convention for all tests that exercise the binary swap point.
	t.Cleanup(SetFdBinaryForTest(fdExe))

	root := evalSymlinks(t, t.TempDir())

	// Tree layout (see plan § Task 1):
	mustWriteFile := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	// Included.
	included := []string{
		filepath.Join(root, ".env"),
		filepath.Join(root, "local.env"),
		filepath.Join(root, "sub", "dir", ".env"),
	}
	for _, p := range included {
		mustWriteFile(p, "SECRET=1\n")
	}

	// Excluded by --exclude .git.
	mustWriteFile(filepath.Join(root, ".git", "foo.env"), "SECRET=2\n")

	// Excluded by --exclude node_modules.
	mustWriteFile(filepath.Join(root, "node_modules", "x.env"), "SECRET=3\n")

	// Included despite .gitignore (load-bearing reason for --no-ignore).
	mustWriteFile(filepath.Join(root, ".gitignore"), "ignored.env\n")
	ignored := filepath.Join(root, "ignored.env")
	mustWriteFile(ignored, "SECRET=4\n")
	included = append(included, ignored)

	// NOT included: basename does not end exactly in ".env".
	mustWriteFile(filepath.Join(root, ".env.local"), "SECRET=5\n")

	got, err := Scan(context.Background(), root)
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}

	// Sort both for comparison.
	sortedIncluded := make([]string, len(included))
	copy(sortedIncluded, included)
	sort.Strings(sortedIncluded)

	if len(got) != len(sortedIncluded) {
		t.Fatalf("got %d paths, want %d\ngot:  %v\nwant: %v", len(got), len(sortedIncluded), got, sortedIncluded)
	}
	for i, w := range sortedIncluded {
		if got[i] != w {
			t.Errorf("got[%d]=%q, want %q", i, got[i], w)
		}
	}
}

