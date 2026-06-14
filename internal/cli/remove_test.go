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

// seedWorkspace registers a workspace entry directly in settings and creates
// the cache dir on disk; returns the workspace name and cache dir path.
func seedWorkspace(t *testing.T, baseDir, path, name string) string {
	t.Helper()
	// Load existing settings (config.Load returns defaults when no file exists yet).
	s, err := config.Load(baseDir)
	if err != nil {
		t.Fatalf("seedWorkspace: config.Load error: %v", err)
	}
	s.Workspaces[path] = config.Workspace{Name: name, CreatedAt: time.Now().UTC()}
	if err := config.Save(baseDir, s); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	cacheDir := filepath.Join(baseDir, config.WorkspacesDir, name)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("create cache dir: %v", err)
	}
	return cacheDir
}

// TestRemove_ExistingWorkspace_RemovesEntryAndCacheDir verifies that removing
// a known workspace deletes both the settings entry and the cache dir, and that
// "removed <name>" appears on stderr.
func TestRemove_ExistingWorkspace_RemovesEntryAndCacheDir(t *testing.T) {
	baseDir := t.TempDir()

	name := "myproject-abc123"
	cacheDir := seedWorkspace(t, baseDir, "/home/user/myproject", name)

	// Confirm cache dir exists before removal.
	if _, err := os.Stat(cacheDir); err != nil {
		t.Fatalf("cache dir should exist before remove: %v", err)
	}

	stdout, stderr, err := runCmd(t, baseDir, "remove", name)
	if err != nil {
		t.Fatalf("remove failed: %v; stderr=%q", err, stderr)
	}
	if stdout != "" {
		t.Errorf("expected empty stdout; got %q", stdout)
	}

	// "removed <name>" must appear on stderr.
	if !strings.Contains(stderr, "removed "+name) {
		t.Errorf("stderr missing 'removed %s'; got %q", name, stderr)
	}

	// Settings entry must be gone.
	s, err := config.Load(baseDir)
	if err != nil {
		t.Fatalf("load settings after remove: %v", err)
	}
	for _, ws := range s.Workspaces {
		if ws.Name == name {
			t.Errorf("settings still contains workspace %q after remove", name)
		}
	}

	// Cache dir must be gone.
	if _, err := os.Stat(cacheDir); !os.IsNotExist(err) {
		t.Errorf("cache dir %q should not exist after remove; err=%v", cacheDir, err)
	}
}

// TestRemove_ExistingWorkspace_QuietSuppressesNotice verifies that --quiet
// suppresses the "removed <name>" stderr notice.
func TestRemove_ExistingWorkspace_QuietSuppressesNotice(t *testing.T) {
	baseDir := t.TempDir()

	name := "myproject-abc456"
	seedWorkspace(t, baseDir, "/home/user/myproject2", name)

	stdout, stderr, err := runCmd(t, baseDir, "--quiet", "remove", name)
	if err != nil {
		t.Fatalf("remove --quiet failed: %v; stderr=%q", err, stderr)
	}
	if stdout != "" {
		t.Errorf("expected empty stdout; got %q", stdout)
	}
	if stderr != "" {
		t.Errorf("--quiet must suppress stderr notice; got %q", stderr)
	}
}

// TestRemove_UnknownName_ExitsNonZeroWithHint verifies that removing an
// unknown workspace name prints the 'run makeslop ls' hint and exits non-zero.
func TestRemove_UnknownName_ExitsNonZeroWithHint(t *testing.T) {
	baseDir := t.TempDir()

	// Seed one workspace so settings.json exists.
	name := "realworkspace-aabbcc"
	seedWorkspace(t, baseDir, "/home/user/realproject", name)

	// Read settings before.
	sBefore, err := config.Load(baseDir)
	if err != nil {
		t.Fatalf("load before: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runWithExitCode(baseDir, &stdout, &stderr, []string{"remove", "does-not-exist"}, nil)
	if code == 0 {
		t.Fatalf("remove unknown name should exit non-zero; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	// Hint must appear on stderr.
	if !strings.Contains(stderr.String(), "does-not-exist") {
		t.Errorf("stderr missing the name %q; got %q", "does-not-exist", stderr.String())
	}
	if !strings.Contains(stderr.String(), "makeslop ls") {
		t.Errorf("stderr missing 'makeslop ls' hint; got %q", stderr.String())
	}

	// Settings must be unchanged.
	sAfter, err := config.Load(baseDir)
	if err != nil {
		t.Fatalf("load after: %v", err)
	}
	if len(sBefore.Workspaces) != len(sAfter.Workspaces) {
		t.Errorf("settings changed: before %d entries, after %d entries",
			len(sBefore.Workspaces), len(sAfter.Workspaces))
	}
}

// TestRemove_IdempotentFS_CacheDirAlreadyGone verifies that remove succeeds
// even when the cache dir was already deleted manually.
func TestRemove_IdempotentFS_CacheDirAlreadyGone(t *testing.T) {
	baseDir := t.TempDir()

	name := "myproject-dd1122"
	cacheDir := seedWorkspace(t, baseDir, "/home/user/myproject3", name)

	// Delete the cache dir manually before running remove.
	if err := os.RemoveAll(cacheDir); err != nil {
		t.Fatalf("pre-delete cache dir: %v", err)
	}

	_, stderr, err := runCmd(t, baseDir, "remove", name)
	if err != nil {
		t.Fatalf("remove with already-gone cache dir should succeed: %v; stderr=%q", err, stderr)
	}

	// Settings entry must be gone.
	s, err := config.Load(baseDir)
	if err != nil {
		t.Fatalf("load settings after remove: %v", err)
	}
	for _, ws := range s.Workspaces {
		if ws.Name == name {
			t.Errorf("settings still contains workspace %q after remove", name)
		}
	}
}

// TestRemove_RmAlias_Works verifies that the "rm" alias resolves to the same
// remove command.
func TestRemove_RmAlias_Works(t *testing.T) {
	baseDir := t.TempDir()

	name := "aliasproject-ee3344"
	cacheDir := seedWorkspace(t, baseDir, "/home/user/aliasproject", name)

	_, stderr, err := runCmd(t, baseDir, "rm", name)
	if err != nil {
		t.Fatalf("rm alias failed: %v; stderr=%q", err, stderr)
	}
	if !strings.Contains(stderr, "removed "+name) {
		t.Errorf("rm alias: stderr missing 'removed %s'; got %q", name, stderr)
	}

	// Settings entry must be gone.
	s, err := config.Load(baseDir)
	if err != nil {
		t.Fatalf("load settings after rm: %v", err)
	}
	for _, ws := range s.Workspaces {
		if ws.Name == name {
			t.Errorf("settings still contains workspace %q after rm alias", name)
		}
	}

	// Cache dir must be gone.
	if _, err := os.Stat(cacheDir); !os.IsNotExist(err) {
		t.Errorf("cache dir %q should not exist after rm alias; err=%v", cacheDir, err)
	}
}
