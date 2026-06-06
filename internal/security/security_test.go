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

// skipNonPOSIX skips on non-POSIX hosts per the CLAUDE.md invariant.
// This is a private copy of docker.SkipNonPOSIX. Importing the docker package
// here would introduce an unwanted upward dependency from the low-level security
// package into docker (which drags in config, assets, and testing helpers).
// The two copies must stay in sync; the implementation is one line.
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

// mustWriteFile creates parent directories and writes a file with the given content.
func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// loadStubConfig writes projectconfig.Stub into a temp directory and loads the
// result, returning the canonical Patterns and SkipDirs defined by the stub.
// This makes tests authoritative consumers of the actual defaults rather than
// maintaining a duplicate hardcoded list.
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

// TestScan_EmptyPatterns_ReturnsNil verifies the opt-in invariant: when no patterns
// are provided, Scan returns nil without walking the tree.
func TestScan_EmptyPatterns_ReturnsNil(t *testing.T) {
	root := evalSymlinks(t, t.TempDir())
	mustWriteFile(t, filepath.Join(root, ".env"), "SECRET=1\n")

	got, err := Scan(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatalf("Scan with nil patterns returned error: %v", err)
	}
	if got != nil {
		t.Errorf("Scan with nil patterns: got %v, want nil", got)
	}
}

// TestScan_DefaultPatterns_PositiveCases verifies each default glob hits its file.
func TestScan_DefaultPatterns_PositiveCases(t *testing.T) {
	root := evalSymlinks(t, t.TempDir())
	patterns, _ := loadStubConfig(t)

	positives := []string{
		// *.env
		".env",
		"local.env",
		"app.env",
		// .env.*
		".env.local",
		".env.production",
		".env.staging",
		// *.pem
		"app.pem",
		"cert.pem",
		// *.key
		"server.key",
		"private.key",
		// id_rsa*
		"id_rsa",
		"id_rsa.pub",
		// id_ed25519*
		"id_ed25519",
		"id_ed25519.pub",
		// .npmrc
		".npmrc",
		// .netrc
		".netrc",
		// .git-credentials
		".git-credentials",
	}

	for _, name := range positives {
		mustWriteFile(t, filepath.Join(root, name), "data\n")
	}

	got, err := Scan(context.Background(), root, patterns, nil)
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

// TestScan_DefaultPatterns_NegativeCases verifies well-known non-secret names are NOT matched.
func TestScan_DefaultPatterns_NegativeCases(t *testing.T) {
	root := evalSymlinks(t, t.TempDir())
	patterns, _ := loadStubConfig(t)

	negatives := []string{
		".envrc",       // .env without extension separator
		"environment",  // plain word
		"keyfile",      // no extension
		"keyboard.txt", // contains "key" but extension is .txt
	}

	for _, name := range negatives {
		mustWriteFile(t, filepath.Join(root, name), "data\n")
	}

	got, err := Scan(context.Background(), root, patterns, nil)
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

// TestScan_SkipDirs_PrunesMatchingDirs verifies secrets in skip-dirs are not returned.
func TestScan_SkipDirs_PrunesMatchingDirs(t *testing.T) {
	root := evalSymlinks(t, t.TempDir())
	patterns, skipDirs := loadStubConfig(t)

	// Plant secrets in skip-dirs — must NOT be returned.
	mustWriteFile(t, filepath.Join(root, ".git", ".env"), "SECRET=2\n")
	mustWriteFile(t, filepath.Join(root, "node_modules", "x.env"), "SECRET=3\n")
	mustWriteFile(t, filepath.Join(root, "vendor", "lib.key"), "SECRET=4\n")
	mustWriteFile(t, filepath.Join(root, ".venv", "secret.pem"), "SECRET=5\n")

	// Plant a secret outside skip-dirs — MUST be returned.
	mustWriteFile(t, filepath.Join(root, "app", ".env"), "SECRET=1\n")

	got, err := Scan(context.Background(), root, patterns, skipDirs)
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}

	// Only app/.env should be returned.
	if len(got) != 1 {
		t.Fatalf("expected 1 result, got %d: %v", len(got), got)
	}
	if filepath.Base(filepath.Dir(got[0])) != "app" {
		t.Errorf("expected file in 'app/', got %q", got[0])
	}
}

// TestScan_ResultsSorted verifies returned paths are sorted.
func TestScan_ResultsSorted(t *testing.T) {
	root := evalSymlinks(t, t.TempDir())
	patterns, _ := loadStubConfig(t)

	// Write files that would come back in reverse order if unsorted.
	mustWriteFile(t, filepath.Join(root, "z.env"), "data\n")
	mustWriteFile(t, filepath.Join(root, "a.env"), "data\n")
	mustWriteFile(t, filepath.Join(root, "m.env"), "data\n")

	got, err := Scan(context.Background(), root, patterns, nil)
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 results, got %d: %v", len(got), got)
	}

	// Verify sorted order.
	for i := 1; i < len(got); i++ {
		if got[i] < got[i-1] {
			t.Errorf("results not sorted: got[%d]=%q < got[%d]=%q", i, got[i], i-1, got[i-1])
		}
	}
}

