package security

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Zwergpro/makeslop/internal/projectconfig"
)

func skipNonPOSIX(t *testing.T, why string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip(why)
	}
}

// evalSymlinks resolves a temp dir — on macOS /tmp is a symlink, so raw
// t.TempDir() paths violate the EvalSymlinks precondition.
func evalSymlinks(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("eval symlinks %s: %v", dir, err)
	}
	return resolved
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// loadStubConfig loads the stub's canonical Patterns/SkipDirs, so tests consume
// the actual defaults instead of a duplicated hardcoded list.
func loadStubConfig(t *testing.T) (patterns, skipDirs []string) {
	t.Helper()
	dir := t.TempDir()
	if err := projectconfig.Scaffold(dir, projectconfig.Cache{Content: true, Agent: true}); err != nil {
		t.Fatalf("loadStubConfig: scaffold: %v", err)
	}
	excl, _, _, err := projectconfig.Load(dir)
	if err != nil {
		t.Fatalf("loadStubConfig: load: %v", err)
	}
	return excl.Patterns, excl.SkipDirs
}

// Opt-in invariant: no patterns means no walk.
func TestScan_EmptyPatterns_ReturnsNil(t *testing.T) {
	root := evalSymlinks(t, t.TempDir())
	mustWriteFile(t, filepath.Join(root, ".env"), "SECRET=1\n")

	got, symlinks, err := Scan(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatalf("Scan with nil patterns returned error: %v", err)
	}
	if got != nil {
		t.Errorf("Scan with nil patterns: got %v, want nil", got)
	}
	if symlinks != nil {
		t.Errorf("Scan with nil patterns: symlinkMatches got %v, want nil", symlinks)
	}
}

func TestScan_DefaultPatterns_PositiveCases(t *testing.T) {
	root := evalSymlinks(t, t.TempDir())
	patterns, _ := loadStubConfig(t)

	positives := []string{
		".env",
		"local.env",
		"app.env",
		".env.local",
		".env.production",
		".env.staging",
		"app.pem",
		"cert.pem",
		"server.key",
		"private.key",
		"id_rsa",
		"id_rsa.pub",
		"id_ed25519",
		"id_ed25519.pub",
		".npmrc",
		".netrc",
		".git-credentials",
		// 8 new patterns added in the security hardening pass
		"cert.p12",
		"key.pfx",
		"terraform.tfstate",
		".pypirc",
		".htpasswd",
		"service-account-prod.json",
		"kubeconfig",
		"cluster.kubeconfig",
	}

	for _, name := range positives {
		mustWriteFile(t, filepath.Join(root, name), "data\n")
	}

	got, _, err := Scan(context.Background(), root, patterns, nil)
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}

	gotSet := make(map[string]bool, len(got))
	for _, p := range got {
		gotSet[filepath.Base(p)] = true
	}

	for _, name := range positives {
		if !gotSet[name] {
			t.Errorf("positive: %q should be matched by default patterns, but was not", name)
		}
	}
}

func TestScan_DefaultPatterns_NegativeCases(t *testing.T) {
	root := evalSymlinks(t, t.TempDir())
	patterns, _ := loadStubConfig(t)

	negatives := []string{
		".envrc",
		"environment",
		"keyfile",
		"keyboard.txt", // contains "key" but extension is .txt
	}

	for _, name := range negatives {
		mustWriteFile(t, filepath.Join(root, name), "data\n")
	}

	got, _, err := Scan(context.Background(), root, patterns, nil)
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}

	gotSet := make(map[string]bool, len(got))
	for _, p := range got {
		gotSet[filepath.Base(p)] = true
	}

	for _, name := range negatives {
		if gotSet[name] {
			t.Errorf("negative: %q should NOT be matched, but was", name)
		}
	}
}

func TestScan_SkipDirs_PrunesMatchingDirs(t *testing.T) {
	root := evalSymlinks(t, t.TempDir())
	patterns, skipDirs := loadStubConfig(t)

	// Secrets in skip-dirs must NOT be returned.
	mustWriteFile(t, filepath.Join(root, ".git", ".env"), "SECRET=2\n")
	mustWriteFile(t, filepath.Join(root, "node_modules", "x.env"), "SECRET=3\n")
	mustWriteFile(t, filepath.Join(root, "vendor", "lib.key"), "SECRET=4\n")
	mustWriteFile(t, filepath.Join(root, ".venv", "secret.pem"), "SECRET=5\n")

	// Secret outside skip-dirs MUST be returned.
	mustWriteFile(t, filepath.Join(root, "app", ".env"), "SECRET=1\n")

	got, _, err := Scan(context.Background(), root, patterns, skipDirs)
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("expected 1 result, got %d: %v", len(got), got)
	}
	if filepath.Base(filepath.Dir(got[0])) != "app" {
		t.Errorf("expected file in 'app/', got %q", got[0])
	}
}

