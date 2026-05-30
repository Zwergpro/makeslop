package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Zwergpro/makeslop/internal/assets"
)

// TestMigrate_FreshDir verifies that Migrate on a fresh directory returns
// applied == true, writes Dockerfile matching assets.Dockerfile, and persists
// migrated_version == MigrationVersion.
func TestMigrate_FreshDir(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".makeslop")

	applied, err := Migrate(base)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if !applied {
		t.Error("Migrate should return applied == true on fresh dir")
	}

	// Dockerfile content must match the embedded asset.
	got, err := os.ReadFile(filepath.Join(base, "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	if !bytes.Equal(got, assets.Dockerfile) {
		t.Errorf("Dockerfile content mismatch: got %d bytes, want %d bytes", len(got), len(assets.Dockerfile))
	}

	// migrated_version must be stamped.
	s, err := Load(base)
	if err != nil {
		t.Fatalf("Load after Migrate: %v", err)
	}
	if s.MigratedVersion != MigrationVersion {
		t.Errorf("MigratedVersion = %d, want %d", s.MigratedVersion, MigrationVersion)
	}
}

// TestMigrate_AlreadyUpToDate verifies that a second Migrate call on an
// already-stamped dir returns applied == false and leaves the Dockerfile
// unchanged.
func TestMigrate_AlreadyUpToDate(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".makeslop")

	// First run applies.
	if _, err := Migrate(base); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(base, "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile after first Migrate: %v", err)
	}

	// Second run should skip.
	applied, err := Migrate(base)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if applied {
		t.Error("second Migrate should return applied == false when already up to date")
	}

	after, err := os.ReadFile(filepath.Join(base, "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile after second Migrate: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("Dockerfile was modified by a no-op Migrate")
	}
}

// TestMigrate_OverwritesEditedDockerfile verifies that Migrate force-overwrites
// a locally-edited Dockerfile when the version is behind.
func TestMigrate_OverwritesEditedDockerfile(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".makeslop")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Plant a sentinel Dockerfile and stamp a low migrated_version.
	sentinel := []byte("# user-edited Dockerfile — must be overwritten by migrate\n")
	if err := os.WriteFile(filepath.Join(base, "Dockerfile"), sentinel, 0o644); err != nil {
		t.Fatalf("seed Dockerfile: %v", err)
	}
	s := &Settings{
		Version:         CurrentVersion,
		Image:           DefaultImage,
		Shell:           DefaultShell,
		Workspaces:      map[string]Workspace{},
		MigratedVersion: 0,
	}
	if err := Save(base, s); err != nil {
		t.Fatalf("Save: %v", err)
	}

	applied, err := Migrate(base)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if !applied {
		t.Error("Migrate should return applied == true when version is behind")
	}

	got, err := os.ReadFile(filepath.Join(base, "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	if bytes.Equal(got, sentinel) {
		t.Error("Migrate did not overwrite the locally-edited Dockerfile")
	}
	if !bytes.Equal(got, assets.Dockerfile) {
		t.Errorf("Dockerfile content mismatch after migrate: got %d bytes, want %d bytes", len(got), len(assets.Dockerfile))
	}
}

// TestMigrate_VersionBehindReRuns verifies that Migrate re-runs (applied ==
// true) and re-stamps to MigrationVersion when MigratedVersion is behind
// MigrationVersion (e.g. 0 on a fresh directory).
func TestMigrate_VersionBehindReRuns(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".makeslop")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	s := &Settings{
		Version:         CurrentVersion,
		Image:           DefaultImage,
		Shell:           DefaultShell,
		Workspaces:      map[string]Workspace{},
		MigratedVersion: 0,
	}
	if err := Save(base, s); err != nil {
		t.Fatalf("Save: %v", err)
	}

	applied, err := Migrate(base)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if !applied {
		t.Errorf("Migrate should return applied == true when MigratedVersion=0 < MigrationVersion=%d", MigrationVersion)
	}

	// Dockerfile must have been written (mirrors TestMigrate_FreshDir assertion).
	got, err := os.ReadFile(filepath.Join(base, "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	if !bytes.Equal(got, assets.Dockerfile) {
		t.Errorf("Dockerfile content mismatch: got %d bytes, want %d bytes", len(got), len(assets.Dockerfile))
	}

	loaded, err := Load(base)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.MigratedVersion != MigrationVersion {
		t.Errorf("MigratedVersion = %d, want %d", loaded.MigratedVersion, MigrationVersion)
	}
}

