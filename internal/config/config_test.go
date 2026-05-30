package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Zwergpro/makeslop/internal/assets"
)

func TestLoad_MissingReturnsEmptyDefaults(t *testing.T) {
	base := t.TempDir()

	s, err := Load(base)
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if s.Version != CurrentVersion {
		t.Errorf("Version = %d, want %d", s.Version, CurrentVersion)
	}
	if s.Workspaces == nil {
		t.Error("Workspaces map is nil; want initialized empty map")
	}
	if len(s.Workspaces) != 0 {
		t.Errorf("Workspaces len = %d, want 0", len(s.Workspaces))
	}
	if s.Image != DefaultImage {
		t.Errorf("Image = %q, want %q", s.Image, DefaultImage)
	}
	if s.Shell != DefaultShell {
		t.Errorf("Shell = %q, want %q", s.Shell, DefaultShell)
	}

	if _, err := os.Stat(filepath.Join(base, SettingsFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("settings.json should not exist after load of missing file; stat err=%v", err)
	}
}

func TestLoad_PreservesExplicitImageAndShell(t *testing.T) {
	base := t.TempDir()
	body := `{"version":1,"image":"custom-img","shell":"/bin/bash","workspaces":{}}`
	if err := os.WriteFile(filepath.Join(base, SettingsFile), []byte(body), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	s, err := Load(base)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Image != "custom-img" {
		t.Errorf("Image = %q, want %q", s.Image, "custom-img")
	}
	if s.Shell != "/bin/bash" {
		t.Errorf("Shell = %q, want %q", s.Shell, "/bin/bash")
	}
}

// Guards that settings.json files written before Image/Shell existed keep working.
func TestLoad_LegacyConfigGetsDefaultsForMissingFields(t *testing.T) {
	base := t.TempDir()
	body := `{"version":1,"workspaces":{}}`
	if err := os.WriteFile(filepath.Join(base, SettingsFile), []byte(body), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	s, err := Load(base)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Image != DefaultImage {
		t.Errorf("Image = %q, want %q", s.Image, DefaultImage)
	}
	if s.Shell != DefaultShell {
		t.Errorf("Shell = %q, want %q", s.Shell, DefaultShell)
	}
}

// Explicitly empty strings on disk must be treated like absent fields so a
// future code path that writes "" can never starve callers of a usable value.
func TestLoad_ExplicitEmptyImageAndShellGetsDefaults(t *testing.T) {
	base := t.TempDir()
	body := `{"version":1,"image":"","shell":"","workspaces":{}}`
	if err := os.WriteFile(filepath.Join(base, SettingsFile), []byte(body), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	s, err := Load(base)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Image != DefaultImage {
		t.Errorf("Image = %q, want %q", s.Image, DefaultImage)
	}
	if s.Shell != DefaultShell {
		t.Errorf("Shell = %q, want %q", s.Shell, DefaultShell)
	}
}

func TestSaveLoadRoundTrip_PreservesNonDefaultImageAndShell(t *testing.T) {
	base := t.TempDir()
	want := &Settings{
		Version:    CurrentVersion,
		Image:      "myimg:tag",
		Shell:      "/bin/fish",
		Workspaces: map[string]Workspace{},
	}
	if err := Save(base, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(base)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Image != want.Image {
		t.Errorf("Image = %q, want %q", got.Image, want.Image)
	}
	if got.Shell != want.Shell {
		t.Errorf("Shell = %q, want %q", got.Shell, want.Shell)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	base := t.TempDir()

	want := &Settings{
		Version: CurrentVersion,
		Workspaces: map[string]Workspace{
			"/workspace/makeslop": {
				Name:      "makeslop-abcdef",
				CreatedAt: time.Date(2026, 5, 20, 16, 45, 0, 0, time.UTC),
			},
			"/tmp/other": {
				Name:      "other-123456",
				CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			},
		},
	}

	if err := Save(base, want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(base)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got.Version != want.Version {
		t.Errorf("Version = %d, want %d", got.Version, want.Version)
	}
	if len(got.Workspaces) != len(want.Workspaces) {
		t.Fatalf("Workspaces len = %d, want %d", len(got.Workspaces), len(want.Workspaces))
	}
	for k, wantWs := range want.Workspaces {
		gotWs, ok := got.Workspaces[k]
		if !ok {
			t.Errorf("missing workspace %q after round-trip", k)
			continue
		}
		if gotWs.Name != wantWs.Name {
			t.Errorf("workspace %q Name = %q, want %q", k, gotWs.Name, wantWs.Name)
		}
		if !gotWs.CreatedAt.Equal(wantWs.CreatedAt) {
			t.Errorf("workspace %q CreatedAt = %v, want %v", k, gotWs.CreatedAt, wantWs.CreatedAt)
		}
	}
}

func TestSaveCreatesBaseDir(t *testing.T) {
	parent := t.TempDir()
	base := filepath.Join(parent, "nested", "deep", ".makeslop")

	s := &Settings{Version: CurrentVersion, Workspaces: map[string]Workspace{}}
	if err := Save(base, s); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(base)
	if err != nil {
		t.Fatalf("stat base dir: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("baseDir is not a directory")
	}
	if _, err := os.Stat(filepath.Join(base, SettingsFile)); err != nil {
		t.Errorf("settings.json missing after save: %v", err)
	}
}

func TestLoad_MalformedJSON(t *testing.T) {
	base := t.TempDir()

	if err := os.WriteFile(filepath.Join(base, SettingsFile), []byte("not-json{"), 0o644); err != nil {
		t.Fatalf("seed bad settings file: %v", err)
	}

	_, err := Load(base)
	if err == nil {
		t.Fatal("expected error from malformed JSON, got nil")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error should not be ErrNotExist: %v", err)
	}
	if !strings.Contains(err.Error(), "settings") {
		t.Errorf("error should mention settings file context: %v", err)
	}
}

func TestSaveLoadByteIdenticalForSameSettings(t *testing.T) {
	base := t.TempDir()

	s := &Settings{
		Version: CurrentVersion,
		// Image/Shell included so the post-Load defaulting does not alter the
		// re-serialized byte sequence on the second save.
		Image: DefaultImage,
		Shell: DefaultShell,
		Workspaces: map[string]Workspace{
			"/x/y": {
				Name:      "y-aabbcc",
				CreatedAt: time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC),
			},
		},
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

func TestLoad_NullWorkspacesBecomesEmptyMap(t *testing.T) {
	base := t.TempDir()

	if err := os.WriteFile(
		filepath.Join(base, SettingsFile),
		[]byte(`{"version":1,"workspaces":null}`),
		0o644,
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	s, err := Load(base)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if s.Workspaces == nil {
		t.Error("Workspaces must be non-nil even when JSON is null")
	}
}

func TestDefaultBaseDir_HonorsHOME(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	got, err := DefaultBaseDir()
	if err != nil {
		t.Fatalf("DefaultBaseDir: %v", err)
	}
	want := filepath.Join(fakeHome, ".makeslop")
	if got != want {
		t.Errorf("DefaultBaseDir = %q, want %q", got, want)
	}
}

// TestLoad_MissingFile_MigratedVersionIsZero guards that Load of a missing
// settings.json yields MigratedVersion == 0 (not defaulted to MigrationVersion).
func TestLoad_MissingFile_MigratedVersionIsZero(t *testing.T) {
	base := t.TempDir()

	s, err := Load(base)
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if s.MigratedVersion != 0 {
		t.Errorf("MigratedVersion = %d, want 0 for missing file", s.MigratedVersion)
	}
}

// TestLoad_LegacyConfig_MigratedVersionIsZero guards backward compat: a
// settings.json written without migrated_version round-trips with MigratedVersion == 0.
func TestLoad_LegacyConfig_MigratedVersionIsZero(t *testing.T) {
	base := t.TempDir()
	body := `{"version":1,"image":"claudebox","shell":"/bin/zsh","workspaces":{}}`
	if err := os.WriteFile(filepath.Join(base, SettingsFile), []byte(body), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	s, err := Load(base)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.MigratedVersion != 0 {
		t.Errorf("MigratedVersion = %d, want 0 for legacy file without migrated_version", s.MigratedVersion)
	}
}

// TestSaveLoad_MigratedVersionRoundTrips guards that a set MigratedVersion
// survives a Save → Load cycle intact.
func TestSaveLoad_MigratedVersionRoundTrips(t *testing.T) {
	base := t.TempDir()
	want := &Settings{
		Version:         CurrentVersion,
		Image:           DefaultImage,
		Shell:           DefaultShell,
		Workspaces:      map[string]Workspace{},
		MigratedVersion: MigrationVersion,
	}
	if err := Save(base, want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(base)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.MigratedVersion != want.MigratedVersion {
		t.Errorf("MigratedVersion = %d, want %d", got.MigratedVersion, want.MigratedVersion)
	}
}

func TestBootstrap_CreatesDirsAndClaudeJSON(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".makeslop")

	if err := Bootstrap(base); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	for _, sub := range []string{"", ".codex", ".claude", WorkspacesDir} {
		path := filepath.Join(base, sub)
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("stat %s: %v", path, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", path)
		}
	}

	claudeJSON := filepath.Join(base, ".claude.json")
	data, err := os.ReadFile(claudeJSON)
	if err != nil {
		t.Fatalf("read .claude.json: %v", err)
	}
	if !bytes.Equal(data, []byte("{}\n")) {
		t.Errorf(".claude.json content = %q, want %q", data, "{}\n")
	}

	// settings.json must NOT be touched by Bootstrap — the workspace registry
	// owns that file.
	if _, err := os.Stat(filepath.Join(base, SettingsFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Bootstrap must not create settings.json; stat err=%v", err)
	}
}

func TestBootstrap_Idempotent(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".makeslop")

	if err := Bootstrap(base); err != nil {
		t.Fatalf("first Bootstrap: %v", err)
	}
	before := snapshotTree(t, base)

	if err := Bootstrap(base); err != nil {
		t.Fatalf("second Bootstrap: %v", err)
	}
	after := snapshotTree(t, base)

	assertSnapshotsEqual(t, before, after)
}

func TestBootstrap_DoesNotOverwriteExistingClaudeJSON(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".makeslop")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	existing := []byte(`{"user":"edited"}`)
	if err := os.WriteFile(filepath.Join(base, ".claude.json"), existing, 0o644); err != nil {
		t.Fatalf("seed .claude.json: %v", err)
	}

	if err := Bootstrap(base); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(base, ".claude.json"))
	if err != nil {
		t.Fatalf("read .claude.json: %v", err)
	}
	if !bytes.Equal(got, existing) {
		t.Errorf(".claude.json was modified by Bootstrap\nbefore: %s\nafter:  %s", existing, got)
	}
}

func TestBootstrap_PartialStateRecovers(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".makeslop")
	codex := filepath.Join(base, ".codex")
	if err := os.MkdirAll(codex, 0o755); err != nil {
		t.Fatalf("seed .codex: %v", err)
	}
	marker := filepath.Join(codex, "preexisting")
	if err := os.WriteFile(marker, []byte("hello"), 0o644); err != nil {
		t.Fatalf("seed marker file: %v", err)
	}

	if err := Bootstrap(base); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	for _, sub := range []string{".codex", ".claude", WorkspacesDir} {
		info, err := os.Stat(filepath.Join(base, sub))
		if err != nil {
			t.Errorf("missing %s after bootstrap: %v", sub, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", sub)
		}
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("marker file disappeared: %v", err)
	}
	if !bytes.Equal(got, []byte("hello")) {
		t.Errorf("marker content = %q, want %q", got, "hello")
	}
}

// TestBootstrap_CreatesDockerfile verifies that Bootstrap seeds Dockerfile with
// content equal to the embedded assets.Dockerfile on a fresh directory.
func TestBootstrap_CreatesDockerfile(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".makeslop")

	if err := Bootstrap(base); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	path := filepath.Join(base, "Dockerfile")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	if !bytes.Equal(got, assets.Dockerfile) {
		t.Errorf("Dockerfile content mismatch: got %d bytes, want %d bytes", len(got), len(assets.Dockerfile))
	}
}

// TestBootstrap_DoesNotOverwriteExistingDockerfile verifies that Bootstrap uses
// the O_EXCL pattern and does not clobber a pre-existing Dockerfile.
func TestBootstrap_DoesNotOverwriteExistingDockerfile(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".makeslop")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	sentinel := []byte("# sentinel — must not be overwritten\n")
	if err := os.WriteFile(filepath.Join(base, "Dockerfile"), sentinel, 0o644); err != nil {
		t.Fatalf("seed Dockerfile: %v", err)
	}

	if err := Bootstrap(base); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(base, "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	if !bytes.Equal(got, sentinel) {
		t.Errorf("Dockerfile was overwritten by Bootstrap\nbefore: %s\nafter:  %s", sentinel, got)
	}
}

// TestBootstrap_DoesNotWriteSettingsJSON verifies that Bootstrap does not create
// or touch settings.json, ensuring migrated_version is never stamped on init.
func TestBootstrap_DoesNotWriteSettingsJSON(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".makeslop")

	if err := Bootstrap(base); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	if _, err := os.Stat(filepath.Join(base, SettingsFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Bootstrap must not create settings.json; stat err=%v", err)
	}
}

func snapshotTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	snap := map[string][]byte{}
	_, err := os.Stat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return snap
	}
	if err != nil {
		t.Fatalf("stat %s: %v", root, err)
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		snap[rel] = data
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return snap
}

func mapKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func assertSnapshotsEqual(t *testing.T, before, after map[string][]byte) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("snapshot file count differs: before=%d after=%d (before keys=%v after keys=%v)",
			len(before), len(after), mapKeys(before), mapKeys(after))
	}
	for k, vBefore := range before {
		vAfter, ok := after[k]
		if !ok {
			t.Errorf("file %s present before, missing after", k)
			continue
		}
		if !bytes.Equal(vBefore, vAfter) {
			t.Errorf("file %s changed:\nbefore:\n%s\nafter:\n%s", k, vBefore, vAfter)
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			t.Errorf("file %s appeared after but not before", k)
		}
	}
}
