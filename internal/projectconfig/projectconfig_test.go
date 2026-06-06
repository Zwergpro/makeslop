package projectconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Zwergpro/makeslop/internal/docker"
)

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

func TestLoad_MissingFile(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
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
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	if err := os.WriteFile(filepath.Join(root, Filename), Stub, 0o644); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	excl, netCfg, _, err := Load(root)
	if err != nil {
		t.Fatalf("Load on default stub: %v", err)
	}
	// explicit path lists remain empty
	if len(excl.Files) != 0 || len(excl.Dirs) != 0 {
		t.Errorf("expected zero Files/Dirs from default stub, got files=%v dirs=%v", excl.Files, excl.Dirs)
	}
	// network proxy address is empty
	if netCfg.ProxyAddress != "" {
		t.Errorf("expected empty ProxyAddress, got %q", netCfg.ProxyAddress)
	}
	// default scan patterns are seeded — these must mirror Stub exactly (sorted).
	// If you change Stub, update this list to match.
	wantPatterns := []string{
		"*.env",
		"*.key",
		"*.pem",
		".env.*",
		".git-credentials",
		".netrc",
		".npmrc",
		"id_ed25519*",
		"id_rsa*",
	}
	if !stringSlicesEqual(excl.Patterns, wantPatterns) {
		t.Errorf("Patterns: got %v, want %v\n(if Stub changed, update wantPatterns to match)", excl.Patterns, wantPatterns)
	}
	// default skip-dirs are seeded — these must mirror Stub exactly (sorted).
	// If you change Stub, update this list to match.
	wantSkipDirs := []string{".git", ".venv", "node_modules", "vendor"}
	if !stringSlicesEqual(excl.SkipDirs, wantSkipDirs) {
		t.Errorf("SkipDirs: got %v, want %v\n(if Stub changed, update wantSkipDirs to match)", excl.SkipDirs, wantSkipDirs)
	}
}

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
			excl, _, _, err := Load(root)
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
		})
	}
}

func TestLoad_MalformedYAML(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
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
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
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
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")

	for _, reserved := range []string{".claude", ".codex", "docs", "CLAUDE.md"} {
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
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
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

	_, _, _, err := Load(root)
	if err == nil {
		t.Fatal("expected error for cross-list duplicate (path absent), got nil")
	}
	if !strings.Contains(err.Error(), "listed in both") {
		t.Errorf("error %q does not contain 'listed in both'", err.Error())
	}
}

func TestLoad_SilentlyDropsMissingEntries(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
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
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

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

	excl, _, _, err := Load(root)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(excl.Files) != 0 || len(excl.Dirs) != 0 {
		t.Errorf("expected empty result (wrong-type drops), got %+v", excl)
	}
}

func TestLoad_DropsSymlinks(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks and /‐paths required; POSIX-only per CLAUDE.md")
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
		t.Errorf("expected symlinks to be dropped, got %+v", excl)
	}
}

// TODO(testing): statFilter non-ErrNotExist stat error path (e.g. chmod 000 on
// parent directory) is not tested. Such a test requires chmod on a directory,
// which is fragile on CI systems running as root or under certain container
// configurations. Skip for now; the error path in statFilter is exercised by
// careful code review.

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

			_, netCfg, _, err := Load(root)
			if err != nil {
				t.Fatalf("Load returned error: %v", err)
			}
			if netCfg.ProxyAddress != tc.address {
				t.Errorf("ProxyAddress: got %q, want %q", netCfg.ProxyAddress, tc.address)
			}
		})
	}
}

func TestLoad_Network_AbsentSection(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	if err := os.WriteFile(filepath.Join(root, Filename), Stub, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, netCfg, _, err := Load(root)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if netCfg.ProxyAddress != "" {
		t.Errorf("expected empty ProxyAddress for absent section, got %q", netCfg.ProxyAddress)
	}
}

func TestLoad_Network_MissingFile(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	_, netCfg, _, err := Load(root)
	if err != nil {
		t.Fatalf("Load on missing file: %v", err)
	}
	if netCfg.ProxyAddress != "" {
		t.Errorf("expected empty ProxyAddress for missing file, got %q", netCfg.ProxyAddress)
	}
}

// TestLoad_Network_InvalidAddress verifies that Load does NOT validate
// network.proxy.address syntax — it stores the raw string regardless. Proxy
// address validation is deferred to runRun in main.go, after flag overrides
// (--proxy wins over network.proxy.address) have been resolved. This allows a
// run with a valid --proxy flag to succeed even when the config file has a
// malformed address.
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
		{"socat delimiter in port", "10.0.0.1:3128,foo", true},  // port must be a plain integer
		{"comma in host", "proxy,forever:3128", true},            // comma is socat option delimiter
		{"negative port", "host:-1", true},                       // port out of range
		{"port above max", "host:99999", true},                   // port > 65535
		{"port zero", "host:0", true},                            // port 0 is not a valid TCP port
		{"tab in host", "host\t.evil:3128", true},                // tab not in allowlist
		{"pipe in host", "host|evil:3128", true},                 // pipe not in allowlist
		{"exclamation in host", "host!evil:3128", true},          // exclamation not in allowlist
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := evalSymlinks(t, t.TempDir())
			var content string
			if tc.quoted {
				content = "exclude:\n  dirs: []\n  files: []\nnetwork:\n  proxy:\n    address: \"" + tc.address + "\"\n"
			} else {
				content = "exclude:\n  dirs: []\n  files: []\nnetwork:\n  proxy:\n    address: " + tc.address + "\n"
			}
			if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}

			// Load must succeed — validation is deferred to runRun.
			_, _, _, err := Load(root)
			if err != nil {
				t.Fatalf("Load must not validate proxy address syntax (got unexpected error for %q): %v", tc.address, err)
			}
		})
	}
}

