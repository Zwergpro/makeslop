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

// TestMigrate_FreshDir verifies that Migrate on a clean machine (neither the
// directory nor settings.json exists) bootstraps the directory, returns
// applied == true, writes the Dockerfile, and stamps Version == ConfigVersion.
// The no-file default is Version = 0 < ConfigVersion, so the migration always
// runs on a clean machine.
func TestMigrate_FreshDir(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".makeslop")
	// Neither the dir nor settings.json exists — bare machine.

	applied, err := Migrate(base)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if !applied {
		t.Error("Migrate should return applied == true on fresh dir")
	}

	got, err := os.ReadFile(filepath.Join(base, "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	if !bytes.Equal(got, assets.Dockerfile) {
		t.Errorf("Dockerfile content mismatch: got %d bytes, want %d bytes", len(got), len(assets.Dockerfile))
	}

	s, err := Load(base)
	if err != nil {
		t.Fatalf("Load after Migrate: %v", err)
	}
	if s.Version != ConfigVersion {
		t.Errorf("Version = %d, want %d", s.Version, ConfigVersion)
	}
}

func TestMigrate_AlreadyUpToDate(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".makeslop")

	if _, err := Migrate(base); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(base, "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile after first Migrate: %v", err)
	}

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

// Migrate must force-overwrite a locally-edited Dockerfile when the version is behind.
func TestMigrate_OverwritesEditedDockerfile(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".makeslop")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	sentinel := []byte("# user-edited Dockerfile — must be overwritten by migrate\n")
	if err := os.WriteFile(filepath.Join(base, "Dockerfile"), sentinel, 0o644); err != nil {
		t.Fatalf("seed Dockerfile: %v", err)
	}
	s := &Settings{
		Image:      DefaultImage,
		Shell:      DefaultShell,
		Workspaces: map[string]Workspace{},
		Version:    0,
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

func TestMigrate_VersionBehindReRuns(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".makeslop")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	s := &Settings{
		Image:      DefaultImage,
		Shell:      DefaultShell,
		Workspaces: map[string]Workspace{},
		Version:    0,
	}
	if err := Save(base, s); err != nil {
		t.Fatalf("Save: %v", err)
	}

	applied, err := Migrate(base)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if !applied {
		t.Errorf("Migrate should return applied == true when Version=0 < ConfigVersion=%d", ConfigVersion)
	}

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
	if loaded.Version != ConfigVersion {
		t.Errorf("Version = %d, want %d", loaded.Version, ConfigVersion)
	}
}

// Downgrade guard: when Version is ahead of ConfigVersion (older binary),
// Migrate must be a no-op rather than re-run unknown migrations.
func TestMigrate_VersionAheadSkips(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".makeslop")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	s := &Settings{
		Image:      DefaultImage,
		Shell:      DefaultShell,
		Workspaces: map[string]Workspace{},
		Version:    999,
	}
	if err := Save(base, s); err != nil {
		t.Fatalf("Save: %v", err)
	}

	applied, err := Migrate(base)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if applied {
		t.Errorf("Migrate should return applied == false when Version=999 > ConfigVersion=%d (downgrade guard)", ConfigVersion)
	}

	loaded, err := Load(base)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Version != 999 {
		t.Errorf("Version = %d, want 999 (downgrade must not re-stamp)", loaded.Version)
	}
}

func TestWriteDockerfile(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".makeslop")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	junk := []byte("# STALE junk that should be overwritten\n")
	if err := os.WriteFile(filepath.Join(base, DockerfileFile), junk, 0o644); err != nil {
		t.Fatalf("seed Dockerfile: %v", err)
	}

	if err := WriteDockerfile(base); err != nil {
		t.Fatalf("WriteDockerfile: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(base, DockerfileFile))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	if !bytes.Equal(got, assets.Dockerfile) {
		t.Errorf("Dockerfile content mismatch: got %d bytes, want %d bytes", len(got), len(assets.Dockerfile))
	}
}

func TestWriteDockerfile_FreshDir(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".makeslop")
	// Do not pre-create the dir; WriteDockerfile must call MkdirAll.

	if err := WriteDockerfile(base); err != nil {
		t.Fatalf("WriteDockerfile on fresh dir: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(base, DockerfileFile))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	if !bytes.Equal(got, assets.Dockerfile) {
		t.Errorf("Dockerfile content mismatch: got %d bytes, want %d bytes", len(got), len(assets.Dockerfile))
	}
}

// WriteDockerfile must error when the base dir is read-only (CreateTemp fails).
func TestWriteDockerfile_ReadOnlyDir(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses permission checks")
	}
	base := filepath.Join(t.TempDir(), ".makeslop")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(base, 0o555); err != nil {
		t.Fatalf("chmod read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(base, 0o755) })

	// Some filesystems (e.g. fakeowner) ignore the chmod; skip if so.
	if f, err := os.CreateTemp(base, "probe-*"); err == nil {
		f.Close()
		os.Remove(f.Name())
		t.Skip("filesystem does not enforce directory permissions; skipping read-only test")
	}

	err := WriteDockerfile(base)
	if err == nil {
		t.Error("WriteDockerfile on read-only dir must return an error, got nil")
	}
}

// Migrate must succeed on a non-existent baseDir (writers MkdirAll internally).
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

// Migrate must not clobber user-set Image/Shell/Workspaces while stamping version.
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
		Image:      wantImage,
		Shell:      wantShell,
		Workspaces: wantWorkspaces,
		Version:    0,
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
	if got.Version != ConfigVersion {
		t.Errorf("Version = %d, want %d", got.Version, ConfigVersion)
	}
}

