package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Zwergpro/makeslop/internal/projectconfig"
)

func assertAbsent(t *testing.T, p string) {
	t.Helper()

	if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected absent %s, got %v", p, err)
	}
}

func runCacheFixture(t *testing.T) (base, root, workspaceDir string) {
	t.Helper()
	setHomeToTestParent(t)

	base = evalSymlinks(t, t.TempDir())
	root = evalSymlinks(t, t.TempDir())
	t.Chdir(root)
	workspaceDir = strings.TrimSpace(initWithImage(t, base))

	return base, root, workspaceDir
}

func writeCacheConfig(t *testing.T, root, text string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(root, projectconfig.Filename), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRun_QuietWarningBeforeCacheError(t *testing.T) {
	skipNonPOSIX(t, "symlinks require POSIX")
	base, root, _ := runCacheFixture(t)
	writeFile(t, filepath.Join(root, "secret"), "not real credentials")

	if err := os.Symlink("secret", filepath.Join(root, "secret.env")); err != nil {
		t.Fatal(err)
	}

	writeCacheConfig(t, root, "exclude:\n  scan:\n    patterns: ['*.env']\ncache:\n  static: [./missing:/opt/cache]\n")
	f := newFakeDocker(0, true)
	var stderr bytes.Buffer
	code := runWithExitCodeAndDeps(base, io.Discard, &stderr, dockerDeps{api: f}, []string{"run", "--quiet"})
	text := stderr.String()
	warning, failure := strings.Index(text, "NOT masked"), strings.Index(text, "cache.static[0]")

	if code != 1 || f.Started || warning < 0 || failure <= warning || strings.Count(text, "cache.static[0]") != 1 {
		t.Fatalf("exit=%d started=%v stderr=%q", code, f.Started, text)
	}
}

func TestRun_ConfiguredCacheMounts(t *testing.T) {
	base, root, ws := runCacheFixture(t)
	file := filepath.Join(root, "settings.json")
	writeFile(t, file, "keep")
	writeCacheConfig(t, root, `cache:
  shared: [.claude]
  local: [docs]
  static: ["./settings.json:~/.settings.json"]
`)

	fc := newFakeDocker(0, true)
	if _, stderr, err := runCmdWithDeps(
		t,
		base,
		depsFrom(fc),
		"run",
	); err != nil {
		t.Fatalf("%v %s", err, stderr)
	}

	for _, want := range []struct{ host, target string }{
		{host: base + "/shared/.claude/", target: "/home/user/.claude/"},
		{host: ws + "/docs/", target: "/workspace/" + filepath.Base(ws) + "/docs/"},
		{host: file, target: "/home/user/.settings.json"},
	} {
		if !hasBind(fc.LastPreview, want.host, want.target, false) {
			t.Fatalf("missing %#v in %s", want, fc.LastPreview)
		}
	}
}

func TestRun_CacheRelativeStatic(t *testing.T) {
	for _, target := range []string{"settings.json", "./settings.json", "nested/settings.json"} {
		t.Run(target, func(t *testing.T) {
			base, root, ws := runCacheFixture(t)
			writeFile(t, filepath.Join(root, "source.json"), "keep")
			writeCacheConfig(t, root, "cache:\n  static: [\"./source.json:"+target+":ro\"]\n")
			subdir := filepath.Join(root, "subdir")
			mkdirAll(t, subdir)
			t.Chdir(subdir)
			fc := newFakeDocker(0, true)
			if _, _, err := runCmdWithDeps(t, base, depsFrom(fc), "run"); err != nil {
				t.Fatal(err)
			}
			wantTarget := "/workspace/" + filepath.Base(ws) + "/" + filepath.ToSlash(filepath.Clean(target))
			if !hasBind(fc.LastPreview, root+"/source.json", wantTarget, true) {
				t.Fatalf("missing read-only bind at %s: %s", wantTarget, fc.LastPreview)
			}
		})
	}
}

func TestRun_CacheRelativeStaticConflicts(t *testing.T) {
	for _, target := range []string{".makeslop.yaml", "./.git/hooks/hook", "docs/file", ".", "../escape"} {
		t.Run(target, func(t *testing.T) {
			base, root, _ := runCacheFixture(t)
			writeFile(t, filepath.Join(root, "source.json"), "keep")
			writeCacheConfig(t, root, "exclude: {dirs: [docs]}\ncache:\n  static: [\"./source.json:"+target+":ro\"]\n")
			fc := newFakeDocker(0, true)
			if _, _, err := runCmdWithDeps(t, base, depsFrom(fc), "run"); err == nil {
				t.Fatal("accepted protected, excluded or escaping target")
			}
			if fc.Started {
				t.Fatal("started despite invalid target")
			}
		})
	}
}

func TestRun_CacheModes(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		name := "real"
		if dryRun {
			name = "dry run"
		}
		t.Run(name, func(t *testing.T) {
			base, root, ws := runCacheFixture(t)
			writeFile(t, filepath.Join(root, "settings.json"), "keep")
			mkdirAll(t, filepath.Join(root, "tools"))
			writeCacheConfig(t, root, `cache:
  shared: [shared-ro:ro, shared-rw:rw, shared-default]
  local: [local-ro:ro, local-rw:rw, local-default]
  static: ["./settings.json:/opt/settings:ro", "./tools:/opt/tools:rw", "./settings.json:/opt/default"]
`)
			fc := newFakeDocker(0, true)
			args := []string{"run"}
			if dryRun {
				args = append(args, "--dry-run")
			}
			stdout, stderr, err := runCmdWithDeps(t, base, depsFrom(fc), args...)
			if err != nil {
				t.Fatalf("%v %s", err, stderr)
			}

			preview := fc.LastPreview

			if dryRun {
				preview = stdout
			}

			mounts := previewFlags(preview, "--mount")
			rootTarget := "/workspace/" + filepath.Base(ws)
			for _, tc := range []struct {
				target   string
				readOnly bool
			}{
				{target: "/home/user/shared-ro/", readOnly: true},
				{target: "/home/user/shared-rw/"},
				{target: "/home/user/shared-default/"},
				{target: rootTarget + "/local-ro/", readOnly: true},
				{target: rootTarget + "/local-rw/"},
				{target: rootTarget + "/local-default/"},
				{target: "/opt/settings", readOnly: true},
				{target: "/opt/tools/"},
				{target: "/opt/default"},
			} {
				suffix := ",target=" + tc.target

				if tc.readOnly {
					suffix += ",readonly"
				}

				hasMount := slices.ContainsFunc(mounts, func(m string) bool { return strings.HasSuffix(m, suffix) })
				if !hasMount {
					t.Errorf("missing mount ending in %q: %v", suffix, mounts)
				}
			}

			for _, dir := range []string{
				base + "/shared/shared-ro", base + "/shared/shared-rw", base + "/shared/shared-default",
				ws + "/local-ro", ws + "/local-rw", ws + "/local-default",
			} {
				if dryRun {
					assertAbsent(t, dir)
				} else if info, err := os.Stat(dir); err != nil || !info.IsDir() {
					t.Errorf("missing managed directory %s: %v", dir, err)
				}
			}
		})
	}
}

