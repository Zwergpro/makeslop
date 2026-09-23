package projectconfig

import (
	"os"
	"path/filepath"
	"reflect"
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
		// One environments row proves walker errors reach Load; the walker's
		// rules are covered by TestValidateEnvironments_Errors.
		{
			name:        "environments old flat form",
			yaml:        "environments:\n  NODE_ENV: production\n",
			wantErrFrag: "move entries under environments.static",
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
		// Path-separator patterns: security.Scan matches basenames only, so a
		// pattern with '/' can never match anything — fail-loud instead of
		// silently dropping all matches (finding #1).
		{
			name:        "path separator secrets/*.pem",
			yaml:        "exclude:\n  scan:\n    patterns:\n      - \"secrets/*.pem\"\n    skip-dirs: []\n  dirs: []\n  files: []\n",
			wantErrFrag: "contains a path separator",
		},
		{
			name:        "path separator **/*.env",
			yaml:        "exclude:\n  scan:\n    patterns:\n      - \"**/*.env\"\n    skip-dirs: []\n  dirs: []\n  files: []\n",
			wantErrFrag: "contains a path separator",
		},
		{
			name:        "path separator a/b",
			yaml:        "exclude:\n  scan:\n    patterns:\n      - \"a/b\"\n    skip-dirs: []\n  dirs: []\n  files: []\n",
			wantErrFrag: "contains a path separator",
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

// TestLoad_Scan_PathStylePattern_LoadLevel verifies that a .makeslop.yaml with a
// path-style pattern fails Load with a clear error (finding #1: path-style
// patterns can never match basenames, so they would silently lose masking).
func TestLoad_Scan_PathStylePattern_LoadLevel(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	content := "exclude:\n  scan:\n    patterns:\n      - \"secrets/*.pem\"\n    skip-dirs: []\n  dirs: []\n  files: []\n"
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, _, _, err := Load(root)
	if err == nil {
		t.Fatal("expected error for path-style scan pattern, got nil")
	}
	if !strings.Contains(err.Error(), "contains a path separator") {
		t.Errorf("error %q does not contain 'contains a path separator'", err.Error())
	}
	if !strings.Contains(err.Error(), "basenames only") {
		t.Errorf("error %q does not explain that patterns match basenames only", err.Error())
	}
	if !strings.HasPrefix(err.Error(), "projectconfig:") {
		t.Errorf("error missing 'projectconfig:' prefix: %q", err.Error())
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

// envNode unmarshals snippet and returns the root content node (not the
// DocumentNode), i.e. what yamlSchema.Environments receives. An empty snippet
// returns the zero node, matching an absent environments: key.
func envNode(t *testing.T, snippet string) *yaml.Node {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(snippet), &doc); err != nil {
		t.Fatalf("unmarshal %q: %v", snippet, err)
	}
	if len(doc.Content) == 0 {
		return &yaml.Node{}
	}
	return doc.Content[0]
}

// envFromDoc decodes a whole document the way Load does (environments: into a
// yaml.Node field), so aliases whose anchors sit outside the block survive
// unresolved. Unlike Load it is not strict: unknown anchor-holding keys are
// ignored here, while Load rejects them (see TestLoad_EnvironmentsAliases).
func envFromDoc(t *testing.T, doc string) *yaml.Node {
	t.Helper()
	var s struct {
		Environments yaml.Node `yaml:"environments"`
	}
	if err := yaml.Unmarshal([]byte(doc), &s); err != nil {
		t.Fatalf("unmarshal %q: %v", doc, err)
	}
	return &s.Environments
}

func TestValidateEnvironments_Success(t *testing.T) {
	cases := []struct {
		name    string
		snippet string
		want    Env
	}{
		{
			name:    "absent block",
			snippet: "",
			want:    Env{},
		},
		{
			name:    "null block",
			snippet: "~",
			want:    Env{},
		},
		{
			name:    "empty mapping",
			snippet: "{}",
			want:    Env{},
		},
		{
			// run sorts the merged pairs; the walker keeps file order.
			name:    "static only, file order",
			snippet: "static:\n  NODE_ENV: production\n  LOG_LEVEL: info\n  API_BASE_URL: https://api.example.com\n",
			want:    Env{Static: []string{"NODE_ENV=production", "LOG_LEVEL=info", "API_BASE_URL=https://api.example.com"}},
		},
		{
			name:    "host only",
			snippet: "host:\n  - TERM\n  - GITHUB_TOKEN\n",
			want:    Env{Host: []string{"GITHUB_TOKEN", "TERM"}},
		},
		{
			name:    "static and host",
			snippet: "static:\n  NODE_ENV: production\nhost:\n  - GITHUB_TOKEN\n",
			want:    Env{Static: []string{"NODE_ENV=production"}, Host: []string{"GITHUB_TOKEN"}},
		},
		{
			name:    "null sub-keys",
			snippet: "static:\nhost: ~\n",
			want:    Env{},
		},
		{
			name:    "empty sub-keys",
			snippet: "static: {}\nhost: []\n",
			want:    Env{},
		},
		{
			name:    "host dedupe and sort",
			snippet: "host: [Z_VAR, A_VAR, Z_VAR, M_VAR, A_VAR]\n",
			want:    Env{Host: []string{"A_VAR", "M_VAR", "Z_VAR"}},
		},
		{
			// node.Value is the raw scalar form, so numbers/booleans coerce.
			name:    "static scalar coercion",
			snippet: "static:\n  PORT: 8080\n  DEBUG: true\n  RATIO: 1.5\n",
			want:    Env{Static: []string{"PORT=8080", "DEBUG=true", "RATIO=1.5"}},
		},
		{
			// Same coercion for host entries; such names are simply unset on
			// most hosts and get skipped by run.
			name:    "host scalar coercion",
			snippet: "host:\n  - 123\n  - true\n  - 1.5\n",
			want:    Env{Host: []string{"1.5", "123", "true"}},
		},
		{
			name:    "static explicit empty string",
			snippet: "static:\n  EMPTY_VAR: \"\"\n",
			want:    Env{Static: []string{"EMPTY_VAR="}},
		},
		{
			name:    "alias as static value and host entry",
			snippet: "static:\n  A: &v X_NAME\n  B: *v\nhost:\n  - *v\n",
			want:    Env{Static: []string{"A=X_NAME", "B=X_NAME"}, Host: []string{"X_NAME"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateEnvironments(envNode(t, tc.snippet))
			if err != nil {
				t.Fatalf("validateEnvironments error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

// A yaml.Node decode target keeps aliases unresolved; the walker follows them
// for the block itself and for the static:/host: sub-keys.
func TestValidateEnvironments_AliasedBlocks(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want Env
	}{
		{
			name: "whole block aliased",
			doc:  "base: &e\n  static: {A: one}\n  host: [H]\nenvironments: *e\n",
			want: Env{Static: []string{"A=one"}, Host: []string{"H"}},
		},
		{
			name: "static and host aliased",
			doc:  "s: &s {A: one}\nl: &l [H]\nenvironments:\n  static: *s\n  host: *l\n",
			want: Env{Static: []string{"A=one"}, Host: []string{"H"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateEnvironments(envFromDoc(t, tc.doc))
			if err != nil {
				t.Fatalf("validateEnvironments error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %#v, want %#v", got, tc.want)
			}
		})
	}

	// An aliased scalar under an unknown key is still the old flat form.
	_, err := validateEnvironments(envFromDoc(t, "v: &v production\nenvironments:\n  NODE_ENV: *v\n"))
	const flatForm = `projectconfig: environments: flat "KEY: value" form is no longer supported; move entries under environments.static`
	if err == nil || err.Error() != flatForm {
		t.Errorf("aliased flat form: err = %v, want %q", err, flatForm)
	}
}

func TestValidateEnvironments_Errors(t *testing.T) {
	const (
		notMapping = `projectconfig: environments must be a mapping with optional "static" and "host" keys`
		flatForm   = `projectconfig: environments: flat "KEY: value" form is no longer supported; move entries under environments.static`
		staticMap  = "projectconfig: environments.static must be a mapping of KEY: value"
		hostList   = "projectconfig: environments.host must be a list of variable names"
		hint       = " (if this was the old flat form, move entries under environments.static)"
	)
	cases := []struct {
		name    string
		snippet string
		wantErr string
	}{
		{name: "environments as a list", snippet: "[A]", wantErr: notMapping},
		{name: "environments as a scalar", snippet: "FOO", wantErr: notMapping},
		{name: "old flat form", snippet: "NODE_ENV: production\n", wantErr: flatForm},
		{name: "old flat form with null value", snippet: "NODE_ENV:\n", wantErr: flatForm},
		{name: "old flat form, uppercase HOST variable", snippet: "HOST: db.local\n", wantErr: flatForm},
		{
			name:    "unknown key with non-scalar value",
			snippet: "hosts: [A]\n",
			wantErr: `projectconfig: unknown key "hosts" in environments (allowed: static, host)`,
		},
		{
			name:    "typo hosts with scalar value",
			snippet: "hosts: GITHUB_TOKEN\n",
			wantErr: `projectconfig: unknown key "hosts" in environments (allowed: static, host)`,
		},
		{
			name:    "typo bare hosts",
			snippet: "hosts:\n",
			wantErr: `projectconfig: unknown key "hosts" in environments (allowed: static, host)`,
		},
		{
			name:    "typo capitalised Host",
			snippet: "Host: X\n",
			wantErr: `projectconfig: unknown key "Host" in environments (allowed: static, host)`,
		},
		{
			name:    "typo Statics",
			snippet: "Statics: X\n",
			wantErr: `projectconfig: unknown key "Statics" in environments (allowed: static, host)`,
		},
		{
			name:    "merge key at top level",
			snippet: "<<: {static: {A: b}}\n",
			wantErr: "projectconfig: environments: merge keys (<<) are not supported",
		},
		{
			name:    "merge key inside static",
			snippet: "static:\n  <<: {A: b}\n",
			wantErr: "projectconfig: environments.static: merge keys (<<) are not supported",
		},
		{name: "scalar under static", snippet: "static: FOO\n", wantErr: staticMap + hint},
		{name: "sequence under static", snippet: "static: [A]\n", wantErr: staticMap},
		{name: "host as scalar", snippet: "host: GITHUB_TOKEN\n", wantErr: hostList + hint},
		{name: "old flat form variable named host", snippet: "host: db.local\n", wantErr: hostList + hint},
		{name: "host as mapping", snippet: "host:\n  GITHUB_TOKEN: x\n", wantErr: hostList},
		{
			name:    "duplicate static block",
			snippet: "static:\n  A: a\nstatic:\n  B: b\n",
			wantErr: `projectconfig: duplicate key "static" in environments`,
		},
		{
			name:    "duplicate host block",
			snippet: "host: [A]\nhost: [B]\n",
			wantErr: `projectconfig: duplicate key "host" in environments`,
		},
		{
			name:    "duplicate key inside static",
			snippet: "static:\n  A: a\n  A: b\n",
			wantErr: `projectconfig: duplicate key "A" in environments.static`,
		},
		{
			name:    "null key at top level",
			snippet: "~: x\n",
			wantErr: "projectconfig: environments: key at line 1 must be a non-null scalar",
		},
		{
			name:    "complex key at top level",
			snippet: "? [a]\n: x\n",
			wantErr: "projectconfig: environments: key at line 1 must be a non-null scalar",
		},
		{
			name:    "null key inside static",
			snippet: "static:\n  ~: x\n",
			wantErr: "projectconfig: environments.static: key at line 2 must be a non-null scalar",
		},
		{
			name:    "complex key inside static",
			snippet: "static:\n  ? [a]\n  : x\n",
			wantErr: "projectconfig: environments.static: key at line 2 must be a non-null scalar",
		},
		{
			name:    "static key with equals sign",
			snippet: "static:\n  \"A=B\": val\n",
			wantErr: "projectconfig: environments.static: key at line 2 must not contain '='",
		},
		{
			name:    "static empty key",
			snippet: "static:\n  \"\": val\n",
			wantErr: "projectconfig: empty key in environments.static",
		},
		{
			name:    "static key with LF",
			snippet: "static:\n  \"KEY\\nNAME\": val\n",
			wantErr: "projectconfig: environments.static: key at line 2 must not contain newline, carriage-return, or tab characters",
		},
		{
			name:    "static key with CR",
			snippet: "static:\n  \"KEY\\rNAME\": val\n",
			wantErr: "projectconfig: environments.static: key at line 2 must not contain newline, carriage-return, or tab characters",
		},
		{
			name:    "static key with TAB",
			snippet: "static:\n  \"KEY\\tNAME\": val\n",
			wantErr: "projectconfig: environments.static: key at line 2 must not contain newline, carriage-return, or tab characters",
		},
		{
			name:    "static value with LF",
			snippet: "static:\n  KEY: \"line1\\nline2\"\n",
			wantErr: `projectconfig: environments.static: key "KEY" value must not contain newline, carriage-return, or tab characters`,
		},
		{
			name:    "static value with CR",
			snippet: "static:\n  KEY: \"line1\\rline2\"\n",
			wantErr: `projectconfig: environments.static: key "KEY" value must not contain newline, carriage-return, or tab characters`,
		},
		{
			name:    "static value with TAB",
			snippet: "static:\n  KEY: \"val1\\tval2\"\n",
			wantErr: `projectconfig: environments.static: key "KEY" value must not contain newline, carriage-return, or tab characters`,
		},
		{
			name:    "static sequence value",
			snippet: "static:\n  FOO: [a, b]\n",
			wantErr: `projectconfig: environments.static: key "FOO" must be a scalar value`,
		},
		{
			name:    "static mapping value",
			snippet: "static:\n  FOO: {a: b}\n",
			wantErr: `projectconfig: environments.static: key "FOO" must be a scalar value`,
		},
		{
			name:    "static null value",
			snippet: "static:\n  KEY: null\n",
			wantErr: `projectconfig: environments.static: key "KEY" has no value`,
		},
		{
			name:    "static bare value",
			snippet: "static:\n  KEY:\n",
			wantErr: `projectconfig: environments.static: key "KEY" has no value`,
		},
		{
			name:    "host null entry",
			snippet: "host:\n  - ~\n",
			wantErr: "projectconfig: environments.host entry at line 2 has no name",
		},
		{
			name:    "host bare dash entry",
			snippet: "host:\n  -\n",
			wantErr: "projectconfig: environments.host entry at line 2 has no name",
		},
		{
			name:    "host empty name",
			snippet: "host:\n  - \"\"\n",
			wantErr: "projectconfig: environments.host entry at line 2 has no name",
		},
		{
			name:    "host name with equals sign",
			snippet: "host:\n  - A=B\n",
			wantErr: "projectconfig: environments.host entry at line 2 must not contain '='",
		},
		{
			name:    "host name with space",
			snippet: "host:\n  - \"A B\"\n",
			wantErr: "projectconfig: environments.host entry at line 2 must not contain whitespace",
		},
		{
			name:    "host name with tab",
			snippet: "host:\n  - \"A\\tB\"\n",
			wantErr: "projectconfig: environments.host entry at line 2 must not contain whitespace",
		},
		{
			// unicode.IsSpace covers non-ASCII whitespace such as NO-BREAK SPACE.
			name:    "host name with U+00A0",
			snippet: "host:\n  - \"A B\"\n",
			wantErr: "projectconfig: environments.host entry at line 2 must not contain whitespace",
		},
		{
			name:    "host non-scalar entry",
			snippet: "host:\n  - [A]\n",
			wantErr: "projectconfig: environments.host entry at line 2 must be a variable name",
		},
		{
			name:    "static/host overlap",
			snippet: "static:\n  GITHUB_TOKEN: x\nhost:\n  - GITHUB_TOKEN\n",
			wantErr: `projectconfig: environment key "GITHUB_TOKEN" listed in both environments.static and environments.host`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateEnvironments(envNode(t, tc.snippet))
			if err == nil {
				t.Fatalf("expected error %q, got nil (env %#v)", tc.wantErr, got)
			}
			if err.Error() != tc.wantErr {
				t.Errorf("error = %q, want %q", err.Error(), tc.wantErr)
			}
			if !reflect.DeepEqual(got, Env{}) {
				t.Errorf("expected zero Env on error, got %#v", got)
			}
		})
	}
}

// Error messages name keys but never echo values (values may be secrets),
// including a KEY=value pasted where a name belongs.
func TestValidateEnvironments_ErrorsOmitValues(t *testing.T) {
	snippets := []string{
		"static:\n  KEY: \"s3cr3t\\nvalue\"\n",
		"SECRET_KEY: s3cr3t\n",
		"static:\n  \"KEY=s3cr3t\": x\n",
		"host:\n  - GITHUB_TOKEN=s3cr3t\n",
		"host:\n  - GITHUB_TOKEN s3cr3t\n",
	}
	for _, s := range snippets {
		_, err := validateEnvironments(envNode(t, s))
		if err == nil {
			t.Fatalf("expected error for %q", s)
		}
		if strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("error leaks value: %q", err.Error())
		}
	}
}

// An absent environments: block returns a zero Env.
func TestLoad_AbsentEnvironments_ZeroEnv(t *testing.T) {
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

	_, _, env, err := Load(root)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if !reflect.DeepEqual(env, Env{}) {
		t.Errorf("expected zero Env for absent environments: block, got %#v", env)
	}
}

// Empty/whitespace-only files and an empty environments: block return a zero Env.
func TestLoad_EmptyAndWhitespaceFile_ZeroEnv(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")

	cases := []struct {
		name    string
		content []byte
	}{
		{"empty bytes", []byte{}},
		{"whitespace only", []byte("   \n   \n")},
		{"empty environments block", []byte("environments:\n")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := evalSymlinks(t, t.TempDir())
			if err := os.WriteFile(filepath.Join(root, Filename), tc.content, 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, _, env, err := Load(root)
			if err != nil {
				t.Fatalf("Load returned error for %q: %v", tc.name, err)
			}
			if !reflect.DeepEqual(env, Env{}) {
				t.Errorf("expected zero Env for %q, got %#v", tc.name, env)
			}
		})
	}
}

func TestLoad_EnvironmentsBlock_ReturnsPairs(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	content := `environments:
  static:
    NODE_ENV: &env production
    PORT: 8080
    LOG_LEVEL: info
    FALLBACK_ENV: *env
  host:
    - TERM
    - GITHUB_TOKEN
`
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, _, env, err := Load(root)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	// Static keeps file order (run sorts); the alias resolves through Load.
	want := Env{
		Static: []string{"NODE_ENV=production", "PORT=8080", "LOG_LEVEL=info", "FALLBACK_ENV=production"},
		Host:   []string{"GITHUB_TOKEN", "TERM"},
	}
	if !reflect.DeepEqual(env, want) {
		t.Errorf("got %#v, want %#v", env, want)
	}
}

// Alias forms reachable from a real file: an anchor on a static value reused by
// a host entry. A top-level key added only to hold an anchor is rejected by
// strict decoding before the walker runs.
func TestLoad_EnvironmentsAliases(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")

	t.Run("static value reused by host entry", func(t *testing.T) {
		root := evalSymlinks(t, t.TempDir())
		content := "environments:\n  static:\n    TOKEN_VAR: &tok GITHUB_TOKEN\n  host:\n    - *tok\n"
		if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, _, env, err := Load(root)
		if err != nil {
			t.Fatalf("Load returned error: %v", err)
		}
		want := Env{Static: []string{"TOKEN_VAR=GITHUB_TOKEN"}, Host: []string{"GITHUB_TOKEN"}}
		if !reflect.DeepEqual(env, want) {
			t.Errorf("got %#v, want %#v", env, want)
		}
	})

	t.Run("top-level anchor holder rejected", func(t *testing.T) {
		root := evalSymlinks(t, t.TempDir())
		content := "base: &e\n  static: {A: one}\nenvironments: *e\n"
		if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, _, _, err := Load(root)
		if err == nil || !strings.Contains(err.Error(), "field base not found") {
			t.Errorf("err = %v, want strict-decode unknown field \"base\"", err)
		}
	})
}

// yaml.v3 skips its duplicate-key check for yaml.Node targets; the walker's own
// check must still fire through Load, at both levels.
func TestLoad_EnvironmentsDuplicateKeys(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")

	cases := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name:    "duplicate static block",
			content: "environments:\n  static:\n    A: a\n  static:\n    B: b\n",
			wantErr: `projectconfig: duplicate key "static" in environments`,
		},
		{
			name:    "duplicate key inside static",
			content: "environments:\n  static:\n    A: a\n    A: b\n",
			wantErr: `projectconfig: duplicate key "A" in environments.static`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := evalSymlinks(t, t.TempDir())
			if err := os.WriteFile(filepath.Join(root, Filename), []byte(tc.content), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, _, _, err := Load(root)
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
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

// TestScaffold_DanglingSymlink verifies that Scaffold rejects a dangling symlink
// at the .makeslop.yaml path with a hard error (finding #2).
func TestScaffold_DanglingSymlink(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	path := filepath.Join(root, Filename)
	// Create a dangling symlink: target does not exist.
	if err := os.Symlink(filepath.Join(root, "nonexistent-target"), path); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	err := Scaffold(root, Cache{Content: true, Agent: true})
	if err == nil {
		t.Fatal("expected error for dangling symlink at config path, got nil")
	}
	if !strings.Contains(err.Error(), "is a symlink") {
		t.Errorf("error %q does not contain 'is a symlink'", err.Error())
	}
	if !strings.Contains(err.Error(), "must be a regular file") {
		t.Errorf("error %q does not contain 'must be a regular file'", err.Error())
	}
	if !strings.HasPrefix(err.Error(), "projectconfig:") {
		t.Errorf("error missing 'projectconfig:' prefix: %q", err.Error())
	}
}

// TestScaffold_LiveSymlink verifies that Scaffold rejects a live symlink pointing
// to a valid config file (finding #2).
func TestScaffold_LiveSymlink(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	// Create a real config file elsewhere.
	realConfig := filepath.Join(root, "real-config.yaml")
	if err := os.WriteFile(realConfig, Stub, 0o644); err != nil {
		t.Fatalf("write real config: %v", err)
	}

	path := filepath.Join(root, Filename)
	if err := os.Symlink(realConfig, path); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	err := Scaffold(root, Cache{Content: true, Agent: true})
	if err == nil {
		t.Fatal("expected error for live symlink at config path, got nil")
	}
	if !strings.Contains(err.Error(), "is a symlink") {
		t.Errorf("error %q does not contain 'is a symlink'", err.Error())
	}
	if !strings.Contains(err.Error(), "must be a regular file") {
		t.Errorf("error %q does not contain 'must be a regular file'", err.Error())
	}
	if !strings.HasPrefix(err.Error(), "projectconfig:") {
		t.Errorf("error missing 'projectconfig:' prefix: %q", err.Error())
	}
}

// TestScaffold_RegularFile_Idempotent confirms that EEXIST on a regular file
// (not a symlink) still returns nil — idempotency is preserved (finding #2:
// only symlinks are rejected, regular-file EEXIST stays success).
func TestScaffold_RegularFile_Idempotent_SymlinkCheck(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	// Pre-create the file.
	userContent := []byte("# my config\nexclude:\n  dirs: []\n  files: []\n")
	if err := os.WriteFile(filepath.Join(root, Filename), userContent, 0o644); err != nil {
		t.Fatalf("pre-write: %v", err)
	}

	if err := Scaffold(root, Cache{Content: true, Agent: true}); err != nil {
		t.Fatalf("Scaffold on existing regular file returned error: %v", err)
	}

	// Content must not be modified.
	got, err := os.ReadFile(filepath.Join(root, Filename))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(userContent) {
		t.Errorf("user content was modified:\ngot:  %q\nwant: %q", got, userContent)
	}
}

// TestLoad_DanglingSymlink verifies that Load rejects a dangling symlink at
// the .makeslop.yaml path with a hard error instead of silently returning empty
// defaults (finding #2).
func TestLoad_DanglingSymlink(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	path := filepath.Join(root, Filename)
	if err := os.Symlink(filepath.Join(root, "nowhere"), path); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, _, _, err := Load(root)
	if err == nil {
		t.Fatal("expected error for dangling symlink, got nil (silently treats as missing — wrong)")
	}
	if !strings.Contains(err.Error(), "is a symlink") {
		t.Errorf("error %q does not contain 'is a symlink'", err.Error())
	}
	if !strings.Contains(err.Error(), "must be a regular file") {
		t.Errorf("error %q does not contain 'must be a regular file'", err.Error())
	}
	if !strings.HasPrefix(err.Error(), "projectconfig:") {
		t.Errorf("error missing 'projectconfig:' prefix: %q", err.Error())
	}
}

// TestLoad_LiveSymlink verifies that Load rejects a live symlink pointing to a
// valid config file (finding #2).
func TestLoad_LiveSymlink(t *testing.T) {
	skipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	// Create a real valid config elsewhere.
	realConfig := filepath.Join(root, "real-config.yaml")
	if err := os.WriteFile(realConfig, Stub, 0o644); err != nil {
		t.Fatalf("write real config: %v", err)
	}

	path := filepath.Join(root, Filename)
	if err := os.Symlink(realConfig, path); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, _, _, err := Load(root)
	if err == nil {
		t.Fatal("expected error for live symlink to valid config, got nil")
	}
	if !strings.Contains(err.Error(), "is a symlink") {
		t.Errorf("error %q does not contain 'is a symlink'", err.Error())
	}
	if !strings.Contains(err.Error(), "must be a regular file") {
		t.Errorf("error %q does not contain 'must be a regular file'", err.Error())
	}
	if !strings.HasPrefix(err.Error(), "projectconfig:") {
		t.Errorf("error missing 'projectconfig:' prefix: %q", err.Error())
	}
}

// TestLoad_MissingFile_ReturnsDefaultsNotError confirms that a truly absent
// .makeslop.yaml (no symlink, no file) still returns empty defaults — regression
// guard for the Lstat-before-ReadFile change (finding #2).
func TestLoad_MissingFile_NoSymlink_ReturnsDefaults(t *testing.T) {
	root := evalSymlinks(t, t.TempDir())

	_, cacheCfg, env, err := Load(root)
	if err != nil {
		t.Fatalf("Load on truly missing file returned error: %v", err)
	}
	if !cacheCfg.Content || !cacheCfg.Agent {
		t.Errorf("Cache defaults wrong: got {Content:%v Agent:%v}, want {true, true}", cacheCfg.Content, cacheCfg.Agent)
	}
	if !reflect.DeepEqual(env, Env{}) {
		t.Errorf("expected zero Env for missing file, got %#v", env)
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
