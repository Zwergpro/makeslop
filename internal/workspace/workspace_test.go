package workspace

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Zwergpro/makeslop/internal/config"
)

func evalSymlinks(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("eval symlinks %s: %v", dir, err)
	}
	return resolved
}

func snapshotDir(t *testing.T, root string) map[string][]byte {
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

func TestWorkspaceName(t *testing.T) {
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
			got := workspaceName(tc.path)
			wantHash := expectedHash(tc.path)
			want := tc.wantBase + "-" + wantHash
			if got != want {
				t.Errorf("workspaceName(%q) = %q, want %q", tc.path, got, want)
			}

			parts := strings.Split(got, "-")
			suffix := parts[len(parts)-1]
			if len(suffix) != 6 {
				t.Errorf("hash suffix %q length = %d, want 6", suffix, len(suffix))
			}
			for _, r := range suffix {
				if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
					t.Errorf("hash suffix %q contains non-hex char %q", suffix, r)
					break
				}
			}
		})
	}
}

func TestWorkspaceName_Deterministic(t *testing.T) {
	inputs := []string{
		"/workspace/makeslop",
		"/",
		"/Users/jane doe/проект",
	}
	for _, p := range inputs {
		first := workspaceName(p)
		for i := 0; i < 5; i++ {
			got := workspaceName(p)
			if got != first {
				t.Errorf("workspaceName(%q) not deterministic: got %q on iteration %d, first was %q", p, got, i, first)
			}
		}
	}
}

func TestWorkspaceName_DifferentPathsDifferentHashes(t *testing.T) {
	a := workspaceName("/a/project")
	b := workspaceName("/b/project")
	if a == b {
		t.Errorf("expected different workspace names for different paths with same basename, both = %q", a)
	}
	if !strings.HasPrefix(a, "project-") || !strings.HasPrefix(b, "project-") {
		t.Errorf("expected both names to start with 'project-', got %q and %q", a, b)
	}
}

func TestLookup_MissingSettingsReturnsErrNotRegistered(t *testing.T) {
	base := t.TempDir()
	w := New(base)

	before := snapshotDir(t, base)
	_, _, err := w.Lookup("/some/pwd")
	if !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("Lookup err = %v, want ErrNotRegistered", err)
	}
	after := snapshotDir(t, base)
	if !snapshotsEqual(before, after) {
		t.Errorf("Lookup mutated baseDir; before=%v after=%v", before, after)
	}
	if _, err := os.Stat(filepath.Join(base, config.SettingsFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("settings.json must not exist after Lookup: %v", err)
	}
}

func TestLookup_NoMatchingAncestor(t *testing.T) {
	base := t.TempDir()
	w := New(base)

	seed := &config.Settings{
		Version: config.CurrentVersion,
		Workspaces: map[string]config.Workspace{
			"/some/other/project": {Name: "project-abcdef", CreatedAt: time.Now().UTC()},
		},
	}
	if err := config.Save(base, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	before, err := os.ReadFile(filepath.Join(base, config.SettingsFile))
	if err != nil {
		t.Fatalf("read before: %v", err)
	}

	_, _, err = w.Lookup("/totally/different/pwd")
	if !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("Lookup err = %v, want ErrNotRegistered", err)
	}

	after, err := os.ReadFile(filepath.Join(base, config.SettingsFile))
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("settings.json was modified by Lookup; before=%s after=%s", before, after)
	}
}