// TestScan_NestedAndHiddenFiles verifies that dotfiles nested in subdirs are found.
func TestScan_NestedAndHiddenFiles(t *testing.T) {
	root := evalSymlinks(t, t.TempDir())
	patterns, _ := loadStubConfig(t)

	mustWriteFile(t, filepath.Join(root, "sub", "dir", ".env"), "SECRET=1\n")
	mustWriteFile(t, filepath.Join(root, ".env"), "SECRET=2\n")

	got, err := Scan(context.Background(), root, patterns, nil)
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 results, got %d: %v", len(got), got)
	}
}

// TestScan_GitignoreFileStillFound verifies that .gitignore-d files are still found
// (the walker does not consult .gitignore — replaces fd's --no-ignore flag).
func TestScan_GitignoreFileStillFound(t *testing.T) {
	root := evalSymlinks(t, t.TempDir())
	patterns, _ := loadStubConfig(t)

	mustWriteFile(t, filepath.Join(root, ".gitignore"), "ignored.env\n")
	mustWriteFile(t, filepath.Join(root, "ignored.env"), "SECRET=1\n")

	got, err := Scan(context.Background(), root, patterns, nil)
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

// TestScan_Symlink_Dropped verifies that a symlink to a secret file is not returned,
// and that symlinked directories are not walked.
func TestScan_Symlink_Dropped(t *testing.T) {
	skipNonPOSIX(t, "symlink tests require POSIX; makeslop is POSIX-only")
	root := evalSymlinks(t, t.TempDir())
	patterns, _ := loadStubConfig(t)

	// Real secret file outside root.
	other := evalSymlinks(t, t.TempDir())
	secretFile := filepath.Join(other, ".env")
	mustWriteFile(t, secretFile, "SECRET=1\n")

	// Symlink to the secret file inside root — should be dropped.
	if err := os.Symlink(secretFile, filepath.Join(root, "link.env")); err != nil {
		t.Fatalf("symlink file: %v", err)
	}

	// Symlink to a directory that contains a secret — target contents must NOT be walked.
	secretDir := filepath.Join(other, "dir")
	mustWriteFile(t, filepath.Join(secretDir, "secret.env"), "SECRET=2\n")
	if err := os.Symlink(secretDir, filepath.Join(root, "linked-dir")); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}

	got, err := Scan(context.Background(), root, patterns, nil)
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no results (only symlinks in root), got %v", got)
	}
}

// TestScan_WalkError_Propagated verifies that an unreadable subdirectory causes
// Scan to return an error (fail-loud invariant).
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

	_, err := Scan(context.Background(), root, patterns, nil)
	if err == nil {
		t.Error("expected error from unreadable subdir, got nil")
	}
}

// TestScan_ContextCancelled verifies that Scan respects context cancellation
// and returns the context error rather than continuing the walk.
func TestScan_ContextCancelled(t *testing.T) {
	root := evalSymlinks(t, t.TempDir())
	patterns, _ := loadStubConfig(t)

	// Plant enough files to make a walk likely to hit the cancellation check.
	for i := 0; i < 10; i++ {
		mustWriteFile(t, filepath.Join(root, "sub", fmt.Sprintf("%d.env", i)), "data\n")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := Scan(ctx, root, patterns, nil)
	if err == nil {
		t.Error("expected error from cancelled context, got nil")
	}
}

// TestScan_UnderRootInvariant verifies every returned path is local (under) root,
// pinning the internal/docker/spec.go:95 "host is under ProjectRoot" contract.
func TestScan_UnderRootInvariant(t *testing.T) {
	root := evalSymlinks(t, t.TempDir())
	patterns, _ := loadStubConfig(t)

	mustWriteFile(t, filepath.Join(root, ".env"), "SECRET=1\n")
	mustWriteFile(t, filepath.Join(root, "sub", ".env"), "SECRET=2\n")

	got, err := Scan(context.Background(), root, patterns, nil)
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
