package projectconfig

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// skipNonPOSIX skips on non-POSIX hosts per the CLAUDE.md POSIX-only invariant.
func skipNonPOSIX(t *testing.T, why string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip(why)
	}
}

// evalSymlinks resolves a temp dir path — on macOS /tmp is a symlink, so raw
// t.TempDir() paths violate the EvalSymlinks precondition.
func evalSymlinks(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("eval symlinks %s: %v", dir, err)
	}
	return resolved
}

func TestScaffold_WritesStub(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	if err := Scaffold(root, Cache{Content: true, Agent: true}); err != nil {
		t.Fatalf("Scaffold returned error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(root, Filename))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(Stub) {
		t.Errorf("file content mismatch:\ngot:  %q\nwant: %q", got, Stub)
	}
}

func TestScaffold_Idempotent(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	userContent := []byte("# my custom config\nexclude:\n  dirs:\n    - secrets\n  files: []\n")
	if err := os.WriteFile(filepath.Join(root, Filename), userContent, 0o644); err != nil {
		t.Fatalf("pre-write: %v", err)
	}

	if err := Scaffold(root, Cache{Content: true, Agent: true}); err != nil {
		t.Fatalf("Scaffold on existing file returned error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(root, Filename))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(userContent) {
		t.Errorf("user content was modified:\ngot:  %q\nwant: %q", got, userContent)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	excl, _, _, err := Load(root)
	if err != nil {
		t.Fatalf("Load on missing file: %v", err)
	}
	if len(excl.Files) != 0 || len(excl.Dirs) != 0 {
		t.Errorf("expected zero Files/Dirs, got %+v", excl)
	}
	if excl.Patterns != nil {
		t.Errorf("expected nil Patterns for missing file, got %v", excl.Patterns)
	}
	if excl.SkipDirs != nil {
		t.Errorf("expected nil SkipDirs for missing file, got %v", excl.SkipDirs)
	}
}

func TestLoad_DefaultStub_RoundTrips(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	if err := os.WriteFile(filepath.Join(root, Filename), Stub, 0o644); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	excl, cacheCfg, _, err := Load(root)
	if err != nil {
		t.Fatalf("Load on default stub: %v", err)
	}
	if len(excl.Files) != 0 || len(excl.Dirs) != 0 {
		t.Errorf("expected zero Files/Dirs from default stub, got files=%v dirs=%v", excl.Files, excl.Dirs)
	}
	if !cacheCfg.Content || !cacheCfg.Agent {
		t.Errorf("Cache from default Stub: got {Content:%v Agent:%v}, want {true, true}", cacheCfg.Content, cacheCfg.Agent)
	}
	// Must mirror Stub exactly (sorted); update if Stub changes.
	wantPatterns := []string{
		"*.env",
		"*.key",
		"*.kubeconfig",
		"*.p12",
		"*.pem",
		"*.pfx",
		"*.tfstate",
		".env.*",
		".git-credentials",
		".htpasswd",
		".netrc",
		".npmrc",
		".pypirc",
		"id_ed25519*",
		"id_rsa*",
		"kubeconfig",
		"service-account*.json",
	}
	if !stringSlicesEqual(excl.Patterns, wantPatterns) {
		t.Errorf("Patterns: got %v, want %v\n(if Stub changed, update wantPatterns to match)", excl.Patterns, wantPatterns)
	}
	// Must mirror Stub exactly (sorted); update if Stub changes.
	wantSkipDirs := []string{".git", ".venv", "node_modules", "vendor"}
	if !stringSlicesEqual(excl.SkipDirs, wantSkipDirs) {
		t.Errorf("SkipDirs: got %v, want %v\n(if Stub changed, update wantSkipDirs to match)", excl.SkipDirs, wantSkipDirs)
	}
}

// yaml.NewDecoder returns io.EOF for these; Load must treat it as zero config.
func TestLoad_EmptyAndCommentOnlyFiles(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")

	cases := []struct {
		name    string
		content []byte
	}{
		{"empty bytes", []byte{}},
		{"whitespace only", []byte("   \n   \n")},
		{"comment only", []byte("# just a comment\n# another comment\n")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := evalSymlinks(t, t.TempDir())
			if err := os.WriteFile(filepath.Join(root, Filename), tc.content, 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			excl, cacheCfg, _, err := Load(root)
			if err != nil {
				t.Fatalf("Load returned error for %q: %v", tc.name, err)
			}
			if len(excl.Files) != 0 || len(excl.Dirs) != 0 {
				t.Errorf("expected zero Files/Dirs for %q, got %+v", tc.name, excl)
			}
			if excl.Patterns != nil {
				t.Errorf("expected nil Patterns for %q, got %v", tc.name, excl.Patterns)
			}
			if excl.SkipDirs != nil {
				t.Errorf("expected nil SkipDirs for %q, got %v", tc.name, excl.SkipDirs)
			}
			if !cacheCfg.Content {
				t.Errorf("Cache.Content: got false, want true for empty/comment-only file %q", tc.name)
			}
			if !cacheCfg.Agent {
				t.Errorf("Cache.Agent: got false, want true for empty/comment-only file %q", tc.name)
			}
		})
	}
}

func TestLoad_MalformedYAML(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	if err := os.WriteFile(filepath.Join(root, Filename), []byte(":\tnot valid yaml{{{\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, _, _, err := Load(root)
	if err == nil {
		t.Fatal("expected error for malformed YAML, got nil")
	}
	if !strings.HasPrefix(err.Error(), "projectconfig:") {
		t.Errorf("error does not have 'projectconfig:' prefix: %q", err.Error())
	}
}

func TestLoad_UnknownField(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	if err := os.WriteFile(filepath.Join(root, Filename), []byte("include:\n  files: []\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, _, _, err := Load(root)
	if err == nil {
		t.Fatal("expected error for unknown field, got nil")
	}
	if !strings.HasPrefix(err.Error(), "projectconfig:") {
		t.Errorf("error does not have 'projectconfig:' prefix: %q", err.Error())
	}
}

func TestLoad_ValidationRules(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")

	cases := []struct {
		name        string
		yaml        string
		wantErrFrag string
	}{
		{
			name:        "absolute path in files",
			yaml:        "exclude:\n  files:\n    - /etc/passwd\n  dirs: []\n",
			wantErrFrag: "absolute path",
		},
		{
			name:        "absolute path in dirs",
			yaml:        "exclude:\n  dirs:\n    - /tmp/secrets\n  files: []\n",
			wantErrFrag: "absolute path",
		},
		{
			name:        "empty string in files",
			yaml:        "exclude:\n  files:\n    - \"\"\n  dirs: []\n",
			wantErrFrag: "empty path",
		},
		{
			name:        "empty string in dirs",
			yaml:        "exclude:\n  dirs:\n    - \"\"\n  files: []\n",
			wantErrFrag: "empty path",
		},
		{
			name:        "dotdot escape in files",
			yaml:        "exclude:\n  files:\n    - ../secret\n  dirs: []\n",
			wantErrFrag: "escapes project root",
		},
		{
			name:        "dotdot escape in dirs",
			yaml:        "exclude:\n  dirs:\n    - ../../up\n  files: []\n",
			wantErrFrag: "escapes project root",
		},
		{
			name:        "dot refers to project root in files",
			yaml:        "exclude:\n  files:\n    - .\n  dirs: []\n",
			wantErrFrag: "refers to project root",
		},
		{
			name:        "dot refers to project root in dirs",
			yaml:        "exclude:\n  dirs:\n    - .\n  files: []\n",
			wantErrFrag: "refers to project root",
		},
		{
			name:        "foo/.. cleans to dot in files",
			yaml:        "exclude:\n  files:\n    - foo/..\n  dirs: []\n",
			wantErrFrag: "refers to project root",
		},
		{
			name:        "foo/.. cleans to dot in dirs",
			yaml:        "exclude:\n  dirs:\n    - foo/..\n  files: []\n",
			wantErrFrag: "refers to project root",
		},
		{
			name:        "environments key with equals sign",
			yaml:        "environments:\n  \"A=B\": value\n",
			wantErrFrag: "must not contain '='",
		},
		{
			name:        "environments non-scalar value",
			yaml:        "environments:\n  FOO:\n    - a\n    - b\n",
			wantErrFrag: "must be a scalar value",
		},
		{
			name:        "environments null value",
			yaml:        "environments:\n  KEY: null\n",
			wantErrFrag: "has no value",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := evalSymlinks(t, t.TempDir())
			if err := os.WriteFile(filepath.Join(root, Filename), []byte(tc.yaml), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, _, _, err := Load(root)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErrFrag)
			}
			if !strings.Contains(err.Error(), tc.wantErrFrag) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErrFrag)
			}
			if !strings.HasPrefix(err.Error(), "projectconfig:") {
				t.Errorf("error missing 'projectconfig:' prefix: %q", err.Error())
			}
		})
	}
}

func TestLoad_ReservedPaths(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")

	for _, reserved := range []string{".claude", ".codex", "docs", "CLAUDE.md", ".makeslop.yaml"} {
		t.Run("dirs/"+reserved, func(t *testing.T) {
			root := evalSymlinks(t, t.TempDir())
			content := "exclude:\n  dirs:\n    - " + reserved + "\n  files: []\n"
			if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, _, _, err := Load(root)
			if err == nil {
				t.Fatalf("expected collision error for %q in dirs, got nil", reserved)
			}
			if !strings.Contains(err.Error(), "reserved agent path") {
				t.Errorf("error %q does not mention 'reserved agent path'", err.Error())
			}
		})
		t.Run("files/"+reserved, func(t *testing.T) {
			root := evalSymlinks(t, t.TempDir())
			content := "exclude:\n  dirs: []\n  files:\n    - " + reserved + "\n"
			if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, _, _, err := Load(root)
			if err == nil {
				t.Fatalf("expected collision error for %q in files, got nil", reserved)
			}
			if !strings.Contains(err.Error(), "reserved agent path") {
				t.Errorf("error %q does not mention 'reserved agent path'", err.Error())
			}
		})
	}
}

func TestLoad_CrossListDuplicate(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	content := "exclude:\n  files:\n    - mydir/secret\n  dirs:\n    - mydir/secret\n"
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, _, _, err := Load(root)
	if err == nil {
		t.Fatal("expected error for cross-list duplicate, got nil")
	}
	if !strings.Contains(err.Error(), "listed in both") {
		t.Errorf("error %q does not contain 'listed in both'", err.Error())
	}
}

// Regression: the cross-list duplicate check must fire even when the path does
// not exist on disk (deterministic error, independent of on-disk state).
func TestLoad_CrossListDuplicate_NoFileOnDisk(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	content := "exclude:\n  files:\n    - ghost\n  dirs:\n    - ghost\n"
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, _, _, err := Load(root)
	if err == nil {
		t.Fatal("expected error for cross-list duplicate (path absent), got nil")
	}
	if !strings.Contains(err.Error(), "listed in both") {
		t.Errorf("error %q does not contain 'listed in both'", err.Error())
	}
}

func TestLoad_SilentlyDropsMissingEntries(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	content := "exclude:\n  files:\n    - nonexistent/api.key\n  dirs:\n    - phantom-dir\n"
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	excl, _, _, err := Load(root)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(excl.Files) != 0 || len(excl.Dirs) != 0 {
		t.Errorf("expected empty result, got %+v", excl)
	}
}

func TestLoad_DropsWrongType(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	if err := os.WriteFile(filepath.Join(root, "am-a-file"), []byte("data"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "am-a-dir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Deliberately cross-wired: file under dirs and dir under files; both drop.
	content := "exclude:\n  dirs:\n    - am-a-file\n  files:\n    - am-a-dir\n"
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	excl, _, _, err := Load(root)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(excl.Files) != 0 || len(excl.Dirs) != 0 {
		t.Errorf("expected empty result (wrong-type drops), got %+v", excl)
	}
	// Non-symlink wrong-type drops must be silent — no warnings.
	if len(excl.Warnings) != 0 {
		t.Errorf("expected no warnings for non-symlink wrong-type drops, got %v", excl.Warnings)
	}
}

// TestLoad_DropsSymlinks verifies that symlinks in exclude.files and exclude.dirs
// are dropped from masking and produce entries in Excludes.Warnings.
func TestLoad_DropsSymlinks(t *testing.T) {
	skipNonPOSIX(t, "symlinks and /‐paths required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	realFile := filepath.Join(root, "real-file")
	realDir := filepath.Join(root, "real-dir")
	if err := os.WriteFile(realFile, []byte("data"), 0o644); err != nil {
		t.Fatalf("write real file: %v", err)
	}
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatalf("mkdir real dir: %v", err)
	}

	symlinkToFile := filepath.Join(root, "link-to-file")
	symlinkToDir := filepath.Join(root, "link-to-dir")
	if err := os.Symlink(realFile, symlinkToFile); err != nil {
		t.Fatalf("symlink to file: %v", err)
	}
	if err := os.Symlink(realDir, symlinkToDir); err != nil {
		t.Fatalf("symlink to dir: %v", err)
	}

	content := "exclude:\n  files:\n    - link-to-file\n  dirs:\n    - link-to-dir\n"
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	excl, _, _, err := Load(root)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(excl.Files) != 0 || len(excl.Dirs) != 0 {
		t.Errorf("expected symlinks to be dropped from masking, got %+v", excl)
	}
	// Both symlinks must produce warnings.
	if len(excl.Warnings) != 2 {
		t.Fatalf("expected 2 warnings for symlinks, got %d: %v", len(excl.Warnings), excl.Warnings)
	}
	for _, w := range excl.Warnings {
		if !strings.Contains(w, "is a symlink and is NOT masked") {
			t.Errorf("warning %q does not mention 'is a symlink and is NOT masked'", w)
		}
	}
}

// TestLoad_SymlinkInFiles_Warning checks that a symlinked entry in exclude.files
// produces a warning and is dropped (not masked).
func TestLoad_SymlinkInFiles_Warning(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	realFile := filepath.Join(root, "real.key")
	if err := os.WriteFile(realFile, []byte("key data"), 0o600); err != nil {
		t.Fatalf("write real file: %v", err)
	}
	linkName := filepath.Join(root, "link.key")
	if err := os.Symlink(realFile, linkName); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	content := "exclude:\n  files:\n    - link.key\n  dirs: []\n"
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	excl, _, _, err := Load(root)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(excl.Files) != 0 {
		t.Errorf("expected symlink dropped from files mask, got %v", excl.Files)
	}
	if len(excl.Warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(excl.Warnings), excl.Warnings)
	}
	if !strings.Contains(excl.Warnings[0], "link.key") {
		t.Errorf("warning %q does not mention the symlink path 'link.key'", excl.Warnings[0])
	}
	if !strings.Contains(excl.Warnings[0], "is NOT masked") {
		t.Errorf("warning %q does not contain 'is NOT masked'", excl.Warnings[0])
	}
}

// TestLoad_SymlinkInDirs_Warning checks that a symlinked entry in exclude.dirs
// produces a warning and is dropped (not masked).
func TestLoad_SymlinkInDirs_Warning(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	realDir := filepath.Join(root, "real-secrets")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	linkName := filepath.Join(root, "link-secrets")
	if err := os.Symlink(realDir, linkName); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	content := "exclude:\n  dirs:\n    - link-secrets\n  files: []\n"
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	excl, _, _, err := Load(root)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(excl.Dirs) != 0 {
		t.Errorf("expected symlink dropped from dirs mask, got %v", excl.Dirs)
	}
	if len(excl.Warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(excl.Warnings), excl.Warnings)
	}
	if !strings.Contains(excl.Warnings[0], "link-secrets") {
		t.Errorf("warning %q does not mention the symlink path 'link-secrets'", excl.Warnings[0])
	}
}

// TestLoad_WrongTypeDrop_NoWarning verifies that a non-symlink wrong-type drop
// (e.g. a directory listed in exclude.files) stays silent (no warning).
func TestLoad_WrongTypeDrop_NoWarning(t *testing.T) {
	root := evalSymlinks(t, t.TempDir())

	if err := os.WriteFile(filepath.Join(root, "am-a-file"), []byte("data"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "am-a-dir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Cross-wired: file listed under dirs, dir listed under files — both drop silently.
	content := "exclude:\n  dirs:\n    - am-a-file\n  files:\n    - am-a-dir\n"
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	excl, _, _, err := Load(root)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(excl.Files) != 0 || len(excl.Dirs) != 0 {
		t.Errorf("expected empty result (wrong-type drops), got %+v", excl)
	}
	if len(excl.Warnings) != 0 {
		t.Errorf("expected no warnings for non-symlink wrong-type drops, got %v", excl.Warnings)
	}
}

// TestLoad_NoWarnings_AbsentFile confirms no warnings for a missing file (zero
// config returned, Warnings nil).
func TestLoad_NoWarnings_AbsentFile(t *testing.T) {
	root := evalSymlinks(t, t.TempDir())

	excl, _, _, err := Load(root)
	if err != nil {
		t.Fatalf("Load on missing file: %v", err)
	}
	if len(excl.Warnings) != 0 {
		t.Errorf("expected no warnings for absent file, got %v", excl.Warnings)
	}
}

// TestStub_ContainsNewPatterns verifies the 8 new patterns are present in the stub.
func TestStub_ContainsNewPatterns(t *testing.T) {
	newPatterns := []string{
		"*.p12",
		"*.pfx",
		"*.tfstate",
		".pypirc",
		".htpasswd",
		"service-account*.json",
		"kubeconfig",
		"*.kubeconfig",
	}
	stubStr := string(Stub)
	for _, p := range newPatterns {
		if !strings.Contains(stubStr, p) {
			t.Errorf("Stub does not contain new pattern %q", p)
		}
	}
}

// TODO(testing): statFilter's non-ErrNotExist stat error path is untested —
// it needs chmod on a directory, which is fragile under root/CI containers.

func TestLoad_DeduplicatesWithinLists(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	// Real files/dirs so entries survive the stat filter.
	if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte("s"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "privdir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	content := "exclude:\n  files:\n    - secret.txt\n    - secret.txt\n  dirs:\n    - privdir\n    - privdir\n"
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	excl, _, _, err := Load(root)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(excl.Files) != 1 {
		t.Errorf("expected 1 file after dedup, got %d: %v", len(excl.Files), excl.Files)
	}
	if len(excl.Dirs) != 1 {
		t.Errorf("expected 1 dir after dedup, got %d: %v", len(excl.Dirs), excl.Dirs)
	}
}

func TestLoad_ReturnsAbsoluteSortedPaths(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	// Names listed z-before-a so a wrong (unsorted) result is detectable.
	files := []string{"z-secret.txt", "a-secret.txt"}
	dirs := []string{"z-priv", "a-priv"}

	for _, f := range files {
		if err := os.WriteFile(filepath.Join(root, f), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	for _, d := range dirs {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	content := "exclude:\n  files:\n    - z-secret.txt\n    - a-secret.txt\n  dirs:\n    - z-priv\n    - a-priv\n"
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	excl, _, _, err := Load(root)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	wantFiles := []string{
		filepath.Join(root, "a-secret.txt"),
		filepath.Join(root, "z-secret.txt"),
	}
	wantDirs := []string{
		filepath.Join(root, "a-priv"),
		filepath.Join(root, "z-priv"),
	}

	if len(excl.Files) != len(wantFiles) {
		t.Fatalf("files len: got %d, want %d; got=%v", len(excl.Files), len(wantFiles), excl.Files)
	}
	for i := range wantFiles {
		if excl.Files[i] != wantFiles[i] {
			t.Errorf("files[%d]: got %q, want %q", i, excl.Files[i], wantFiles[i])
		}
	}

	if len(excl.Dirs) != len(wantDirs) {
		t.Fatalf("dirs len: got %d, want %d; got=%v", len(excl.Dirs), len(wantDirs), excl.Dirs)
	}
	for i := range wantDirs {
		if excl.Dirs[i] != wantDirs[i] {
			t.Errorf("dirs[%d]: got %q, want %q", i, excl.Dirs[i], wantDirs[i])
		}
	}
}

// A stale "network:" block (from a prior proxy-egress makeslop version) must be
// rejected by strict decode — the intended loud break for old config files.
func TestLoad_Network_BlockRejected(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")

	cases := []struct {
		name    string
		content string
	}{
		{
			"proxy address set",
			"exclude:\n  dirs: []\n  files: []\nnetwork:\n  proxy:\n    address: 10.0.0.5:3128\n",
		},
		{
			"empty proxy address",
			"exclude:\n  dirs: []\n  files: []\nnetwork:\n  proxy:\n    address: \"\"\n",
		},
		{
			"network block only",
			"network:\n  proxy:\n    address: \"\"\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := evalSymlinks(t, t.TempDir())
			if err := os.WriteFile(filepath.Join(root, Filename), []byte(tc.content), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}

			_, _, _, err := Load(root)
			if err == nil {
				t.Fatal("expected error for stale network: block, got nil")
			}
			if !strings.HasPrefix(err.Error(), "projectconfig:") {
				t.Errorf("error missing 'projectconfig:' prefix: %q", err.Error())
			}
		})
	}
}

func TestLoad_Scan_ValidPatternsAndSkipDirs(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")

	cases := []struct {
		name         string
		yaml         string
		wantPatterns []string
		wantSkipDirs []string
	}{
		{
			name:         "basic patterns and skip-dirs",
			yaml:         "exclude:\n  scan:\n    patterns:\n      - \"*.env\"\n      - \".env.*\"\n      - \"*.pem\"\n    skip-dirs:\n      - .git\n      - node_modules\n  dirs: []\n  files: []\n",
			wantPatterns: []string{"*.env", "*.pem", ".env.*"},
			wantSkipDirs: []string{".git", "node_modules"},
		},
		{
			name:         "empty scan section",
			yaml:         "exclude:\n  scan:\n    patterns: []\n    skip-dirs: []\n  dirs: []\n  files: []\n",
			wantPatterns: nil,
			wantSkipDirs: nil,
		},
		{
			name:         "patterns deduped and sorted",
			yaml:         "exclude:\n  scan:\n    patterns:\n      - \"*.pem\"\n      - \"*.env\"\n      - \"*.pem\"\n    skip-dirs: []\n  dirs: []\n  files: []\n",
			wantPatterns: []string{"*.env", "*.pem"},
			wantSkipDirs: nil,
		},
		{
			name:         "skip-dirs deduped and sorted",
			yaml:         "exclude:\n  scan:\n    patterns: []\n    skip-dirs:\n      - vendor\n      - .git\n      - vendor\n  dirs: []\n  files: []\n",
			wantPatterns: nil,
			wantSkipDirs: []string{".git", "vendor"},
		},
		{
			name:         "absent scan section yields nil slices",
			yaml:         "exclude:\n  dirs: []\n  files: []\n",
			wantPatterns: nil,
			wantSkipDirs: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := evalSymlinks(t, t.TempDir())
			if err := os.WriteFile(filepath.Join(root, Filename), []byte(tc.yaml), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			excl, _, _, err := Load(root)
			if err != nil {
				t.Fatalf("Load returned error: %v", err)
			}
			if !stringSlicesEqual(excl.Patterns, tc.wantPatterns) {
				t.Errorf("Patterns: got %v, want %v", excl.Patterns, tc.wantPatterns)
			}
			if !stringSlicesEqual(excl.SkipDirs, tc.wantSkipDirs) {
				t.Errorf("SkipDirs: got %v, want %v", excl.SkipDirs, tc.wantSkipDirs)
			}
		})
	}
}

func TestLoad_Scan_InvalidPatterns(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")

	cases := []struct {
		name        string
		yaml        string
		wantErrFrag string
	}{
		{
			name:        "bad glob bracket",
			yaml:        "exclude:\n  scan:\n    patterns:\n      - \"[bad\"\n    skip-dirs: []\n  dirs: []\n  files: []\n",
			wantErrFrag: "invalid scan pattern",
		},
		{
			name:        "empty pattern entry",
			yaml:        "exclude:\n  scan:\n    patterns:\n      - \"\"\n    skip-dirs: []\n  dirs: []\n  files: []\n",
			wantErrFrag: "empty pattern",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := evalSymlinks(t, t.TempDir())
			if err := os.WriteFile(filepath.Join(root, Filename), []byte(tc.yaml), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, _, _, err := Load(root)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErrFrag)
			}
			if !strings.Contains(err.Error(), tc.wantErrFrag) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErrFrag)
			}
			if !strings.HasPrefix(err.Error(), "projectconfig:") {
				t.Errorf("error missing 'projectconfig:' prefix: %q", err.Error())
			}
		})
	}
}

func TestLoad_Scan_InvalidSkipDirs(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")

	cases := []struct {
		name        string
		yaml        string
		wantErrFrag string
	}{
		{
			name:        "skip-dir with path separator foo/bar",
			yaml:        "exclude:\n  scan:\n    patterns: []\n    skip-dirs:\n      - foo/bar\n  dirs: []\n  files: []\n",
			wantErrFrag: "bare directory name",
		},
		{
			name:        "skip-dir dot",
			yaml:        "exclude:\n  scan:\n    patterns: []\n    skip-dirs:\n      - \".\"\n  dirs: []\n  files: []\n",
			wantErrFrag: "bare directory name",
		},
		{
			name:        "skip-dir dotdot",
			yaml:        "exclude:\n  scan:\n    patterns: []\n    skip-dirs:\n      - \"..\"\n  dirs: []\n  files: []\n",
			wantErrFrag: "bare directory name",
		},
		{
			name:        "empty skip-dir entry",
			yaml:        "exclude:\n  scan:\n    patterns: []\n    skip-dirs:\n      - \"\"\n  dirs: []\n  files: []\n",
			wantErrFrag: "empty entry",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := evalSymlinks(t, t.TempDir())
			if err := os.WriteFile(filepath.Join(root, Filename), []byte(tc.yaml), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, _, _, err := Load(root)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErrFrag)
			}
			if !strings.Contains(err.Error(), tc.wantErrFrag) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErrFrag)
			}
			if !strings.HasPrefix(err.Error(), "projectconfig:") {
				t.Errorf("error missing 'projectconfig:' prefix: %q", err.Error())
			}
		})
	}
}