func TestScan_ResultsSorted(t *testing.T) {
	root := evalSymlinks(t, t.TempDir())
	patterns, _ := loadStubConfig(t)

	// Names ordered so an unsorted walk would return them reversed.
	mustWriteFile(t, filepath.Join(root, "z.env"), "data\n")
	mustWriteFile(t, filepath.Join(root, "a.env"), "data\n")
	mustWriteFile(t, filepath.Join(root, "m.env"), "data\n")

	got, _, err := Scan(context.Background(), root, patterns, nil)
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 results, got %d: %v", len(got), got)
	}

	for i := 1; i < len(got); i++ {
		if got[i] < got[i-1] {
			t.Errorf("results not sorted: got[%d]=%q < got[%d]=%q", i, got[i], i-1, got[i-1])
		}
	}
}

func TestScan_NestedAndHiddenFiles(t *testing.T) {
	root := evalSymlinks(t, t.TempDir())
	patterns, _ := loadStubConfig(t)

	mustWriteFile(t, filepath.Join(root, "sub", "dir", ".env"), "SECRET=1\n")
	mustWriteFile(t, filepath.Join(root, ".env"), "SECRET=2\n")

	got, _, err := Scan(context.Background(), root, patterns, nil)
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 results, got %d: %v", len(got), got)
	}
}

// The walker does not consult .gitignore (replaces fd's --no-ignore flag).
func TestScan_GitignoreFileStillFound(t *testing.T) {
	root := evalSymlinks(t, t.TempDir())
	patterns, _ := loadStubConfig(t)

	mustWriteFile(t, filepath.Join(root, ".gitignore"), "ignored.env\n")
	mustWriteFile(t, filepath.Join(root, "ignored.env"), "SECRET=1\n")

	got, _, err := Scan(context.Background(), root, patterns, nil)
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}

	gotSet := make(map[string]bool, len(got))
	for _, p := range got {
		gotSet[filepath.Base(p)] = true
	}
	if !gotSet["ignored.env"] {
		t.Error("ignored.env should be found despite .gitignore, but was not")
	}
}

// Symlinks to a secret file/dir must not appear in paths (no-follow invariant).
// Symlinks whose basename matches a pattern must appear in symlinkMatches so the
// caller can warn the user; non-matching symlinks are silently ignored.
func TestScan_Symlink_Dropped(t *testing.T) {
	skipNonPOSIX(t, "symlink tests require POSIX; makeslop is POSIX-only")
	root := evalSymlinks(t, t.TempDir())
	patterns, _ := loadStubConfig(t)

	other := evalSymlinks(t, t.TempDir())
	secretFile := filepath.Join(other, ".env")
	mustWriteFile(t, secretFile, "SECRET=1\n")

	// link.env — basename matches "*.env" → should appear in symlinkMatches.
	if err := os.Symlink(secretFile, filepath.Join(root, "link.env")); err != nil {
		t.Fatalf("symlink file: %v", err)
	}

	secretDir := filepath.Join(other, "dir")
	mustWriteFile(t, filepath.Join(secretDir, "secret.env"), "SECRET=2\n")
	// linked-dir — basename "linked-dir" does NOT match any pattern → silently ignored.
	if err := os.Symlink(secretDir, filepath.Join(root, "linked-dir")); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}

	got, symlinks, err := Scan(context.Background(), root, patterns, nil)
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("paths: expected empty (no regular files), got %v", got)
	}
	if len(symlinks) != 1 {
		t.Fatalf("symlinkMatches: expected 1 entry (link.env), got %v", symlinks)
	}
	if filepath.Base(symlinks[0]) != "link.env" {
		t.Errorf("symlinkMatches[0] base: got %q, want %q", filepath.Base(symlinks[0]), "link.env")
	}
}

// Fail-loud: an unreadable subdir must surface as an error, not be skipped.
func TestScan_WalkError_Propagated(t *testing.T) {
	skipNonPOSIX(t, "chmod 0000 requires POSIX; makeslop is POSIX-only")
	if os.Getuid() == 0 {
		t.Skip("root bypasses permission checks")
	}
	root := evalSymlinks(t, t.TempDir())
	patterns, _ := loadStubConfig(t)

	locked := filepath.Join(root, "locked")
	mustWriteFile(t, filepath.Join(locked, "placeholder"), "")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	_, _, err := Scan(context.Background(), root, patterns, nil)
	if err == nil {
		t.Error("expected error from unreadable subdir, got nil")
	}
}

func TestScan_ContextCancelled(t *testing.T) {
	root := evalSymlinks(t, t.TempDir())
	patterns, _ := loadStubConfig(t)

	for i := 0; i < 10; i++ {
		mustWriteFile(t, filepath.Join(root, "sub", fmt.Sprintf("%d.env", i)), "data\n")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := Scan(ctx, root, patterns, nil)
	if err == nil {
		t.Error("expected error from cancelled context, got nil")
	}
}

// Every returned path must be local to root, pinning the docker/spec.go
// "host is under ProjectRoot" contract.
func TestScan_UnderRootInvariant(t *testing.T) {
	root := evalSymlinks(t, t.TempDir())
	patterns, _ := loadStubConfig(t)

	mustWriteFile(t, filepath.Join(root, ".env"), "SECRET=1\n")
	mustWriteFile(t, filepath.Join(root, "sub", ".env"), "SECRET=2\n")

	got, _, err := Scan(context.Background(), root, patterns, nil)
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}

	for _, p := range got {
		rel, err := filepath.Rel(root, p)
		if err != nil {
			t.Errorf("filepath.Rel(%q, %q) error: %v", root, p, err)
			continue
		}
		if !filepath.IsLocal(rel) {
			t.Errorf("path %q is not local to root %q (rel=%q)", p, root, rel)
		}
	}
}