func TestRun_CacheCreatesOnlyManagedDirectories(t *testing.T) {
	base, root, ws := runCacheFixture(t)
	file := filepath.Join(root, "settings.json")
	writeFile(t, file, "keep")
	writeCacheConfig(t, root, `cache:
  shared: [.claude]
  local: [docs]
  static: ["./settings.json:~/.settings.json"]
`)

	if _, _, err := runCmdWithDeps(
		t,
		base,
		depsFrom(newFakeDocker(0, true)),
		"run",
	); err != nil {
		t.Fatal(err)
	}

	for _, dir := range []string{base + "/shared/.claude", ws + "/docs"} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("missing directory %s: %v", dir, err)
		}
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "keep" {
		t.Fatalf("file changed: %q %v", data, err)
	}
	assertAbsent(t, filepath.Join(ws, ".codex"))
}

func TestRun_CacheDryRunNoWrites(t *testing.T) {
	base, root, ws := runCacheFixture(t)
	writeCacheConfig(t, root, "cache: {shared: [.claude], local: [docs]}")
	fc := newFakeDocker(0, true)

	out, _, err := runCmdWithDeps(
		t,
		base,
		depsFrom(fc),
		"run",
		"--dry-run",
	)
	if err != nil {
		t.Fatal(err)
	}

	assertAbsent(t, base+"/shared/.claude")
	assertAbsent(t, ws+"/docs")
	if !strings.Contains(out, base+"/shared/.claude/") || !strings.Contains(out, ws+"/docs/") {
		t.Fatalf("missing cache mounts: %s", out)
	}
	if fc.DaemonChecked || fc.Started {
		t.Fatal("dry-run contacted Docker")
	}
}

