package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Zwergpro/makeslop/internal/config"
)

// TestLs_EmptyRegistry_NudgeOnStderr verifies that an empty workspace registry
// prints the "no workspaces registered" nudge to stderr and leaves stdout empty.
func TestLs_EmptyRegistry_NudgeOnStderr(t *testing.T) {
	baseDir := t.TempDir()

	stdout, stderr, err := runCmd(t, baseDir, "ls")
	if err != nil {
		t.Fatalf("ls with empty registry should exit 0, got err: %v; stderr=%q", err, stderr)
	}
	if stdout != "" {
		t.Errorf("expected empty stdout for empty registry; got %q", stdout)
	}
	if !strings.Contains(stderr, "no workspaces registered") {
		t.Errorf("stderr missing 'no workspaces registered'; got %q", stderr)
	}
	if !strings.Contains(stderr, "makeslop init") {
		t.Errorf("stderr missing 'makeslop init' hint; got %q", stderr)
	}
}

// TestLs_EmptyRegistry_QuietSuppressesNudge verifies that --quiet suppresses
// the stderr nudge when the registry is empty; stdout stays empty.
func TestLs_EmptyRegistry_QuietSuppressesNudge(t *testing.T) {
	baseDir := t.TempDir()

	stdout, stderr, err := runCmd(t, baseDir, "--quiet", "ls")
	if err != nil {
		t.Fatalf("ls --quiet with empty registry should exit 0, got err: %v; stderr=%q", err, stderr)
	}
	if stdout != "" {
		t.Errorf("expected empty stdout; got %q", stdout)
	}
	if stderr != "" {
		t.Errorf("--quiet must suppress stderr nudge; got %q", stderr)
	}
}

// TestLs_MultipleWorkspaces_SortedByName verifies that multiple registered
// workspaces are listed sorted by name with all three columns populated.
func TestLs_MultipleWorkspaces_SortedByName(t *testing.T) {
	baseDir := t.TempDir()

	// Seed the settings with two workspaces (non-alphabetical order in the map
	// to confirm the output is sorted).
	createdAt := time.Date(2025, 3, 15, 12, 30, 0, 0, time.UTC)
	s := &config.Settings{
		Version:    config.ConfigVersion,
		Image:      config.DefaultImage,
		Shell:      config.DefaultShell,
		TmpDirSize: config.DefaultTmpDirSize,
		Workspaces: map[string]config.Workspace{
			"/home/user/project-b": {Name: "project-b-abc123", CreatedAt: createdAt},
			"/home/user/project-a": {Name: "project-a-def456", CreatedAt: createdAt.Add(time.Hour)},
		},
	}
	if err := config.Save(baseDir, s); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	stdout, stderr, err := runCmd(t, baseDir, "ls")
	if err != nil {
		t.Fatalf("ls failed: %v; stderr=%q", err, stderr)
	}
	if stderr != "" {
		t.Errorf("expected empty stderr; got %q", stderr)
	}

	// Header must appear.
	if !strings.Contains(stdout, "NAME") {
		t.Errorf("stdout missing NAME header; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "PATH") {
		t.Errorf("stdout missing PATH header; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "CREATED") {
		t.Errorf("stdout missing CREATED header; got:\n%s", stdout)
	}

	// Both workspace names must appear.
	if !strings.Contains(stdout, "project-a-def456") {
		t.Errorf("stdout missing 'project-a-def456'; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "project-b-abc123") {
		t.Errorf("stdout missing 'project-b-abc123'; got:\n%s", stdout)
	}

	// Both paths must appear.
	if !strings.Contains(stdout, "/home/user/project-a") {
		t.Errorf("stdout missing '/home/user/project-a'; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "/home/user/project-b") {
		t.Errorf("stdout missing '/home/user/project-b'; got:\n%s", stdout)
	}

	// Both created timestamps should use the expected format.
	if !strings.Contains(stdout, "2025-03-15 12:30 UTC") {
		t.Errorf("stdout missing formatted timestamp '2025-03-15 12:30 UTC'; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "2025-03-15 13:30 UTC") {
		t.Errorf("stdout missing formatted timestamp '2025-03-15 13:30 UTC'; got:\n%s", stdout)
	}

	// Sorted by name: project-a must appear before project-b.
	idxA := strings.Index(stdout, "project-a-def456")
	idxB := strings.Index(stdout, "project-b-abc123")
	if idxA < 0 || idxB < 0 {
		t.Fatalf("one or both workspace entries missing from output:\n%s", stdout)
	}
	if idxA >= idxB {
		t.Errorf("workspaces not sorted by name: project-a@%d must come before project-b@%d\noutput:\n%s", idxA, idxB, stdout)
	}
}

// TestLs_SingleWorkspace_ShowsTableWithEntry verifies that a single registered
// workspace produces the header and exactly one data row with all columns populated.
func TestLs_SingleWorkspace_ShowsTableWithEntry(t *testing.T) {
	baseDir := t.TempDir()

	createdAt := time.Date(2025, 6, 1, 9, 0, 0, 0, time.UTC)
	s := &config.Settings{
		Version:    config.ConfigVersion,
		Image:      config.DefaultImage,
		Shell:      config.DefaultShell,
		TmpDirSize: config.DefaultTmpDirSize,
		Workspaces: map[string]config.Workspace{
			"/home/user/solo": {Name: "solo-aabbcc", CreatedAt: createdAt},
		},
	}
	if err := config.Save(baseDir, s); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	stdout, stderr, err := runCmd(t, baseDir, "ls")
	if err != nil {
		t.Fatalf("ls with single workspace should exit 0, got err: %v; stderr=%q", err, stderr)
	}
	if stderr != "" {
		t.Errorf("expected empty stderr with a registered workspace; got %q", stderr)
	}

	// Header columns.
	for _, col := range []string{"NAME", "PATH", "CREATED"} {
		if !strings.Contains(stdout, col) {
			t.Errorf("stdout missing %q header column; got:\n%s", col, stdout)
		}
	}
	// Data row.
	if !strings.Contains(stdout, "solo-aabbcc") {
		t.Errorf("stdout missing workspace name 'solo-aabbcc'; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "/home/user/solo") {
		t.Errorf("stdout missing path '/home/user/solo'; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "2025-06-01 09:00 UTC") {
		t.Errorf("stdout missing formatted timestamp '2025-06-01 09:00 UTC'; got:\n%s", stdout)
	}
}

// TestLs_CorruptSettings_ReportsError verifies that a corrupt settings.json
// causes ls to exit non-zero with an error on stderr.
func TestLs_CorruptSettings_ReportsError(t *testing.T) {
	baseDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(baseDir, config.SettingsFile), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write corrupt settings: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runWithExitCode(baseDir, &stdout, &stderr, []string{"ls"}, nil)
	if code == 0 {
		t.Fatalf("expected non-zero exit from ls with corrupt settings; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}
