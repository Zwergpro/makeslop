package cli

import (
	"strings"
	"testing"
)

func TestVersion_PrintsVersionAndExitsZero(t *testing.T) {
	// Mutates the package-level version var; must not run parallel with other
	// tests touching it.
	orig := version
	version = "test-1.2.3"
	t.Cleanup(func() { version = orig })

	baseDir := t.TempDir()

	stdout, stderr, err := runCmd(t, baseDir, "version")
	if err != nil {
		t.Fatalf("makeslop version failed: %v; stderr=%q", err, stderr)
	}
	if stdout != "test-1.2.3\n" {
		t.Errorf("stdout = %q, want %q", stdout, "test-1.2.3\n")
	}
	if stderr != "" {
		t.Errorf("expected empty stderr, got %q", stderr)
	}
}

// version is exempt from the home-dir guard.
func TestVersion_ExemptFromHomeDirGuard(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", evalSymlinks(t, tmpHome))

	outsidePwd := t.TempDir()
	t.Chdir(outsidePwd)

	baseDir := t.TempDir()

	stdout, stderr, err := runCmd(t, baseDir, "version")
	if err != nil {
		t.Fatalf("makeslop version must succeed outside HOME; err=%v; stderr=%q", err, stderr)
	}
	if strings.Contains(stderr, "refusing to run") {
		t.Errorf("version must not trigger the home-dir guard: stderr=%q", stderr)
	}
	if stdout == "" {
		t.Errorf("version must print a non-empty version string; stdout=%q", stdout)
	}
}

// version is pipe-safe: the real (false) ttyCheck under go test must not block it.
func TestVersion_ExemptFromTTYCheck(t *testing.T) {
	baseDir := t.TempDir()

	stdout, stderr, err := runCmd(t, baseDir, "version")
	if err != nil {
		t.Fatalf("makeslop version must succeed without a TTY; err=%v; stderr=%q", err, stderr)
	}
	if stdout == "" {
		t.Errorf("version must print a non-empty version string; stdout=%q", stdout)
	}
	if stderr != "" {
		t.Errorf("expected empty stderr, got %q", stderr)
	}
}

// --out-of-home is rejected on commands that don't register it.
func TestOutOfHome_RejectedOnVersion(t *testing.T) {
	baseDir := t.TempDir()

	for _, cmd := range [][]string{
		{"version", "--out-of-home"},
		{"migrate", "--out-of-home"},
		{"build", "--out-of-home"},
		{"config", "--out-of-home"},
		{"status", "--out-of-home"},
	} {
		t.Run(cmd[0], func(t *testing.T) {
			_, _, err := runCmd(t, baseDir, cmd...)
			if err == nil {
				t.Fatalf("%v --out-of-home should fail with unknown flag, got nil", cmd[0])
			}
			if !strings.Contains(err.Error(), "unknown flag") && !strings.Contains(err.Error(), "out-of-home") {
				t.Errorf("%v --out-of-home error should mention unknown flag or out-of-home; got: %v", cmd[0], err)
			}
		})
	}
}