// Concrete upgrade path for existing installs: a dir stamped at version 1
// must migrate, overwrite its Dockerfile, re-stamp to ConfigVersion, and a
// following call must be a no-op.
func TestMigrate_UpgradeFromVersion1(t *testing.T) {
	const previousVersion = 1

	// Test is vacuous unless ConfigVersion > 1.
	if ConfigVersion <= previousVersion {
		t.Skipf("ConfigVersion=%d is not > 1; test is vacuous", ConfigVersion)
	}

	base := filepath.Join(t.TempDir(), ".makeslop")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	sentinel := []byte("# stale Dockerfile from version 1\n")
	if err := os.WriteFile(filepath.Join(base, "Dockerfile"), sentinel, 0o644); err != nil {
		t.Fatalf("seed Dockerfile: %v", err)
	}
	s := &Settings{
		Image:      DefaultImage,
		Shell:      DefaultShell,
		Workspaces: map[string]Workspace{},
		Version:    previousVersion,
	}
	if err := Save(base, s); err != nil {
		t.Fatalf("Save: %v", err)
	}

	applied, err := Migrate(base)
	if err != nil {
		t.Fatalf("Migrate (upgrade from 1): %v", err)
	}
	if !applied {
		t.Error("Migrate should return applied == true when upgrading from version 1")
	}

	got, err := os.ReadFile(filepath.Join(base, "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile after upgrade: %v", err)
	}
	if bytes.Equal(got, sentinel) {
		t.Error("Migrate did not overwrite the stale version-1 Dockerfile")
	}
	if !bytes.Equal(got, assets.Dockerfile) {
		t.Errorf("Dockerfile content mismatch after upgrade: got %d bytes, want %d bytes", len(got), len(assets.Dockerfile))
	}

	loaded, err := Load(base)
	if err != nil {
		t.Fatalf("Load after upgrade: %v", err)
	}
	if loaded.Version != ConfigVersion {
		t.Errorf("Version = %d, want %d", loaded.Version, ConfigVersion)
	}

	applied2, err := Migrate(base)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if applied2 {
		t.Error("second Migrate should return applied == false when already at ConfigVersion")
	}
}

func TestMigrationStatus_Fresh(t *testing.T) {
	s := &Settings{Version: 0}
	current, latest, stale := MigrationStatus(s)
	if current != 0 {
		t.Errorf("current = %d, want 0", current)
	}
	if latest != ConfigVersion {
		t.Errorf("latest = %d, want ConfigVersion (%d)", latest, ConfigVersion)
	}
	if !stale {
		t.Errorf("stale = false, want true when Version=0 < ConfigVersion=%d", ConfigVersion)
	}
}

func TestMigrationStatus_Equal(t *testing.T) {
	s := &Settings{Version: ConfigVersion}
	current, latest, stale := MigrationStatus(s)
	if current != ConfigVersion {
		t.Errorf("current = %d, want %d", current, ConfigVersion)
	}
	if latest != ConfigVersion {
		t.Errorf("latest = %d, want %d", latest, ConfigVersion)
	}
	if stale {
		t.Errorf("stale = true, want false when Version == ConfigVersion")
	}
}

func TestMigrationStatus_Behind(t *testing.T) {
	// Vacuous unless ConfigVersion > 1.
	if ConfigVersion <= 1 {
		t.Skipf("ConfigVersion=%d is not > 1; stale-behind-1 path is vacuous", ConfigVersion)
	}
	s := &Settings{Version: ConfigVersion - 1}
	current, latest, stale := MigrationStatus(s)
	if current != ConfigVersion-1 {
		t.Errorf("current = %d, want %d", current, ConfigVersion-1)
	}
	if latest != ConfigVersion {
		t.Errorf("latest = %d, want %d", latest, ConfigVersion)
	}
	if !stale {
		t.Errorf("stale = false, want true when Version=%d < ConfigVersion=%d", ConfigVersion-1, ConfigVersion)
	}
}

// Downgrade scenario: a version above ConfigVersion reports stale = false.
func TestMigrationStatus_Ahead(t *testing.T) {
	s := &Settings{Version: ConfigVersion + 10}
	_, _, stale := MigrationStatus(s)
	if stale {
		t.Errorf("stale = true, want false when Version=%d > ConfigVersion=%d (downgrade scenario)", ConfigVersion+10, ConfigVersion)
	}
}

// Byte-identical save invariant must still hold when Version is set.
func TestSaveLoadByteIdenticalForSameSettings_WithVersion(t *testing.T) {
	base := t.TempDir()

	s := &Settings{
		Version:    ConfigVersion,
		Image:      DefaultImage,
		Shell:      DefaultShell,
		TmpDirSize: DefaultTmpDirSize,
		Workspaces: map[string]Workspace{},
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
