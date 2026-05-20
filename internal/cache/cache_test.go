package cache

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// evalSymlinks returns dir with symlinks resolved. The Cache.Init and
// Cache.Lookup contract requires callers to pass an absolute, EvalSymlinks-
// evaluated path; on hosts where /tmp is itself a symlink (e.g. macOS where
// /tmp -> /private/tmp) the raw t.TempDir() result violates that precondition
// and would skew findAncestor matches. Use this helper anywhere a temp dir
// stands in for a real project pwd.
func evalSymlinks(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("eval symlinks %s: %v", dir, err)
	}
	return resolved
}

func TestLoadSettings_MissingReturnsEmptyDefaults(t *testing.T) {
	base := t.TempDir()
	c := New(base)

	s, err := c.loadSettings()
	if err != nil {
		t.Fatalf("loadSettings: unexpected error: %v", err)
	}
	if s.Version != currentVersion {
		t.Errorf("Version = %d, want %d", s.Version, currentVersion)
	}
	if s.Projects == nil {
		t.Error("Projects map is nil; want initialized empty map")
	}
	if len(s.Projects) != 0 {
		t.Errorf("Projects len = %d, want 0", len(s.Projects))
	}

	// Loading must not create the settings file.
	if _, err := os.Stat(filepath.Join(base, settingsFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("settings.json should not exist after load of missing file; stat err=%v", err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	base := t.TempDir()
	c := New(base)

	want := &Settings{
		Version: currentVersion,
		Projects: map[string]Project{
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

	if err := c.saveSettings(want); err != nil {
		t.Fatalf("saveSettings: %v", err)
	}

	got, err := c.loadSettings()
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}

	if got.Version != want.Version {
		t.Errorf("Version = %d, want %d", got.Version, want.Version)
	}
	if len(got.Projects) != len(want.Projects) {
		t.Fatalf("Projects len = %d, want %d", len(got.Projects), len(want.Projects))
	}
	for k, wantProj := range want.Projects {
		gotProj, ok := got.Projects[k]
		if !ok {
			t.Errorf("missing project %q after round-trip", k)
			continue
		}
		if gotProj.Name != wantProj.Name {
			t.Errorf("project %q Name = %q, want %q", k, gotProj.Name, wantProj.Name)
		}
		if !gotProj.CreatedAt.Equal(wantProj.CreatedAt) {
			t.Errorf("project %q CreatedAt = %v, want %v", k, gotProj.CreatedAt, wantProj.CreatedAt)
		}
	}
}

func TestSaveCreatesBaseDir(t *testing.T) {
	parent := t.TempDir()
	base := filepath.Join(parent, "nested", "deep", ".makeslop")
	c := New(base)

	s := &Settings{Version: currentVersion, Projects: map[string]Project{}}
	if err := c.saveSettings(s); err != nil {
		t.Fatalf("saveSettings: %v", err)
	}

	info, err := os.Stat(base)
	if err != nil {
		t.Fatalf("stat base dir: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("baseDir is not a directory")
	}
	if _, err := os.Stat(filepath.Join(base, settingsFile)); err != nil {
		t.Errorf("settings.json missing after save: %v", err)
	}
}

func TestLoadSettings_MalformedJSON(t *testing.T) {
	base := t.TempDir()
	c := New(base)

	if err := os.WriteFile(filepath.Join(base, settingsFile), []byte("not-json{"), 0o644); err != nil {
		t.Fatalf("seed bad settings file: %v", err)
	}

	_, err := c.loadSettings()
	if err == nil {
		t.Fatal("expected error from malformed JSON, got nil")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Errorf("error should not be ErrNotExist: %v", err)
	}
	if !strings.Contains(err.Error(), "settings") {
		t.Errorf("error should mention settings file context: %v", err)
	}
}

func TestSaveLoadByteIdenticalForSameSettings(t *testing.T) {
	base := t.TempDir()
	c := New(base)

	s := &Settings{
		Version: currentVersion,
		Projects: map[string]Project{
			"/x/y": {
				Name:      "y-aabbcc",
				CreatedAt: time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC),
			},
		},
	}

	if err := c.saveSettings(s); err != nil {
		t.Fatalf("first save: %v", err)
	}
	first, err := os.ReadFile(filepath.Join(base, settingsFile))
	if err != nil {
		t.Fatalf("read first: %v", err)
	}

	// Load and re-save the same logical content; bytes must match.
	loaded, err := c.loadSettings()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := c.saveSettings(loaded); err != nil {
		t.Fatalf("second save: %v", err)
	}
	second, err := os.ReadFile(filepath.Join(base, settingsFile))
	if err != nil {
		t.Fatalf("read second: %v", err)
	}

	if !bytes.Equal(first, second) {
		t.Errorf("settings.json bytes differ between equal saves\nfirst:\n%s\nsecond:\n%s", first, second)
	}

	// And the file should be valid JSON parsable back.
	var check Settings
	if err := json.Unmarshal(second, &check); err != nil {
		t.Errorf("saved file is not valid JSON: %v", err)
	}
}

func TestLoadSettings_NullProjectsBecomesEmptyMap(t *testing.T) {
	base := t.TempDir()
	c := New(base)

	if err := os.WriteFile(
		filepath.Join(base, settingsFile),
		[]byte(`{"version":1,"projects":null}`),
		0o644,
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	s, err := c.loadSettings()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if s.Projects == nil {
		t.Error("Projects must be non-nil even when JSON is null")
	}
}

func TestProjectName(t *testing.T) {
	expectedHash := func(p string) string {
		sum := sha256.Sum256([]byte(p))
		return hex.EncodeToString(sum[:])[:6]
	}

	cases := []struct {
		name     string
		path     string
		wantBase string
	}{
		{
			name:     "nested path",
			path:     "/workspace/makeslop",
			wantBase: "makeslop",
		},
		{
			name:     "single segment",
			path:     "/usr",
			wantBase: "usr",
		},
		{
			name:     "filesystem root uses 'root'",
			path:     string(filepath.Separator),
			wantBase: "root",
		},
		{
			name:     "path with spaces",
			path:     "/Users/jane doe/My Projects/my app",
			wantBase: "my app",
		},
		{
			name:     "unicode path",
			path:     "/home/użytkownik/проект-α",
			wantBase: "проект-α",
		},
		{
			name:     "deeply nested",
			path:     "/a/b/c/d/e/f/g",
			wantBase: "g",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := projectName(tc.path)
			wantHash := expectedHash(tc.path)
			want := tc.wantBase + "-" + wantHash
			if got != want {
				t.Errorf("projectName(%q) = %q, want %q", tc.path, got, want)
			}

			// Hash segment must be exactly 6 hex chars.
			parts := strings.Split(got, "-")
			suffix := parts[len(parts)-1]
			if len(suffix) != 6 {
				t.Errorf("hash suffix %q length = %d, want 6", suffix, len(suffix))
			}
			for _, r := range suffix {
				if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
					t.Errorf("hash suffix %q contains non-hex char %q", suffix, r)
					break
				}
			}
		})
	}
}

func TestProjectName_Deterministic(t *testing.T) {
	inputs := []string{
		"/workspace/makeslop",
		"/",
		"/Users/jane doe/проект",
	}
	for _, p := range inputs {
		first := projectName(p)
		for i := 0; i < 5; i++ {
			got := projectName(p)
			if got != first {
				t.Errorf("projectName(%q) not deterministic: got %q on iteration %d, first was %q", p, got, i, first)
			}
		}
	}
}

func TestProjectName_DifferentPathsDifferentHashes(t *testing.T) {
	// Same basename, different absolute paths must yield different names
	// (because the hash is over the full absolute path).
	a := projectName("/a/project")
	b := projectName("/b/project")
	if a == b {
		t.Errorf("expected different project names for different paths with same basename, both = %q", a)
	}
	// And the basename prefix should be identical.
	if !strings.HasPrefix(a, "project-") || !strings.HasPrefix(b, "project-") {
		t.Errorf("expected both names to start with 'project-', got %q and %q", a, b)
	}
}

// snapshotDir returns a map of relative path -> file content for every regular
// file under root. It is used to assert "no change happened" by comparing two
// snapshots byte-for-byte. Returns an empty map when root doesn't exist.
func snapshotDir(t *testing.T, root string) map[string][]byte {
	t.Helper()
	snap := map[string][]byte{}
	_, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
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

func snapshotsEqual(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok {
			return false
		}
		if !bytes.Equal(av, bv) {
			return false
		}
	}
	return true
}

func TestLookup_MissingSettingsReturnsErrNotRegistered(t *testing.T) {
	base := t.TempDir()
	c := New(base)

	before := snapshotDir(t, base)
	_, err := c.Lookup("/some/pwd")
	if !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("Lookup err = %v, want ErrNotRegistered", err)
	}
	after := snapshotDir(t, base)
	if !snapshotsEqual(before, after) {
		t.Errorf("Lookup mutated baseDir; before=%v after=%v", before, after)
	}
	if _, err := os.Stat(filepath.Join(base, settingsFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("settings.json must not exist after Lookup: %v", err)
	}
}

func TestLookup_NoMatchingAncestor(t *testing.T) {
	base := t.TempDir()
	c := New(base)

	seed := &Settings{
		Version: currentVersion,
		Projects: map[string]Project{
			"/some/other/project": {Name: "project-abcdef", CreatedAt: time.Now().UTC()},
		},
	}
	if err := c.saveSettings(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	before, err := os.ReadFile(filepath.Join(base, settingsFile))
	if err != nil {
		t.Fatalf("read before: %v", err)
	}

	_, err = c.Lookup("/totally/different/pwd")
	if !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("Lookup err = %v, want ErrNotRegistered", err)
	}

	after, err := os.ReadFile(filepath.Join(base, settingsFile))
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("settings.json was modified by Lookup; before=%s after=%s", before, after)
	}
}

func TestLookup_ExactPwdMatch(t *testing.T) {
	base := t.TempDir()
	c := New(base)

	pwd := "/workspace/makeslop"
	seed := &Settings{
		Version: currentVersion,
		Projects: map[string]Project{
			pwd: {Name: "makeslop-abcdef", CreatedAt: time.Now().UTC()},
		},
	}
	if err := c.saveSettings(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := c.Lookup(pwd)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	want := filepath.Join(base, projectsDir, "makeslop-abcdef")
	if got != want {
		t.Errorf("Lookup = %q, want %q", got, want)
	}
}

func TestLookup_ParentRegistered(t *testing.T) {
	base := t.TempDir()
	c := New(base)

	parent := "/workspace/makeslop"
	seed := &Settings{
		Version: currentVersion,
		Projects: map[string]Project{
			parent: {Name: "makeslop-abcdef", CreatedAt: time.Now().UTC()},
		},
	}
	if err := c.saveSettings(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	before, err := os.ReadFile(filepath.Join(base, settingsFile))
	if err != nil {
		t.Fatalf("read before: %v", err)
	}

	got, err := c.Lookup(filepath.Join(parent, "internal", "cache"))
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	want := filepath.Join(base, projectsDir, "makeslop-abcdef")
	if got != want {
		t.Errorf("Lookup = %q, want %q", got, want)
	}

	after, err := os.ReadFile(filepath.Join(base, settingsFile))
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("settings.json was modified by Lookup; before=%s after=%s", before, after)
	}
}

func TestLookup_CorruptSettingsReturnsWrappedError(t *testing.T) {
	base := t.TempDir()
	c := New(base)

	if err := os.WriteFile(filepath.Join(base, settingsFile), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seed bad settings: %v", err)
	}

	_, err := c.Lookup("/any/pwd")
	if err == nil {
		t.Fatal("expected error from corrupt settings, got nil")
	}
	if errors.Is(err, ErrNotRegistered) {
		t.Errorf("error must NOT be ErrNotRegistered: %v", err)
	}
	if !strings.Contains(err.Error(), "settings") {
		t.Errorf("error should mention settings context: %v", err)
	}
}

func TestInit_FreshCreatesEverything(t *testing.T) {
	base := t.TempDir()
	c := New(base)

	pwd := filepath.Join(evalSymlinks(t, t.TempDir()), "myproject")
	if err := os.MkdirAll(pwd, 0o755); err != nil {
		t.Fatalf("mkdir pwd: %v", err)
	}

	got, err := c.Init(pwd)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	wantName := projectName(pwd)
	wantDir := filepath.Join(base, projectsDir, wantName)
	if got != wantDir {
		t.Errorf("Init = %q, want %q", got, wantDir)
	}

	info, err := os.Stat(wantDir)
	if err != nil {
		t.Fatalf("stat project dir: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("project path is not a directory")
	}

	s, err := c.loadSettings()
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	proj, ok := s.Projects[pwd]
	if !ok {
		t.Fatalf("settings does not contain entry for %q; have %v", pwd, s.Projects)
	}
	if proj.Name != wantName {
		t.Errorf("project name = %q, want %q", proj.Name, wantName)
	}
	if proj.CreatedAt.IsZero() {
		t.Errorf("project CreatedAt is zero")
	}
	if proj.CreatedAt.Location() != time.UTC {
		t.Errorf("project CreatedAt location = %v, want UTC", proj.CreatedAt.Location())
	}
}

func TestInit_FromSubdirOfRegisteredProjectIsNoOp(t *testing.T) {
	base := t.TempDir()
	c := New(base)

	parent := filepath.Join(evalSymlinks(t, t.TempDir()), "registered")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}

	parentDir, err := c.Init(parent)
	if err != nil {
		t.Fatalf("Init parent: %v", err)
	}

	before, err := os.ReadFile(filepath.Join(base, settingsFile))
	if err != nil {
		t.Fatalf("read before: %v", err)
	}

	subdir := filepath.Join(parent, "deeply", "nested", "subdir")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}

	got, err := c.Init(subdir)
	if err != nil {
		t.Fatalf("Init subdir: %v", err)
	}
	if got != parentDir {
		t.Errorf("Init from subdir = %q, want parent dir %q", got, parentDir)
	}

	after, err := os.ReadFile(filepath.Join(base, settingsFile))
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("settings.json must be byte-equal after no-op subdir init\nbefore=%s\nafter=%s", before, after)
	}
}

func TestInit_FromParentOfRegisteredRegistersNew(t *testing.T) {
	base := t.TempDir()
	c := New(base)

	parent := filepath.Join(evalSymlinks(t, t.TempDir()), "parent")
	child := filepath.Join(parent, "child")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	childDir, err := c.Init(child)
	if err != nil {
		t.Fatalf("Init child: %v", err)
	}

	parentDir, err := c.Init(parent)
	if err != nil {
		t.Fatalf("Init parent: %v", err)
	}
	if parentDir == childDir {
		t.Errorf("parent and child should have different cache dirs; both = %q", parentDir)
	}

	s, err := c.loadSettings()
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	if _, ok := s.Projects[child]; !ok {
		t.Errorf("child %q missing from settings", child)
	}
	if _, ok := s.Projects[parent]; !ok {
		t.Errorf("parent %q missing from settings", parent)
	}
	if len(s.Projects) != 2 {
		t.Errorf("expected 2 projects registered, got %d: %v", len(s.Projects), s.Projects)
	}
}

func TestInit_FromSiblingRegistersNew(t *testing.T) {
	base := t.TempDir()
	c := New(base)

	root := evalSymlinks(t, t.TempDir())
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	if err := os.MkdirAll(a, 0o755); err != nil {
		t.Fatalf("mkdir a: %v", err)
	}
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatalf("mkdir b: %v", err)
	}

	aDir, err := c.Init(a)
	if err != nil {
		t.Fatalf("Init a: %v", err)
	}
	bDir, err := c.Init(b)
	if err != nil {
		t.Fatalf("Init b: %v", err)
	}
	if aDir == bDir {
		t.Errorf("siblings should have distinct cache dirs; both = %q", aDir)
	}

	s, err := c.loadSettings()
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	if len(s.Projects) != 2 {
		t.Errorf("expected 2 projects, got %d", len(s.Projects))
	}
}

