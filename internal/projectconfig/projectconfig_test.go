package projectconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Zwergpro/makeslop/internal/docker"
)

// evalSymlinks resolves symlinks for a temp-dir path, matching the precondition
// documented on Load (and workspace.Lookup, security.Scan). On macOS-style
// hosts /tmp is itself a symlink, so raw t.TempDir() paths violate the
// precondition.
func evalSymlinks(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("eval symlinks %s: %v", dir, err)
	}
	return resolved
}

// TestScaffold_WritesStub verifies that Scaffold creates .makeslop.yaml with
// the exact byte-stable stub when no file exists.
func TestScaffold_WritesStub(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	if err := Scaffold(root); err != nil {
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

// TestScaffold_Idempotent verifies that Scaffold does not modify an existing
// .makeslop.yaml with arbitrary user content.
func TestScaffold_Idempotent(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	userContent := []byte("# my custom config\nexclude:\n  dirs:\n    - secrets\n  files: []\n")
	if err := os.WriteFile(filepath.Join(root, Filename), userContent, 0o644); err != nil {
		t.Fatalf("pre-write: %v", err)
	}

	if err := Scaffold(root); err != nil {
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

// TestLoad_MissingFile verifies that Load returns a zero Excludes (no error)
// when the config file is absent.
func TestLoad_MissingFile(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	excl, _, err := Load(root)
	if err != nil {
		t.Fatalf("Load on missing file: %v", err)
	}
	if len(excl.Files) != 0 || len(excl.Dirs) != 0 {
		t.Errorf("expected zero Excludes, got %+v", excl)
	}
}

// TestLoad_EmptyStub verifies that Load returns a zero Excludes when the file
// contains the scaffolded empty-list stub.
func TestLoad_EmptyStub(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	if err := os.WriteFile(filepath.Join(root, Filename), Stub, 0o644); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	excl, _, err := Load(root)
	if err != nil {
		t.Fatalf("Load on stub: %v", err)
	}
	if len(excl.Files) != 0 || len(excl.Dirs) != 0 {
		t.Errorf("expected zero Excludes from empty stub, got %+v", excl)
	}
}

// TestLoad_EmptyAndCommentOnlyFiles verifies that Load returns a zero Excludes
// (no error) for empty bytes, whitespace-only, and comment-only YAML files.
// yaml.NewDecoder returns io.EOF for these cases; Load must treat it as zero config.
func TestLoad_EmptyAndCommentOnlyFiles(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")

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
			excl, _, err := Load(root)
			if err != nil {
				t.Fatalf("Load returned error for %q: %v", tc.name, err)
			}
			if len(excl.Files) != 0 || len(excl.Dirs) != 0 {
				t.Errorf("expected zero Excludes for %q, got %+v", tc.name, excl)
			}
		})
	}
}

// TestLoad_MalformedYAML verifies that Load returns a "projectconfig:"-prefixed
// error when the file contains invalid YAML.
func TestLoad_MalformedYAML(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	if err := os.WriteFile(filepath.Join(root, Filename), []byte(":\tnot valid yaml{{{\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, _, err := Load(root)
	if err == nil {
		t.Fatal("expected error for malformed YAML, got nil")
	}
	if !strings.HasPrefix(err.Error(), "projectconfig:") {
		t.Errorf("error does not have 'projectconfig:' prefix: %q", err.Error())
	}
}

// TestLoad_UnknownField verifies that KnownFields(true) causes an error on
// unrecognized top-level keys (e.g. "include:").
func TestLoad_UnknownField(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	if err := os.WriteFile(filepath.Join(root, Filename), []byte("include:\n  files: []\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, _, err := Load(root)
	if err == nil {
		t.Fatal("expected error for unknown field, got nil")
	}
	if !strings.HasPrefix(err.Error(), "projectconfig:") {
		t.Errorf("error does not have 'projectconfig:' prefix: %q", err.Error())
	}
}

// TestLoad_ValidationRules verifies individual path validation rules via
// subtests.
func TestLoad_ValidationRules(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")

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
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := evalSymlinks(t, t.TempDir())
			if err := os.WriteFile(filepath.Join(root, Filename), []byte(tc.yaml), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, _, err := Load(root)
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

// TestLoad_ReservedPaths verifies that each of the four reserved agent paths
// is rejected with a "collides with a reserved agent path" error, for both
// exclude.dirs and exclude.files.
func TestLoad_ReservedPaths(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")

	for _, reserved := range []string{".claude", ".codex", "docs", "CLAUDE.md"} {
		t.Run("dirs/"+reserved, func(t *testing.T) {
			root := evalSymlinks(t, t.TempDir())
			content := "exclude:\n  dirs:\n    - " + reserved + "\n  files: []\n"
			if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, _, err := Load(root)
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
			_, _, err := Load(root)
			if err == nil {
				t.Fatalf("expected collision error for %q in files, got nil", reserved)
			}
			if !strings.Contains(err.Error(), "reserved agent path") {
				t.Errorf("error %q does not mention 'reserved agent path'", err.Error())
			}
		})
	}
}

// TestLoad_CrossListDuplicate verifies that listing the same path in both
// exclude.files and exclude.dirs is a parse-time error, regardless of on-disk
// state.
func TestLoad_CrossListDuplicate(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	content := "exclude:\n  files:\n    - mydir/secret\n  dirs:\n    - mydir/secret\n"
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, _, err := Load(root)
	if err == nil {
		t.Fatal("expected error for cross-list duplicate, got nil")
	}
	if !strings.Contains(err.Error(), "listed in both") {
		t.Errorf("error %q does not contain 'listed in both'", err.Error())
	}
}

// TestLoad_CrossListDuplicate_NoFileOnDisk verifies the cross-list duplicate
// check fires even when the path does NOT exist on disk (deterministic error).
func TestLoad_CrossListDuplicate_NoFileOnDisk(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	// "ghost" does not exist on disk — but the error must still fire.
	content := "exclude:\n  files:\n    - ghost\n  dirs:\n    - ghost\n"
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, _, err := Load(root)
	if err == nil {
		t.Fatal("expected error for cross-list duplicate (path absent), got nil")
	}
	if !strings.Contains(err.Error(), "listed in both") {
		t.Errorf("error %q does not contain 'listed in both'", err.Error())
	}
}

// TestLoad_SilentlyDropsMissingEntries verifies that entries whose path does
// not exist on disk are silently skipped (both files and dirs).
func TestLoad_SilentlyDropsMissingEntries(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	content := "exclude:\n  files:\n    - nonexistent/api.key\n  dirs:\n    - phantom-dir\n"
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	excl, _, err := Load(root)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(excl.Files) != 0 || len(excl.Dirs) != 0 {
		t.Errorf("expected empty result, got %+v", excl)
	}
}

// TestLoad_DropsWrongType verifies that a dirs entry that is actually a file
// is dropped, and a files entry that is actually a directory is dropped.
func TestLoad_DropsWrongType(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	// Create a regular file at "am-a-file" and a directory at "am-a-dir".
	if err := os.WriteFile(filepath.Join(root, "am-a-file"), []byte("data"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "am-a-dir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// am-a-file listed under dirs → should be dropped.
	// am-a-dir listed under files → should be dropped.
	content := "exclude:\n  dirs:\n    - am-a-file\n  files:\n    - am-a-dir\n"
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	excl, _, err := Load(root)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(excl.Files) != 0 || len(excl.Dirs) != 0 {
		t.Errorf("expected empty result (wrong-type drops), got %+v", excl)
	}
}

// TestLoad_DropsSymlinks verifies that symlinks (to regular file and to dir)
// are silently dropped from both exclude.files and exclude.dirs.
func TestLoad_DropsSymlinks(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks and /‐paths required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	// Create real targets.
	realFile := filepath.Join(root, "real-file")
	realDir := filepath.Join(root, "real-dir")
	if err := os.WriteFile(realFile, []byte("data"), 0o644); err != nil {
		t.Fatalf("write real file: %v", err)
	}
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatalf("mkdir real dir: %v", err)
	}

	// Create symlinks pointing to real targets.
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

	excl, _, err := Load(root)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(excl.Files) != 0 || len(excl.Dirs) != 0 {
		t.Errorf("expected symlinks to be dropped, got %+v", excl)
	}
}

// TODO(testing): statFilter non-ErrNotExist stat error path (e.g. chmod 000 on
// parent directory) is not tested. Such a test requires chmod on a directory,
// which is fragile on CI systems running as root or under certain container
// configurations. Skip for now; the error path in statFilter is exercised by
// careful code review.

// TestLoad_DeduplicatesWithinLists verifies that the same path listed twice
// within files (or dirs) results in exactly one entry.
func TestLoad_DeduplicatesWithinLists(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	// Create actual files/dirs on disk so they pass the stat filter.
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

	excl, _, err := Load(root)
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

// TestLoad_ReturnsAbsoluteSortedPaths verifies that Load returns absolute paths
// joined under root, and that multiple entries are lexicographically sorted.
func TestLoad_ReturnsAbsoluteSortedPaths(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	// Create real files/dirs — names chosen so z comes before a alphabetically
	// ONLY if sorting is wrong. After sort: a < z.
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

	// Deliberately list in z-first order to verify sorting.
	content := "exclude:\n  files:\n    - z-secret.txt\n    - a-secret.txt\n  dirs:\n    - z-priv\n    - a-priv\n"
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	excl, _, err := Load(root)
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

// TestLoad_Network_ValidAddress verifies that a well-formed host:port address in
// network.proxy.address is parsed into Network.ProxyAddress.
func TestLoad_Network_ValidAddress(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")

	cases := []struct {
		name    string
		address string
	}{
		{"hostname:port", "proxy.example.com:8888"},
		{"IPv4:port", "10.0.0.5:3128"},
		{"localhost:port", "localhost:8080"},
		{"IP with high port", "192.168.1.100:65535"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := evalSymlinks(t, t.TempDir())
			content := "exclude:\n  dirs: []\n  files: []\nnetwork:\n  proxy:\n    address: " + tc.address + "\n"
			if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}

			_, netCfg, err := Load(root)
			if err != nil {
				t.Fatalf("Load returned error: %v", err)
			}
			if netCfg.ProxyAddress != tc.address {
				t.Errorf("ProxyAddress: got %q, want %q", netCfg.ProxyAddress, tc.address)
			}
		})
	}
}

// TestLoad_Network_AbsentSection verifies that Load returns a zero Network when
// the network: section is absent from the YAML.
func TestLoad_Network_AbsentSection(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	// Use the standard stub which has no network: section.
	if err := os.WriteFile(filepath.Join(root, Filename), Stub, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, netCfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if netCfg.ProxyAddress != "" {
		t.Errorf("expected empty ProxyAddress for absent section, got %q", netCfg.ProxyAddress)
	}
}

// TestLoad_Network_MissingFile verifies that Load returns a zero Network (not
// an error) when the config file is absent.
func TestLoad_Network_MissingFile(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	_, netCfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load on missing file: %v", err)
	}
	if netCfg.ProxyAddress != "" {
		t.Errorf("expected empty ProxyAddress for missing file, got %q", netCfg.ProxyAddress)
	}
}

// TestLoad_Network_InvalidAddress verifies that a malformed network.proxy.address
// causes a "projectconfig:"-prefixed error.
func TestLoad_Network_InvalidAddress(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")

	cases := []struct {
		name    string
		address string
		// quoted controls whether the YAML value is written as a double-quoted
		// string. Use true for addresses containing YAML-special characters (e.g.
		// bare ":" is a mapping-value indicator and makes the YAML unparseable
		// without quoting).
		quoted bool
	}{
		{"missing port", "proxy.example.com", false},
		{"empty host", ":8888", true},
		{"not a url", "not a url", false},
		{"only colon", ":", true},
		{"empty string triggers no proxy", "", false}, // empty address => zero Network, no error
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := evalSymlinks(t, t.TempDir())
			var content string
			if tc.address == "" {
				// Omit the address field entirely to test the "empty address ⇒ zero Network" case.
				content = "exclude:\n  dirs: []\n  files: []\nnetwork:\n  proxy:\n    address: \"\"\n"
			} else if tc.quoted {
				content = "exclude:\n  dirs: []\n  files: []\nnetwork:\n  proxy:\n    address: \"" + tc.address + "\"\n"
			} else {
				content = "exclude:\n  dirs: []\n  files: []\nnetwork:\n  proxy:\n    address: " + tc.address + "\n"
			}
			if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}

			_, netCfg, err := Load(root)
			if tc.address == "" {
				// Empty address field → no error, zero Network.
				if err != nil {
					t.Fatalf("unexpected error for empty address: %v", err)
				}
				if netCfg.ProxyAddress != "" {
					t.Errorf("expected empty ProxyAddress, got %q", netCfg.ProxyAddress)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error for address %q, got nil", tc.address)
			}
			if !strings.HasPrefix(err.Error(), "projectconfig:") {
				t.Errorf("error missing 'projectconfig:' prefix: %q", err.Error())
			}
			if !strings.Contains(err.Error(), "invalid network.proxy.address") {
				t.Errorf("error does not mention 'invalid network.proxy.address': %q", err.Error())
			}
		})
	}
}

// TestLoad_Network_ExcludesUnchanged verifies that the presence of a network:
// section does not affect the Excludes parsing behavior.
func TestLoad_Network_ExcludesUnchanged(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	// Create real files/dirs so they pass the stat filter.
	if err := os.WriteFile(filepath.Join(root, "secret.env"), []byte("s"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "private"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	content := "exclude:\n  files:\n    - secret.env\n  dirs:\n    - private\nnetwork:\n  proxy:\n    address: 10.0.0.1:8888\n"
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	excl, netCfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	// Verify excludes still work correctly.
	if len(excl.Files) != 1 || excl.Files[0] != filepath.Join(root, "secret.env") {
		t.Errorf("unexpected Files: %v", excl.Files)
	}
	if len(excl.Dirs) != 1 || excl.Dirs[0] != filepath.Join(root, "private") {
		t.Errorf("unexpected Dirs: %v", excl.Dirs)
	}

	// Verify network config is also parsed.
	if netCfg.ProxyAddress != "10.0.0.1:8888" {
		t.Errorf("ProxyAddress: got %q, want %q", netCfg.ProxyAddress, "10.0.0.1:8888")
	}
}