func TestLoad_Scan_UnknownKeyRejected(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	content := "exclude:\n  scan:\n    patterns: []\n    skip-dirs: []\n    unknown-key: oops\n  dirs: []\n  files: []\n"
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, _, _, err := Load(root)
	if err == nil {
		t.Fatal("expected error for unknown key under exclude.scan, got nil")
	}
	if !strings.HasPrefix(err.Error(), "projectconfig:") {
		t.Errorf("error missing 'projectconfig:' prefix: %q", err.Error())
	}
}

// An absent cache: block defaults both fields to true (backward-compatible).
func TestLoad_Cache_AbsentBlock(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	content := "exclude:\n  dirs: []\n  files: []\n"
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, cacheCfg, _, err := Load(root)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if !cacheCfg.Content {
		t.Errorf("Cache.Content: got false, want true (absent block should default to true)")
	}
	if !cacheCfg.Agent {
		t.Errorf("Cache.Agent: got false, want true (absent block should default to true)")
	}
}

func TestLoad_Cache_MissingFile(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	_, cacheCfg, _, err := Load(root)
	if err != nil {
		t.Fatalf("Load on missing file: %v", err)
	}
	if !cacheCfg.Content {
		t.Errorf("Cache.Content: got false, want true for missing file")
	}
	if !cacheCfg.Agent {
		t.Errorf("Cache.Agent: got false, want true for missing file")
	}
}

