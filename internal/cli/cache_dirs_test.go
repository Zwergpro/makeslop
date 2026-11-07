package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type symlinkCacheDirCase struct {
	name, link, leaf string
	dangling         bool
}

func assertAbsent(t *testing.T, p string) {
	t.Helper()

	if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected absent %s, got %v", p, err)
	}
}

func symlinkCacheDirFixture(t *testing.T, tc symlinkCacheDirCase) (cacheDir, string) {
	t.Helper()

	base := evalSymlinks(t, t.TempDir())
	outside := evalSymlinks(t, t.TempDir())
	link := filepath.Join(base, tc.link)
	mkdirAll(t, filepath.Dir(link))

	target := outside
	if tc.dangling {
		target = filepath.Join(outside, "missing")
	}
	symlink(t, target, link)

	dir := cacheDir{Base: base, Path: filepath.Join(base, tc.leaf), Entry: "cache.shared[0]"}
	return dir, outside
}

func TestCheckCacheDir_RejectsSymlinkComponents(t *testing.T) {
	skipNonPOSIX(t, "symlinks")
	cases := []symlinkCacheDirCase{
		{name: "shared base", link: "shared", leaf: "shared/a"},
		{name: "workspace base", link: "workspaces/app-abc123", leaf: "workspaces/app-abc123/docs"},
		{name: "intermediate", link: "shared/a", leaf: "shared/a/b"},
		{name: "dangling", link: "shared/a", leaf: "shared/a", dangling: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, outside := symlinkCacheDirFixture(t, tc)
			if err := checkCacheDir(dir); err == nil {
				t.Fatal("accepted symlink")
			}
			if err := prepareCacheDirs(context.Background(), []cacheDir{dir}); err == nil {
				t.Fatal("prepared symlink")
			}

			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatalf("outside changed: %v %v", entries, err)
			}
		})
	}
}

func TestCheckCacheDir_Containment(t *testing.T) {
	base := evalSymlinks(t, t.TempDir())
	cases := []struct {
		name, path string
		valid      bool
	}{
		{name: "shared", path: filepath.Join(base, "shared/.config/tool"), valid: true},
		{name: "local", path: filepath.Join(base, "workspaces/app-abc123/docs"), valid: true},
		{name: "equal", path: base},
		{name: "outside", path: base + "-other/docs"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkCacheDir(cacheDir{Base: base, Path: tc.path, Entry: "cache.local[0]"})
			if (err == nil) != tc.valid {
				t.Fatalf("check: %v", err)
			}
			if tc.valid {
				assertAbsent(t, tc.path)
			}
		})
	}
}

func TestPrepareCacheDirs_PrechecksAll(t *testing.T) {
	base := evalSymlinks(t, t.TempDir())
	bad := filepath.Join(base, "bad")
	writeFile(t, bad, "file")
	dirs := []cacheDir{
		{Base: base, Path: filepath.Join(base, "shared/good"), Entry: "cache.shared[0]"},
		{Base: base, Path: filepath.Join(bad, "child"), Entry: "cache.local[0]"},
	}

	if err := prepareCacheDirs(context.Background(), dirs); err == nil {
		t.Fatal("accepted file parent")
	}
	assertAbsent(t, filepath.Join(base, "shared"))
}

func TestPrepareCacheDirs_Canceled(t *testing.T) {
	base := evalSymlinks(t, t.TempDir())
	target := filepath.Join(base, "shared/a")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := prepareCacheDirs(ctx, []cacheDir{{Base: base, Path: target, Entry: "cache.shared[0]"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	assertAbsent(t, filepath.Join(base, "shared"))
}

func TestPrepareCacheDirs_CreatesAndPreserves(t *testing.T) {
	base := evalSymlinks(t, t.TempDir())
	dirs := []cacheDir{
		{Base: base, Path: filepath.Join(base, "shared/.config/tool"), Entry: "cache.shared[0]"},
		{Base: base, Path: filepath.Join(base, "workspaces/app-abc123/docs"), Entry: "cache.local[0]"},
	}
	if err := prepareCacheDirs(context.Background(), dirs); err != nil {
		t.Fatal(err)
	}
	for _, dir := range dirs {
		if info, err := os.Stat(dir.Path); err != nil || !info.IsDir() {
			t.Fatalf("directory %s: %v", dir.Path, err)
		}
	}

	sentinel := filepath.Join(dirs[0].Path, "sentinel")
	writeFile(t, sentinel, "keep")
	if err := prepareCacheDirs(context.Background(), dirs); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep" {
		t.Fatalf("sentinel %q: %v", data, err)
	}

	assertAbsent(t, filepath.Join(base, ".claude.json"))
	assertAbsent(t, filepath.Join(base, "workspaces/app-abc123/CLAUDE.md"))
}