func TestLookup_ExactPwdMatch(t *testing.T) {
	base := t.TempDir()
	w := New(base)

	pwd := "/workspace/makeslop"
	seed := &config.Settings{
		Version: config.CurrentVersion,
		Workspaces: map[string]config.Workspace{
			pwd: {Name: "makeslop-abcdef", CreatedAt: time.Now().UTC()},
		},
	}
	if err := config.Save(base, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	matched, got, err := w.Lookup(pwd)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if matched != pwd {
		t.Errorf("matched = %q, want %q", matched, pwd)
	}
	want := filepath.Join(base, config.WorkspacesDir, "makeslop-abcdef")
	if got != want {
		t.Errorf("Lookup = %q, want %q", got, want)
	}
}

func TestLookup_ParentRegistered(t *testing.T) {
	base := t.TempDir()
	w := New(base)

	parent := "/workspace/makeslop"
	seed := &config.Settings{
		Version: config.CurrentVersion,
		Workspaces: map[string]config.Workspace{
			parent: {Name: "makeslop-abcdef", CreatedAt: time.Now().UTC()},
		},
	}
	if err := config.Save(base, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	before, err := os.ReadFile(filepath.Join(base, config.SettingsFile))
	if err != nil {
		t.Fatalf("read before: %v", err)
	}

	matched, got, err := w.Lookup(filepath.Join(parent, "internal", "workspace"))
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if matched != parent {
		t.Errorf("matched = %q, want %q (ancestor, not subdir)", matched, parent)
	}
	want := filepath.Join(base, config.WorkspacesDir, "makeslop-abcdef")
	if got != want {
		t.Errorf("Lookup = %q, want %q", got, want)
	}

	after, err := os.ReadFile(filepath.Join(base, config.SettingsFile))
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("settings.json was modified by Lookup; before=%s after=%s", before, after)
	}
}

func TestLookup_CorruptSettingsReturnsWrappedError(t *testing.T) {
	base := t.TempDir()
	w := New(base)

	if err := os.WriteFile(filepath.Join(base, config.SettingsFile), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seed bad settings: %v", err)
	}

	_, _, err := w.Lookup("/any/pwd")
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
	w := New(base)

	pwd := filepath.Join(evalSymlinks(t, t.TempDir()), "myproject")
	if err := os.MkdirAll(pwd, 0o755); err != nil {
		t.Fatalf("mkdir pwd: %v", err)
	}

	got, err := w.Init(pwd)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	wantName := workspaceName(pwd)
	wantDir := filepath.Join(base, config.WorkspacesDir, wantName)
	if got != wantDir {
		t.Errorf("Init = %q, want %q", got, wantDir)
	}

	info, err := os.Stat(wantDir)
	if err != nil {
		t.Fatalf("stat workspace dir: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("workspace path is not a directory")
	}

	for _, d := range []string{".claude", ".codex", "docs"} {
		p := filepath.Join(wantDir, d)
		di, err := os.Stat(p)
		if err != nil {
			t.Errorf("stat %s: %v", p, err)
			continue
		}
		if !di.IsDir() {
			t.Errorf("%s is not a directory", p)
		}
	}
	claudeMd := filepath.Join(wantDir, "CLAUDE.md")
	fi, err := os.Stat(claudeMd)
	if err != nil {
		t.Errorf("stat %s: %v", claudeMd, err)
	} else {
		if !fi.Mode().IsRegular() {
			t.Errorf("%s is not a regular file", claudeMd)
		}
		if fi.Size() != 0 {
			t.Errorf("%s size = %d, want 0", claudeMd, fi.Size())
		}
	}

	s, err := config.Load(base)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	ws, ok := s.Workspaces[pwd]
	if !ok {
		t.Fatalf("settings does not contain entry for %q; have %v", pwd, s.Workspaces)
	}
	if ws.Name != wantName {
		t.Errorf("workspace name = %q, want %q", ws.Name, wantName)
	}
	if ws.CreatedAt.IsZero() {
		t.Errorf("workspace CreatedAt is zero")
	}
	if ws.CreatedAt.Location() != time.UTC {
		t.Errorf("workspace CreatedAt location = %v, want UTC", ws.CreatedAt.Location())
	}
}

func TestInit_FromSubdirOfRegisteredWorkspaceIsNoOp(t *testing.T) {
	base := t.TempDir()
	w := New(base)

	parent := filepath.Join(evalSymlinks(t, t.TempDir()), "registered")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}

	parentDir, err := w.Init(parent)
	if err != nil {
		t.Fatalf("Init parent: %v", err)
	}

	before, err := os.ReadFile(filepath.Join(base, config.SettingsFile))
	if err != nil {
		t.Fatalf("read before: %v", err)
	}

	subdir := filepath.Join(parent, "deeply", "nested", "subdir")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}

	got, err := w.Init(subdir)
	if err != nil {
		t.Fatalf("Init subdir: %v", err)
	}
	if got != parentDir {
		t.Errorf("Init from subdir = %q, want parent dir %q", got, parentDir)
	}

	after, err := os.ReadFile(filepath.Join(base, config.SettingsFile))
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("settings.json must be byte-equal after no-op subdir init\nbefore=%s\nafter=%s", before, after)
	}
}

func TestInit_FromParentOfRegisteredRegistersNew(t *testing.T) {
	base := t.TempDir()
	w := New(base)

	parent := filepath.Join(evalSymlinks(t, t.TempDir()), "parent")
	child := filepath.Join(parent, "child")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	childDir, err := w.Init(child)
	if err != nil {
		t.Fatalf("Init child: %v", err)
	}

	parentDir, err := w.Init(parent)
	if err != nil {
		t.Fatalf("Init parent: %v", err)
	}
	if parentDir == childDir {
		t.Errorf("parent and child should have different cache dirs; both = %q", parentDir)
	}

	s, err := config.Load(base)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if _, ok := s.Workspaces[child]; !ok {
		t.Errorf("child %q missing from settings", child)
	}
	if _, ok := s.Workspaces[parent]; !ok {
		t.Errorf("parent %q missing from settings", parent)
	}
	if len(s.Workspaces) != 2 {
		t.Errorf("expected 2 workspaces registered, got %d: %v", len(s.Workspaces), s.Workspaces)
	}
}