func TestLoad_Cache_BothFalse(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	content := "cache:\n  content: false\n  agent: false\n"
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, cacheCfg, _, err := Load(root)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cacheCfg.Content {
		t.Errorf("Cache.Content: got true, want false")
	}
	if cacheCfg.Agent {
		t.Errorf("Cache.Agent: got true, want false")
	}
}

func TestLoad_Cache_BothTrue(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	content := "cache:\n  content: true\n  agent: true\n"
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, cacheCfg, _, err := Load(root)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if !cacheCfg.Content {
		t.Errorf("Cache.Content: got false, want true")
	}
	if !cacheCfg.Agent {
		t.Errorf("Cache.Agent: got false, want true")
	}
}

// content:false with agent absent → Cache{false,true} (absent field defaults true).
func TestLoad_Cache_MixedContentFalseAgentAbsent(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	content := "cache:\n  content: false\n"
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, cacheCfg, _, err := Load(root)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cacheCfg.Content {
		t.Errorf("Cache.Content: got true, want false")
	}
	if !cacheCfg.Agent {
		t.Errorf("Cache.Agent: got false, want true (absent field defaults to true)")
	}
}

// agent:false with content absent → Cache{true,false}.
func TestLoad_Cache_MixedAgentFalseContentAbsent(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	content := "cache:\n  agent: false\n"
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, cacheCfg, _, err := Load(root)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if !cacheCfg.Content {
		t.Errorf("Cache.Content: got false, want true (absent field defaults to true)")
	}
	if cacheCfg.Agent {
		t.Errorf("Cache.Agent: got true, want false")
	}
}