func TestRun_CacheDryRunMatchesLaunch(t *testing.T) {
	base, root, _ := runCacheFixture(t)
	writeCacheConfig(t, root, "cache: {shared: [.claude], local: [docs]}")
	fc := newFakeDocker(0, true)

	out, _, err := runCmdWithDeps(
		t,
		base,
		depsFrom(fc),
		"run",
		"--dry-run",
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCmdWithDeps(
		t,
		base,
		depsFrom(fc),
		"run",
	); err != nil {
		t.Fatal(err)
	}

	if out != fc.LastPreview+"\n" {
		t.Fatal("dry-run/launch drift")
	}
}

func TestRun_CacheDryRunMissingStatic(t *testing.T) {
	base, root, _ := runCacheFixture(t)
	writeCacheConfig(t, root, "cache: {static: [\"missing:/opt/missing\"]}")

	if _, _, err := runCmdWithDeps(
		t,
		base,
		depsFrom(newFakeDocker(0, true)),
		"run",
		"--dry-run",
	); err == nil {
		t.Fatal("accepted missing static source")
	}
}

func TestRun_CacheFromSubdirectory(t *testing.T) {
	base, root, _ := runCacheFixture(t)
	mkdirAll(t, filepath.Join(root, "tools/subdir"))
	writeCacheConfig(t, root, "cache: {static: [\"./tools:/opt/tools\"]}")
	t.Chdir(filepath.Join(root, "tools/subdir"))
	fc := newFakeDocker(0, true)

	if _, _, err := runCmdWithDeps(
		t,
		base,
		depsFrom(fc),
		"run",
	); err != nil {
		t.Fatal(err)
	}
	if !hasBind(fc.LastPreview, root+"/tools/", "/opt/tools/", false) {
		t.Fatalf("wrong relative bind: %s", fc.LastPreview)
	}
}

func cacheFailureConfig(kind, workspace string, fc *fakeDocker) string {
	text := "cache:\n  shared: [new]\n  local: [docs]\n"
	switch kind {
	case "static":
		text += "  static: [\"missing:/opt/missing\"]\n"
	case "policy":
		text += "  static: [\".:/workspace/" + filepath.Base(workspace) + "/.git\"]\n"
	case "image":
		fc.ImageMissing = true
	case "network":
		text += "networks: [missing]\n"
	}

	return text
}

func TestRun_CacheFailureNoCreation(t *testing.T) {
	for _, kind := range []string{"static", "policy", "image", "network"} {
		t.Run(kind, func(t *testing.T) {
			base, root, ws := runCacheFixture(t)
			fc := newFakeDocker(0, true)
			writeCacheConfig(t, root, cacheFailureConfig(kind, ws, fc))

			if _, _, err := runCmdWithDeps(
				t,
				base,
				depsFrom(fc),
				"run",
			); err == nil {
				t.Fatal("expected launch error")
			}

			assertAbsent(t, base+"/shared/new")
			assertAbsent(t, ws+"/docs")
			if fc.Started {
				t.Fatal("container launched on validation failure")
			}
		})
	}
}

func TestRun_CacheMissingExcludeConflict(t *testing.T) {
	base, root, ws := runCacheFixture(t)
	writeCacheConfig(t, root, "exclude: {dirs: [docs]}\ncache: {local: [docs:ro]}\n")
	fc := newFakeDocker(0, true)

	_, _, err := runCmdWithDeps(
		t,
		base,
		depsFrom(fc),
		"run",
	)
	if err == nil || !strings.Contains(err.Error(), "docs") {
		t.Fatalf("expected conflict: %v", err)
	}
	assertAbsent(t, ws+"/docs")
}

func TestRun_CacheScanMaskOrder(t *testing.T) {
	base, root, ws := runCacheFixture(t)
	writeFile(t, filepath.Join(root, "docs/key.pem"), "secret")
	writeCacheConfig(t, root, "exclude: {scan: {patterns: ['*.pem']}}\ncache: {local: [docs]}\n")
	fc := newFakeDocker(0, true)
	if _, _, err := runCmdWithDeps(
		t,
		base,
		depsFrom(fc),
		"run",
	); err != nil {
		t.Fatal(err)
	}

	mounts := previewFlags(fc.LastPreview, "--mount")
	docs := "/workspace/" + filepath.Base(ws) + "/docs/"
	overlay := slices.Index(mounts, bindFlag(ws+"/docs/", docs, false))
	mask := slices.Index(mounts, bindFlag("/dev/null", docs+"key.pem", false))
	if overlay < 0 || mask <= overlay {
		t.Fatalf("mask order: %v", mounts)
	}
}

func TestRun_NoImplicitCaches(t *testing.T) {
	base, root, ws := runCacheFixture(t)
	for _, text := range []string{"", "cache: {}"} {
		t.Run(text, func(t *testing.T) {
			writeFile(t, base+"/.claude.json", "legacy")
			writeCacheConfig(t, root, text)
			fc := newFakeDocker(0, true)

			if _, _, err := runCmdWithDeps(
				t,
				base,
				depsFrom(fc),
				"run",
			); err != nil {
				t.Fatal(err)
			}
			for _, mount := range previewFlags(fc.LastPreview, "--mount") {
				if strings.Contains(mount, ",target=/home/user/") || strings.Contains(mount, ",source="+ws+"/") {
					t.Fatalf("implicit mount %#v", mount)
				}
			}

			if data, err := os.ReadFile(base + "/.claude.json"); err != nil || string(data) != "legacy" {
				t.Fatal("legacy data changed")
			}
		})
	}
}