func TestInit_SecondCallByteEqualNoOp(t *testing.T) {
	base := t.TempDir()
	c := New(base)

	pwd := filepath.Join(evalSymlinks(t, t.TempDir()), "proj")
	if err := os.MkdirAll(pwd, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	first, err := c.Init(pwd)
	if err != nil {
		t.Fatalf("Init first: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(base, settingsFile))
	if err != nil {
		t.Fatalf("read before: %v", err)
	}

	second, err := c.Init(pwd)
	if err != nil {
		t.Fatalf("Init second: %v", err)
	}
	if first != second {
		t.Errorf("Init returned different paths on repeat: first=%q second=%q", first, second)
	}

	after, err := os.ReadFile(filepath.Join(base, settingsFile))
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("settings.json must be byte-equal across idempotent Init\nbefore=%s\nafter=%s", before, after)
	}
}

func TestInit_CorruptSettingsReturnsWrappedError(t *testing.T) {
	base := t.TempDir()
	c := New(base)

	if err := os.WriteFile(filepath.Join(base, settingsFile), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seed bad settings: %v", err)
	}

	_, err := c.Init("/any/pwd")
	if err == nil {
		t.Fatal("expected error from corrupt settings, got nil")
	}
	if errors.Is(err, ErrNotRegistered) {
		t.Errorf("error must NOT be ErrNotRegistered: %v", err)
	}
	if !strings.Contains(err.Error(), "settings") {
		t.Errorf("error should mention settings context: %v", err)
	}
}

func TestFindAncestor_StopsAtRoot(t *testing.T) {
	base := t.TempDir()
	c := New(base)
	s := &Settings{Version: currentVersion, Projects: map[string]Project{}}

	// Walk from a deep path with empty settings: must terminate (not infinite loop)
	// and return ok=false.
	_, _, ok := c.findAncestor(s, "/a/b/c/d/e/f")
	if ok {
		t.Errorf("findAncestor returned ok=true on empty settings")
	}
}

func TestFindAncestor_RootRegistered(t *testing.T) {
	base := t.TempDir()
	c := New(base)
	rootKey := string(filepath.Separator)
	s := &Settings{
		Version: currentVersion,
		Projects: map[string]Project{
			rootKey: {Name: "root-aabbcc", CreatedAt: time.Now().UTC()},
		},
	}
	matched, proj, ok := c.findAncestor(s, "/some/deep/path")
	if !ok {
		t.Fatal("expected ancestor match when root is registered")
	}
	if matched != rootKey {
		t.Errorf("matched = %q, want %q", matched, rootKey)
	}
	if proj.Name != "root-aabbcc" {
		t.Errorf("proj.Name = %q, want root-aabbcc", proj.Name)
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