func TestLoad_Cache_UnknownKeyRejected(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	content := "cache:\n  content: true\n  agent: true\n  typo: bad\n"
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, _, _, err := Load(root)
	if err == nil {
		t.Fatal("expected error for unknown key under cache:, got nil")
	}
	if !strings.HasPrefix(err.Error(), "projectconfig:") {
		t.Errorf("error missing 'projectconfig:' prefix: %q", err.Error())
	}
}

// renderStub(Cache{true,true}) must round-trip through Load to Cache{true,true}.
func TestRenderStub_TrueTrue(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	data := renderStub(Cache{Content: true, Agent: true})
	if err := os.WriteFile(filepath.Join(root, Filename), data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, cacheCfg, _, err := Load(root)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if !cacheCfg.Content {
		t.Errorf("Cache.Content: got false, want true")
	}
	if !cacheCfg.Agent {
		t.Errorf("Cache.Agent: got false, want true")
	}
}

// renderStub(Cache{false,false}) must round-trip through Load to Cache{false,false}.
func TestRenderStub_FalseFalse(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	data := renderStub(Cache{Content: false, Agent: false})
	if err := os.WriteFile(filepath.Join(root, Filename), data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, cacheCfg, _, err := Load(root)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cacheCfg.Content {
		t.Errorf("Cache.Content: got true, want false")
	}
	if cacheCfg.Agent {
		t.Errorf("Cache.Agent: got true, want false")
	}
}

func TestScaffold_CacheFalseFalse(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	if err := Scaffold(root, Cache{Content: false, Agent: false}); err != nil {
		t.Fatalf("Scaffold returned error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(root, Filename))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	want := renderStub(Cache{Content: false, Agent: false})
	if string(got) != string(want) {
		t.Errorf("file content mismatch:\ngot:  %q\nwant: %q", got, want)
	}

	_, cacheCfg, _, err := Load(root)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cacheCfg.Content {
		t.Errorf("Cache.Content: got true, want false")
	}
	if cacheCfg.Agent {
		t.Errorf("Cache.Agent: got true, want false")
	}
}

// A second Scaffold with different Cache values must be a no-op: EEXIST wins
// over the c parameter, so user edits are never clobbered.
func TestScaffold_IdempotentWithDifferentCache(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	if err := Scaffold(root, Cache{Content: true, Agent: true}); err != nil {
		t.Fatalf("first Scaffold returned error: %v", err)
	}

	first, err := os.ReadFile(filepath.Join(root, Filename))
	if err != nil {
		t.Fatalf("ReadFile after first scaffold: %v", err)
	}

	if err := Scaffold(root, Cache{Content: false, Agent: false}); err != nil {
		t.Fatalf("second Scaffold returned error: %v", err)
	}

	second, err := os.ReadFile(filepath.Join(root, Filename))
	if err != nil {
		t.Fatalf("ReadFile after second scaffold: %v", err)
	}

	if string(first) != string(second) {
		t.Errorf("second Scaffold clobbered the file:\nfirst:  %q\nsecond: %q", first, second)
	}
}

// Stub must equal renderStub(Cache{true,true}) so callers comparing against Stub work.
func TestStub_MatchesDefaultRenderStub(t *testing.T) {
	want := renderStub(Cache{Content: true, Agent: true})
	if string(Stub) != string(want) {
		t.Errorf("Stub does not match renderStub(Cache{true,true}):\nStub:  %q\nwant: %q", Stub, want)
	}
}

func TestValidateEnvironments_ValidMap(t *testing.T) {
	nodes := map[string]yaml.Node{
		"NODE_ENV":     {Kind: yaml.ScalarNode, Tag: "!!str", Value: "production"},
		"LOG_LEVEL":    {Kind: yaml.ScalarNode, Tag: "!!str", Value: "info"},
		"API_BASE_URL": {Kind: yaml.ScalarNode, Tag: "!!str", Value: "https://api.example.com"},
	}
	got, err := validateEnvironments(nodes)
	if err != nil {
		t.Fatalf("validateEnvironments error: %v", err)
	}
	want := []string{
		"API_BASE_URL=https://api.example.com",
		"LOG_LEVEL=info",
		"NODE_ENV=production",
	}
	if !stringSlicesEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestValidateEnvironments_ScalarCoercion(t *testing.T) {
	// validateEnvironments reads node.Value (raw string form), so numeric/boolean
	// scalars coerce: PORT: 8080 → "8080", DEBUG: true → "true".
	nodes := map[string]yaml.Node{
		"PORT":  {Kind: yaml.ScalarNode, Tag: "!!int", Value: "8080"},
		"DEBUG": {Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"},
	}
	got, err := validateEnvironments(nodes)
	if err != nil {
		t.Fatalf("validateEnvironments error: %v", err)
	}
	want := []string{"DEBUG=true", "PORT=8080"}
	if !stringSlicesEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// A key with '=' is rejected (would produce "A=B=value", which Docker misparses).
func TestValidateEnvironments_EqualsInKeyRejected(t *testing.T) {
	valNode := yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "val"}
	_, err := validateEnvironments(map[string]yaml.Node{"A=B": valNode})
	if err == nil {
		t.Fatal("expected error for key with '=', got nil")
	}
	if !strings.Contains(err.Error(), "must not contain '='") {
		t.Errorf("error %q does not contain \"must not contain '='\"", err.Error())
	}
	if !strings.HasPrefix(err.Error(), "projectconfig:") {
		t.Errorf("error missing 'projectconfig:' prefix: %q", err.Error())
	}
}

// A value with a newline or tab is rejected (breaks ShellCommand dry-run output).
func TestValidateEnvironments_NewlineOrTabInValueRejected(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"LF", "line1\nline2"},
		{"CR", "line1\rline2"},
		{"TAB", "val1\tval2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			valNode := yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: tc.value}
			_, err := validateEnvironments(map[string]yaml.Node{"KEY": valNode})
			if err == nil {
				t.Fatalf("expected error for newline/tab in value (%s), got nil", tc.name)
			}
			if !strings.Contains(err.Error(), "must not contain newline or tab characters") {
				t.Errorf("error %q does not contain 'must not contain newline or tab characters'", err.Error())
			}
			if !strings.HasPrefix(err.Error(), "projectconfig:") {
				t.Errorf("error missing 'projectconfig:' prefix: %q", err.Error())
			}
		})
	}
}

// A key with \n, \r, or \t is rejected (would corrupt the "KEY=value" entry).
func TestValidateEnvironments_NewlineOrTabInKeyRejected(t *testing.T) {
	cases := []struct {
		name string
		key  string
	}{
		{"LF", "KEY\nNAME"},
		{"CR", "KEY\rNAME"},
		{"TAB", "KEY\tNAME"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			valNode := yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "val"}
			_, err := validateEnvironments(map[string]yaml.Node{tc.key: valNode})
			if err == nil {
				t.Fatalf("expected error for key with %s character, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), "must not contain newline or tab characters") {
				t.Errorf("error %q does not contain expected message", err.Error())
			}
			if !strings.HasPrefix(err.Error(), "projectconfig:") {
				t.Errorf("error missing 'projectconfig:' prefix: %q", err.Error())
			}
		})
	}
}

// An empty key is rejected. Tested directly because yaml.v3 can't represent an
// unquoted empty map key.
func TestValidateEnvironments_EmptyKeyRejected(t *testing.T) {
	valNode := yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "val"}
	_, err := validateEnvironments(map[string]yaml.Node{"": valNode})
	if err == nil {
		t.Fatal("expected error for empty key, got nil")
	}
	if !strings.Contains(err.Error(), "empty key") {
		t.Errorf("error %q does not contain 'empty key'", err.Error())
	}
	if !strings.HasPrefix(err.Error(), "projectconfig:") {
		t.Errorf("error missing 'projectconfig:' prefix: %q", err.Error())
	}
}

// Non-scalar values (sequences, mappings) are rejected fail-loud.
func TestValidateEnvironments_NonScalarRejected(t *testing.T) {
	cases := []struct {
		name string
		node yaml.Node
	}{
		{
			"sequence value",
			yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"},
		},
		{
			"mapping value",
			yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateEnvironments(map[string]yaml.Node{"FOO": tc.node})
			if err == nil {
				t.Fatalf("expected error for non-scalar %s, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), "must be a scalar value") {
				t.Errorf("error %q does not contain 'must be a scalar value'", err.Error())
			}
			if !strings.HasPrefix(err.Error(), "projectconfig:") {
				t.Errorf("error missing 'projectconfig:' prefix: %q", err.Error())
			}
		})
	}
}

// Null scalars are rejected fail-loud. yaml.v3 decodes both `KEY:` and
// `KEY: null` as ScalarNode with Tag="!!null".
func TestValidateEnvironments_NullScalarRejected(t *testing.T) {
	nullNode := yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
	_, err := validateEnvironments(map[string]yaml.Node{"KEY": nullNode})
	if err == nil {
		t.Fatal("expected error for null scalar, got nil")
	}
	if !strings.Contains(err.Error(), "has no value") {
		t.Errorf("error %q does not contain 'has no value'", err.Error())
	}
	if !strings.HasPrefix(err.Error(), "projectconfig:") {
		t.Errorf("error missing 'projectconfig:' prefix: %q", err.Error())
	}
}

// An explicit empty string (KEY: "") is accepted and produces "KEY=".
func TestValidateEnvironments_ExplicitEmptyString(t *testing.T) {
	emptyNode := yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: ""}
	got, err := validateEnvironments(map[string]yaml.Node{"EMPTY_VAR": emptyNode})
	if err != nil {
		t.Fatalf("validateEnvironments error: %v", err)
	}
	if len(got) != 1 || got[0] != "EMPTY_VAR=" {
		t.Errorf("got %v, want [\"EMPTY_VAR=\"]", got)
	}
}