func TestInit_FromSiblingRegistersNew(t *testing.T) {
	base := t.TempDir()
	w := New(base)

	root := evalSymlinks(t, t.TempDir())
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	if err := os.MkdirAll(a, 0o755); err != nil {
		t.Fatalf("mkdir a: %v", err)
	}
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatalf("mkdir b: %v", err)
	}

	aDir, err := w.Init(a)
	if err != nil {
		t.Fatalf("Init a: %v", err)
	}
	bDir, err := w.Init(b)
	if err != nil {
		t.Fatalf("Init b: %v", err)
	}
	if aDir == bDir {
		t.Errorf("siblings should have distinct cache dirs; both = %q", aDir)
	}

	s, err := config.Load(base)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if len(s.Workspaces) != 2 {
		t.Errorf("expected 2 workspaces, got %d", len(s.Workspaces))
	}
}

func TestInit_SecondCallByteEqualNoOp(t *testing.T) {
	base := t.TempDir()
	w := New(base)

	pwd := filepath.Join(evalSymlinks(t, t.TempDir()), "proj")
	if err := os.MkdirAll(pwd, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	first, err := w.Init(pwd)
	if err != nil {
		t.Fatalf("Init first: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(base, config.SettingsFile))
	if err != nil {
		t.Fatalf("read before: %v", err)
	}

	// User edits the scaffolded template; the second Init must not clobber.
	sentinel := []byte("user content\n")
	if err := os.WriteFile(filepath.Join(first, "CLAUDE.md"), sentinel, 0o644); err != nil {
		t.Fatalf("write sentinel CLAUDE.md: %v", err)
	}
	marker := []byte("marker\n")
	if err := os.WriteFile(filepath.Join(first, ".claude", "marker.txt"), marker, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	cacheBefore := snapshotDir(t, first)

	second, err := w.Init(pwd)
	if err != nil {
		t.Fatalf("Init second: %v", err)
	}
	if first != second {
		t.Errorf("Init returned different paths on repeat: first=%q second=%q", first, second)
	}

	after, err := os.ReadFile(filepath.Join(base, config.SettingsFile))
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("settings.json must be byte-equal across idempotent Init\nbefore=%s\nafter=%s", before, after)
	}

	cacheAfter := snapshotDir(t, first)
	if !snapshotsEqual(cacheBefore, cacheAfter) {
		t.Errorf("cache dir must be byte-equal across idempotent Init\nbefore=%v\nafter=%v", cacheBefore, cacheAfter)
	}
}

func TestInit_CorruptSettingsReturnsWrappedError(t *testing.T) {
	base := t.TempDir()
	w := New(base)

	if err := os.WriteFile(filepath.Join(base, config.SettingsFile), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seed bad settings: %v", err)
	}

	_, err := w.Init("/any/pwd")
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
	w := New(base)
	s := &config.Settings{Version: config.CurrentVersion, Workspaces: map[string]config.Workspace{}}

	// Empty settings + deep path: must terminate at the filesystem root.
	_, _, ok := w.findAncestor(s, "/a/b/c/d/e/f")
	if ok {
		t.Errorf("findAncestor returned ok=true on empty settings")
	}
}

// TestInit_ConcurrentDistinctPwdsAllRegistered is the lost-update regression
// test: N concurrent Init calls for N distinct pwds under one baseDir must all
// end up registered in settings.json (none silently dropped).
func TestInit_ConcurrentDistinctPwdsAllRegistered(t *testing.T) {
	base := t.TempDir()
	w := New(base)

	const n = 10
	root := evalSymlinks(t, t.TempDir())
	pwds := make([]string, n)
	for i := 0; i < n; i++ {
		pwds[i] = filepath.Join(root, strings.Repeat("a", i+1))
		if err := os.MkdirAll(pwds[i], 0o755); err != nil {
			t.Fatalf("mkdir pwd[%d]: %v", i, err)
		}
	}

	var wg sync.WaitGroup
	wg.Add(n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			_, errs[i] = w.Init(pwds[i])
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d Init(%q): %v", i, pwds[i], err)
		}
	}

	s, err := config.Load(base)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for i, pwd := range pwds {
		if _, ok := s.Workspaces[pwd]; !ok {
			t.Errorf("pwd[%d] %q missing from settings (lost update)", i, pwd)
		}
	}
	if len(s.Workspaces) != n {
		t.Errorf("Workspaces count = %d, want %d", len(s.Workspaces), n)
	}
}

func TestFindAncestor_RootRegistered(t *testing.T) {
	base := t.TempDir()
	w := New(base)
	rootKey := string(filepath.Separator)
	s := &config.Settings{
		Version: config.CurrentVersion,
		Workspaces: map[string]config.Workspace{
			rootKey: {Name: "root-aabbcc", CreatedAt: time.Now().UTC()},
		},
	}
	matched, ws, ok := w.findAncestor(s, "/some/deep/path")
	if !ok {
		t.Fatal("expected ancestor match when root is registered")
	}
	if matched != rootKey {
		t.Errorf("matched = %q, want %q", matched, rootKey)
	}
	if ws.Name != "root-aabbcc" {
		t.Errorf("ws.Name = %q, want root-aabbcc", ws.Name)
	}
}