// Symlink whose basename matches a pattern goes into symlinkMatches (not paths).
func TestScan_SymlinkMatch_ReportedInSecondSlice(t *testing.T) {
	skipNonPOSIX(t, "symlink tests require POSIX; makeslop is POSIX-only")
	root := evalSymlinks(t, t.TempDir())
	patterns, _ := loadStubConfig(t)

	other := evalSymlinks(t, t.TempDir())
	target := filepath.Join(other, "real.env")
	mustWriteFile(t, target, "SECRET=x\n")

	linkPath := filepath.Join(root, "my.env")
	if err := os.Symlink(target, linkPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	paths, symlinks, err := Scan(context.Background(), root, patterns, nil)
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if len(paths) != 0 {
		t.Errorf("paths: expected empty, got %v", paths)
	}
	if len(symlinks) != 1 || filepath.Base(symlinks[0]) != "my.env" {
		t.Errorf("symlinkMatches: expected [my.env], got %v", symlinks)
	}
}

// Symlink whose basename does NOT match any pattern is silently ignored.
func TestScan_SymlinkNoMatch_SilentlyIgnored(t *testing.T) {
	skipNonPOSIX(t, "symlink tests require POSIX; makeslop is POSIX-only")
	root := evalSymlinks(t, t.TempDir())
	patterns, _ := loadStubConfig(t)

	other := evalSymlinks(t, t.TempDir())
	target := filepath.Join(other, "readme.txt")
	mustWriteFile(t, target, "not a secret\n")

	if err := os.Symlink(target, filepath.Join(root, "readme.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	paths, symlinks, err := Scan(context.Background(), root, patterns, nil)
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if len(paths) != 0 {
		t.Errorf("paths: expected empty, got %v", paths)
	}
	if len(symlinks) != 0 {
		t.Errorf("symlinkMatches: expected empty (no-match symlink is silent), got %v", symlinks)
	}
}

// symlinkMatches is sorted, independent of walk order.
func TestScan_SymlinkMatches_Sorted(t *testing.T) {
	skipNonPOSIX(t, "symlink tests require POSIX; makeslop is POSIX-only")
	root := evalSymlinks(t, t.TempDir())
	patterns, _ := loadStubConfig(t)

	other := evalSymlinks(t, t.TempDir())
	target := filepath.Join(other, "real.env")
	mustWriteFile(t, target, "SECRET=x\n")

	// Create symlinks whose names would sort differently from walk order.
	for _, name := range []string{"z.env", "a.env", "m.env"} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatalf("symlink %s: %v", name, err)
		}
	}

	paths, symlinks, err := Scan(context.Background(), root, patterns, nil)
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if len(paths) != 0 {
		t.Errorf("paths: expected empty (all are symlinks), got %v", paths)
	}
	if len(symlinks) != 3 {
		t.Fatalf("symlinkMatches: expected 3, got %v", symlinks)
	}
	for i := 1; i < len(symlinks); i++ {
		if symlinks[i] < symlinks[i-1] {
			t.Errorf("symlinkMatches not sorted: [%d]=%q < [%d]=%q", i, symlinks[i], i-1, symlinks[i-1])
		}
	}
}

// Regular-file behaviour is unchanged by the symlink-reporting addition.
func TestScan_RegularFile_UnaffectedBySymlinkChange(t *testing.T) {
	skipNonPOSIX(t, "symlink tests require POSIX; makeslop is POSIX-only")
	root := evalSymlinks(t, t.TempDir())
	patterns, _ := loadStubConfig(t)

	mustWriteFile(t, filepath.Join(root, ".env"), "SECRET=1\n")
	mustWriteFile(t, filepath.Join(root, "normal.txt"), "not a secret\n")

	other := evalSymlinks(t, t.TempDir())
	target := filepath.Join(other, "real.env")
	mustWriteFile(t, target, "SECRET=2\n")
	if err := os.Symlink(target, filepath.Join(root, "link.env")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	paths, symlinks, err := Scan(context.Background(), root, patterns, nil)
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	// Regular .env must be in paths.
	if len(paths) != 1 || filepath.Base(paths[0]) != ".env" {
		t.Errorf("paths: expected [.env], got %v", paths)
	}
	// link.env (symlink matching *.env) must be in symlinkMatches.
	if len(symlinks) != 1 || filepath.Base(symlinks[0]) != "link.env" {
		t.Errorf("symlinkMatches: expected [link.env], got %v", symlinks)
	}
}