func TestValidateEnvironments_SortedOutput(t *testing.T) {
	nodes := map[string]yaml.Node{
		"Z_VAR": {Kind: yaml.ScalarNode, Tag: "!!str", Value: "z"},
		"A_VAR": {Kind: yaml.ScalarNode, Tag: "!!str", Value: "a"},
		"M_VAR": {Kind: yaml.ScalarNode, Tag: "!!str", Value: "m"},
	}
	got, err := validateEnvironments(nodes)
	if err != nil {
		t.Fatalf("validateEnvironments error: %v", err)
	}
	want := []string{"A_VAR=a", "M_VAR=m", "Z_VAR=z"}
	if !stringSlicesEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// An absent environments: block returns nil env (backward-compatible).
func TestLoad_AbsentEnvironments_NilEnv(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	content := `exclude:
  scan:
    patterns: ["*.env"]
    skip-dirs: [.git]
  files: []
  dirs: []
cache:
  content: true
  agent: true
`
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, _, envVars, err := Load(root)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if envVars != nil {
		t.Errorf("expected nil env for absent environments: block, got %v", envVars)
	}
}

func TestLoad_MissingFile_NilEnv(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	_, _, envVars, err := Load(root)
	if err != nil {
		t.Fatalf("Load on missing file returned error: %v", err)
	}
	if envVars != nil {
		t.Errorf("expected nil env for missing file, got %v", envVars)
	}
}

// Empty/whitespace-only files return nil env (io.EOF branch).
func TestLoad_EmptyAndWhitespaceFile_NilEnv(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")

	cases := []struct {
		name    string
		content []byte
	}{
		{"empty bytes", []byte{}},
		{"whitespace only", []byte("   \n   \n")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := evalSymlinks(t, t.TempDir())
			if err := os.WriteFile(filepath.Join(root, Filename), tc.content, 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, _, envVars, err := Load(root)
			if err != nil {
				t.Fatalf("Load returned error for %q: %v", tc.name, err)
			}
			if envVars != nil {
				t.Errorf("expected nil env for %q, got %v", tc.name, envVars)
			}
		})
	}
}

func TestLoad_EnvironmentsBlock_ReturnsSortedPairs(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	content := `environments:
  NODE_ENV: production
  PORT: 8080
  LOG_LEVEL: info
`
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, _, envVars, err := Load(root)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	want := []string{"LOG_LEVEL=info", "NODE_ENV=production", "PORT=8080"}
	if !stringSlicesEqual(envVars, want) {
		t.Errorf("got %v, want %v", envVars, want)
	}
}

// A typo in the top-level key (enviroments:) is an unknown field that strict
// mode must reject.
func TestLoad_TypoInEnvironments_StrictModeRejects(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	content := "enviroments:\n  NODE_ENV: production\n"
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, _, _, err := Load(root)
	if err == nil {
		t.Fatal("expected error for typo in top-level key, got nil")
	}
	if !strings.HasPrefix(err.Error(), "projectconfig:") {
		t.Errorf("error missing 'projectconfig:' prefix: %q", err.Error())
	}
}

// stringSlicesEqual compares element-wise; nil and empty [] are treated as equal.
func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
