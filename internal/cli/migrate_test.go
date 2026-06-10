package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Zwergpro/makeslop/internal/assets"
)

func TestMigrate_FirstRun_PrintsUpdatedAndWritesDockerfile(t *testing.T) {
	baseDir := t.TempDir()

	stdout, stderr, err := runCmd(t, baseDir, "migrate")
	if err != nil {
		t.Fatalf("migrate failed: %v; stderr=%q", err, stderr)
	}
	if !strings.Contains(stdout, "updated") {
		t.Errorf("stdout missing 'updated': %q", stdout)
	}
	if strings.Contains(stdout, "already up to date") {
		t.Errorf("stdout must not say 'already up to date' on first run: %q", stdout)
	}

	dockerfilePath := filepath.Join(baseDir, "Dockerfile")
	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("Dockerfile not created by migrate: %v", err)
	}
	if !bytes.Equal(data, assets.Dockerfile) {
		t.Errorf("Dockerfile content mismatch: got %d bytes, want %d bytes", len(data), len(assets.Dockerfile))
	}
}

// Second migrate is idempotent: "already up to date", file unchanged.
func TestMigrate_SecondRun_PrintsAlreadyUpToDate(t *testing.T) {
	baseDir := t.TempDir()

	if _, _, err := runCmd(t, baseDir, "migrate"); err != nil {
		t.Fatalf("first migrate failed: %v", err)
	}
	snapBefore := snapshotTree(t, baseDir)

	stdout, stderr, err := runCmd(t, baseDir, "migrate")
	if err != nil {
		t.Fatalf("second migrate failed: %v; stderr=%q", err, stderr)
	}
	if !strings.Contains(stdout, "already up to date") {
		t.Errorf("stdout missing 'already up to date' on second run: %q", stdout)
	}

	snapAfter := snapshotTree(t, baseDir)
	assertSnapshotsEqual(t, snapBefore, snapAfter)
}

// migrate works standalone (no prior init, no pre-created dirs).
func TestMigrate_WithoutPriorInit_SucceedsAndWritesDockerfile(t *testing.T) {
	// Non-existing subdir so migrate must create the directory itself.
	parent := t.TempDir()
	baseDir := filepath.Join(parent, "brand-new-dir")

	stdout, stderr, err := runCmd(t, baseDir, "migrate")
	if err != nil {
		t.Fatalf("migrate without prior init failed: %v; stderr=%q", err, stderr)
	}
	if !strings.Contains(stdout, "updated") {
		t.Errorf("stdout missing 'updated': %q", stdout)
	}

	dockerfilePath := filepath.Join(baseDir, "Dockerfile")
	if _, err := os.Stat(dockerfilePath); err != nil {
		t.Errorf("Dockerfile not created by migrate without prior init: %v", err)
	}
}

func TestMigrate_CorruptSettings_ReportsError(t *testing.T) {
	baseDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(baseDir, "settings.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seed corrupt settings: %v", err)
	}

	stdout, stderr, err := runCmd(t, baseDir, "migrate")
	if err == nil {
		t.Fatalf("expected error from migrate with corrupt settings, got nil; stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(err.Error(), "settings") {
		t.Errorf("expected error to mention 'settings' context, got %q", err.Error())
	}
}
