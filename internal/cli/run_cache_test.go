package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Zwergpro/makeslop/internal/projectconfig"
)

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
		if !hasMountWithContainerAndHost(fc.LastSpec.Mounts, want.target, want.host) {
			t.Fatalf("missing %#v in %#v", want, fc.LastSpec.Mounts)
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
			found := false
			for _, mount := range fc.LastSpec.Mounts {
				if mount.Container == wantTarget {
					found = true
					if mount.Host != root+"/source.json" || !mount.ReadOnly {
						t.Fatalf("wrong mount: %#v", mount)
					}
				}
			}
			if !found {
				t.Fatalf("missing target %s", wantTarget)
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
				if dryRun {
					found := false
					for _, line := range strings.Split(stdout, "\n") {
						mountLine := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), "\\"))
						if strings.Contains(mountLine, "target="+tc.target+",") || strings.HasSuffix(mountLine, "target="+tc.target) {
							found = true
							if strings.Contains(line, ",readonly") != tc.readOnly {
								t.Errorf("wrong mode for %s: %s", tc.target, line)
							}
						}
					}
					if !found {
						t.Errorf("missing target %s in %s", tc.target, stdout)
					}
					continue
				}
				found := false
				for _, mount := range fc.LastSpec.Mounts {
					if mount.Container == tc.target {
						found = true
						if mount.ReadOnly != tc.readOnly {
							t.Errorf("wrong mode: %#v", mount)
						}
					}
				}
				if !found {
					t.Errorf("missing target %s", tc.target)
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

	if out != fc.LastSpec.ShellCommand()+"\n" {
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
	if !hasMountWithContainerAndHost(fc.LastSpec.Mounts, "/opt/tools/", root+"/tools/") {
		t.Fatalf("wrong relative bind: %#v", fc.LastSpec.Mounts)
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

	overlay, mask := -1, -1
	for i, mount := range fc.LastSpec.Mounts {
		if mount.Host == ws+"/docs/" {
			overlay = i
		}
		if mount.Host == "/dev/null" && mount.Container == "/workspace/"+filepath.Base(ws)+"/docs/key.pem" {
			mask = i
		}
	}
	if overlay < 0 || mask <= overlay {
		t.Fatalf("mask order: %#v", fc.LastSpec.Mounts)
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
			for _, mount := range fc.LastSpec.Mounts {
				if strings.HasPrefix(mount.Container, "/home/user/") || strings.HasPrefix(mount.Host, ws+"/") {
					t.Fatalf("implicit mount %#v", mount)
				}
			}

			if data, err := os.ReadFile(base + "/.claude.json"); err != nil || string(data) != "legacy" {
				t.Fatal("legacy data changed")
			}
		})
	}
}