func TestLoad_Network_ExcludesUnchanged(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

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

	excl, netCfg, _, err := Load(root)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if len(excl.Files) != 1 || excl.Files[0] != filepath.Join(root, "secret.env") {
		t.Errorf("unexpected Files: %v", excl.Files)
	}
	if len(excl.Dirs) != 1 || excl.Dirs[0] != filepath.Join(root, "private") {
		t.Errorf("unexpected Dirs: %v", excl.Dirs)
	}

	if netCfg.ProxyAddress != "10.0.0.1:8888" {
		t.Errorf("ProxyAddress: got %q, want %q", netCfg.ProxyAddress, "10.0.0.1:8888")
	}
}

// TestLoad_Network_LogFieldRejected verifies that a config file containing the
// removed network.log field is rejected with a strict-decode (unknown field) error.
// This protects users with old configs from silently losing their log configuration.
func TestLoad_Network_LogFieldRejected(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	content := "network:\n  proxy:\n    address: \"\"\n  log: requests.log\n"
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, _, _, err := Load(root)
	if err == nil {
		t.Fatal("expected error for unknown network.log field, got nil")
	}
	if !strings.HasPrefix(err.Error(), "projectconfig:") {
		t.Errorf("error missing 'projectconfig:' prefix: %q", err.Error())
	}
}

// ---- exclude.scan tests ----

func TestLoad_Scan_ValidPatternsAndSkipDirs(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")

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
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")

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
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")

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
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	// KnownFields(true) must reject an unknown key under exclude.scan.
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

// ---- cache: block tests ----

// TestLoad_Cache_AbsentBlock verifies that an absent cache: block defaults both
// Cache.Content and Cache.Agent to true (backward-compatible).
func TestLoad_Cache_AbsentBlock(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	// Write a config without any cache: key.
	content := "exclude:\n  dirs: []\n  files: []\n"
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, _, cacheCfg, err := Load(root)
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

// TestLoad_Cache_MissingFile verifies that a missing file also defaults Cache to {true,true}.
func TestLoad_Cache_MissingFile(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	_, _, cacheCfg, err := Load(root)
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

// TestLoad_Cache_BothFalse verifies that explicit content:false + agent:false
// produces Cache{false,false}.
func TestLoad_Cache_BothFalse(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	content := "cache:\n  content: false\n  agent: false\n"
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, _, cacheCfg, err := Load(root)
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

// TestLoad_Cache_BothTrue verifies that explicit content:true + agent:true
// produces Cache{true,true}.
func TestLoad_Cache_BothTrue(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	content := "cache:\n  content: true\n  agent: true\n"
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, _, cacheCfg, err := Load(root)
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

// TestLoad_Cache_MixedContentFalseAgentAbsent verifies that content:false with
// agent absent produces Cache{false,true} (absent field defaults to true).
func TestLoad_Cache_MixedContentFalseAgentAbsent(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	content := "cache:\n  content: false\n"
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, _, cacheCfg, err := Load(root)
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

// TestLoad_Cache_MixedAgentFalseContentAbsent verifies that agent:false with
// content absent produces Cache{true,false}.
func TestLoad_Cache_MixedAgentFalseContentAbsent(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
	root := evalSymlinks(t, t.TempDir())

	content := "cache:\n  agent: false\n"
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, _, cacheCfg, err := Load(root)
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

// TestLoad_Cache_UnknownKeyRejected verifies that KnownFields(true) rejects an
// unknown key nested under cache:.
func TestLoad_Cache_UnknownKeyRejected(t *testing.T) {
	docker.SkipNonPOSIX(t, "symlinks required; POSIX-only per CLAUDE.md")
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

// stringSlicesEqual returns true if two string slices are element-wise equal.
// nil and empty [] are treated as equal (both have len 0).
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