// TestMigrate_VersionAheadSkips verifies that Migrate is a no-op (applied ==
// false) when MigratedVersion is ahead of MigrationVersion, e.g. after a
// binary downgrade. This prevents re-running migrations that the older binary
// does not know about.
func TestMigrate_VersionAheadSkips(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".makeslop")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	s := &Settings{
		Version:         CurrentVersion,
		Image:           DefaultImage,
		Shell:           DefaultShell,
		Workspaces:      map[string]Workspace{},
		MigratedVersion: 999,
	}
	if err := Save(base, s); err != nil {
		t.Fatalf("Save: %v", err)
	}

	applied, err := Migrate(base)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if applied {
		t.Errorf("Migrate should return applied == false when MigratedVersion=999 > MigrationVersion=%d (downgrade guard)", MigrationVersion)
	}

	// Version must remain unchanged (no downgrade stamp).
	loaded, err := Load(base)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.MigratedVersion != 999 {
		t.Errorf("MigratedVersion = %d, want 999 (downgrade must not re-stamp)", loaded.MigratedVersion)
	}
}

// TestMigrate_NonExistentBaseDirSucceeds verifies that Migrate on a
// non-existent baseDir succeeds (writers call MkdirAll internally).
func TestMigrate_NonExistentBaseDirSucceeds(t *testing.T) {
	base := filepath.Join(t.TempDir(), "does", "not", "exist", ".makeslop")

	applied, err := Migrate(base)
	if err != nil {
		t.Fatalf("Migrate on non-existent dir: %v", err)
	}
	if !applied {
		t.Error("Migrate should return applied == true on non-existent dir")
	}

	if _, err := os.Stat(filepath.Join(base, "Dockerfile")); err != nil {
		t.Errorf("Dockerfile not created in non-existent base dir: %v", err)
	}
}

// TestMigrate_PreservesOtherSettings verifies that Migrate does not clobber
// user-set Image, Shell, Workspaces while stamping migrated_version.
func TestMigrate_PreservesOtherSettings(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".makeslop")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	wantImage := "my-custom-image:v42"
	wantShell := "/bin/fish"
	wantWorkspaces := map[string]Workspace{
		"/projects/alpha": {
			Name:      "alpha-aabbcc",
			CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		},
	}

	s := &Settings{
		Version:         CurrentVersion,
		Image:           wantImage,
		Shell:           wantShell,
		Workspaces:      wantWorkspaces,
		MigratedVersion: 0,
	}
	if err := Save(base, s); err != nil {
		t.Fatalf("Save: %v", err)
	}

	applied, err := Migrate(base)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if !applied {
		t.Error("Migrate should return applied == true")
	}

	got, err := Load(base)
	if err != nil {
		t.Fatalf("Load after Migrate: %v", err)
	}
	if got.Image != wantImage {
		t.Errorf("Image = %q, want %q", got.Image, wantImage)
	}
	if got.Shell != wantShell {
		t.Errorf("Shell = %q, want %q", got.Shell, wantShell)
	}
	if len(got.Workspaces) != len(wantWorkspaces) {
		t.Errorf("Workspaces len = %d, want %d", len(got.Workspaces), len(wantWorkspaces))
	}
	for k, w := range wantWorkspaces {
		g, ok := got.Workspaces[k]
		if !ok {
			t.Errorf("workspace %q missing after Migrate", k)
			continue
		}
		if g.Name != w.Name {
			t.Errorf("workspace %q Name = %q, want %q", k, g.Name, w.Name)
		}
		if !g.CreatedAt.Equal(w.CreatedAt) {
			t.Errorf("workspace %q CreatedAt = %v, want %v", k, g.CreatedAt, w.CreatedAt)
		}
	}
	if got.MigratedVersion != MigrationVersion {
		t.Errorf("MigratedVersion = %d, want %d", got.MigratedVersion, MigrationVersion)
	}
}

// TestSaveLoadByteIdenticalForSameSettings_WithMigratedVersion ensures the
// existing byte-identical invariant still holds when MigratedVersion is set.
func TestSaveLoadByteIdenticalForSameSettings_WithMigratedVersion(t *testing.T) {
	base := t.TempDir()

	s := &Settings{
		Version:         CurrentVersion,
		Image:           DefaultImage,
		Shell:           DefaultShell,
		Workspaces:      map[string]Workspace{},
		MigratedVersion: MigrationVersion,
	}

	if err := Save(base, s); err != nil {
		t.Fatalf("first save: %v", err)
	}
	first, err := os.ReadFile(filepath.Join(base, SettingsFile))
	if err != nil {
		t.Fatalf("read first: %v", err)
	}

	loaded, err := Load(base)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := Save(base, loaded); err != nil {
		t.Fatalf("second save: %v", err)
	}
	second, err := os.ReadFile(filepath.Join(base, SettingsFile))
	if err != nil {
		t.Fatalf("read second: %v", err)
	}

	if !bytes.Equal(first, second) {
		t.Errorf("settings.json bytes differ between equal saves\nfirst:\n%s\nsecond:\n%s", first, second)
	}

	var check Settings
	if err := json.Unmarshal(second, &check); err != nil {
		t.Errorf("saved file is not valid JSON: %v", err)
	}
}

