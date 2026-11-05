package cli

import (
	"path/filepath"
	"reflect"
	"syscall"
	"testing"

	"github.com/Zwergpro/makeslop/internal/docker"
	"github.com/Zwergpro/makeslop/internal/projectconfig"
)

func cacheFixture(t *testing.T) (cachePaths, []docker.Project) {
	t.Helper()

	base := evalSymlinks(t, t.TempDir())
	root := evalSymlinks(t, t.TempDir())
	home := evalSymlinks(t, t.TempDir())
	paths := cachePaths{
		BaseDir: base, WorkspaceDir: filepath.Join(base, "workspaces/app-abc123"), HostHome: home,
	}

	return paths, []docker.Project{{Host: root, Name: "app-abc123"}}
}

func staticCacheFixture(t *testing.T) (cachePaths, []docker.Project) {
	t.Helper()

	paths, projects := cacheFixture(t)
	for _, root := range []string{paths.HostHome, projects[0].Host} {
		writeFile(t, filepath.Join(root, "a b,c'd"), "keep")
	}

	dir := filepath.Join(projects[0].Host, "tools")
	mkdirAll(t, dir)
	symlink(t, dir, filepath.Join(projects[0].Host, "link"))

	return paths, projects
}

func TestResolveCache_Mappings(t *testing.T) {
	paths, projects := cacheFixture(t)
	file := filepath.Join(projects[0].Host, "settings.json")
	writeFile(t, file, "keep")
	cfg := projectconfig.CacheMounts{
		Shared: []string{".claude"}, Local: []string{"docs"},
		Static: []projectconfig.CacheBind{{Host: "./settings.json", Container: "~/.settings.json"}},
	}

	got, err := resolveCache(
		cfg,
		paths,
		projects,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []docker.Mount{
		{Host: paths.BaseDir + "/shared/.claude/", Container: "/home/user/.claude/"},
		{Host: paths.WorkspaceDir + "/docs/", Container: "/workspace/app-abc123/docs/"},
		{Host: file, Container: "/home/user/.settings.json"},
	}
	if !reflect.DeepEqual(got.Mounts, want) {
		t.Fatalf("got %#v, want %#v", got.Mounts, want)
	}
	if len(got.Dirs) != 2 {
		t.Fatalf("dirs %#v", got.Dirs)
	}

	assertAbsent(t, filepath.Join(paths.BaseDir, "shared"))
	assertAbsent(t, paths.WorkspaceDir)
}

func TestResolveCache_StaticFilePaths(t *testing.T) {
	paths, projects := staticCacheFixture(t)
	cases := []struct{ name, host, target, wantHost, wantTarget string }{
		{
			name: "file characters", host: "./a b,c'd", target: "/opt/a b,c'd",
			wantHost: projects[0].Host + "/a b,c'd", wantTarget: "/opt/a b,c'd",
		},
		{
			name: "home expansion", host: "~/a b,c'd", target: "~/a b,c'd",
			wantHost: paths.HostHome + "/a b,c'd", wantTarget: "/home/user/a b,c'd",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := projectconfig.CacheMounts{
				Static: []projectconfig.CacheBind{{Host: tc.host, Container: tc.target}},
			}
			plan, err := resolveCache(
				cfg,
				paths,
				projects,
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			if m := plan.Mounts[0]; m.Host != tc.wantHost || m.Container != tc.wantTarget {
				t.Fatalf("got %#v", m)
			}
		})
	}
}

func TestResolveCache_StaticDirectorySources(t *testing.T) {
	paths, projects := staticCacheFixture(t)
	for _, source := range []string{"./tools", "./link"} {
		t.Run(source, func(t *testing.T) {
			cfg := projectconfig.CacheMounts{
				Static: []projectconfig.CacheBind{{Host: source, Container: "/opt/tools"}},
			}
			plan, err := resolveCache(
				cfg,
				paths,
				projects,
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}

			want := docker.Mount{Host: projects[0].Host + "/tools/", Container: "/opt/tools/"}
			if plan.Mounts[0] != want {
				t.Fatalf("got %#v, want %#v", plan.Mounts[0], want)
			}
		})
	}
}

func TestResolveCache_MissingStaticDoesNotCreateManaged(t *testing.T) {
	paths, projects := cacheFixture(t)
	cfg := projectconfig.CacheMounts{
		Shared: []string{"new"},
		Static: []projectconfig.CacheBind{{Host: "missing", Container: "/opt/x"}},
	}

	if _, err := resolveCache(
		cfg,
		paths,
		projects,
		nil,
	); err == nil {
		t.Fatal("accepted missing")
	}
	assertAbsent(t, filepath.Join(paths.BaseDir, "shared"))
}

func TestResolveCache_SpecialAndDanglingSources(t *testing.T) {
	skipNonPOSIX(t, "FIFO and symlink sources")
	paths, projects := cacheFixture(t)
	fifo := filepath.Join(projects[0].Host, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(projects[0].Host, "dangling")
	symlink(t, filepath.Join(projects[0].Host, "missing"), dangling)

	for _, source := range []string{fifo, dangling} {
		cfg := projectconfig.CacheMounts{
			Static: []projectconfig.CacheBind{{Host: source, Container: "/opt/x"}},
		}
		if _, err := resolveCache(
			cfg,
			paths,
			projects,
			nil,
		); err == nil {
			t.Fatalf("accepted %s", source)
		}
	}
}

func TestResolveCache_SharedAndLocalScope(t *testing.T) {
	paths, projects := cacheFixture(t)
	cfg := projectconfig.CacheMounts{Shared: []string{".claude"}, Local: []string{".claude"}}
	first, err := resolveCache(
		cfg,
		paths,
		projects,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	projects[0].Name = "other-def456"
	paths.WorkspaceDir = filepath.Join(paths.BaseDir, "workspaces/other-def456")
	second, err := resolveCache(
		cfg,
		paths,
		projects,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.Mounts[0].Host != second.Mounts[0].Host || first.Mounts[1].Host == second.Mounts[1].Host {
		t.Fatal("wrong scope")
	}
}

func TestResolveCache_Empty(t *testing.T) {
	_, projects := cacheFixture(t)
	empty, err := resolveCache(
		projectconfig.CacheMounts{},
		cachePaths{},
		projects,
		nil,
	)
	if err != nil || len(empty.Mounts) != 0 {
		t.Fatalf("empty: %#v %v", empty, err)
	}
}

func TestResolveCache_ProtectedDestinations(t *testing.T) {
	paths, projects := cacheFixture(t)
	projects = append(projects, docker.Project{Host: t.TempDir(), Name: "joined"})
	source := filepath.Join(projects[0].Host, "source")
	writeFile(t, source, "x")
	targets := []string{
		"/", "/home", "/home/user", "~", "/workspace", "/workspace/app-abc123",
		"/workspace/joined", "/workspace/joined/data",
		"/workspace/app-abc123/.makeslop.yaml", "/workspace/app-abc123/.makeslop.yaml/x",
		"/workspace/app-abc123/.git", "/workspace/app-abc123/.git/hooks",
		"/workspace/app-abc123/.git/hooks/x", "/tmp", "/tmp/a", "/proc/x", "/sys", "/dev/x",
	}

	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			cfg := projectconfig.CacheMounts{
				Static: []projectconfig.CacheBind{{Host: source, Container: target}},
			}
			if _, err := resolveCache(
				cfg,
				paths,
				projects,
				nil,
			); err == nil {
				t.Fatalf("accepted %q", target)
			}
		})
	}
}

func TestResolveCache_OverlappingDestinations(t *testing.T) {
	paths, projects := cacheFixture(t)
	source := filepath.Join(projects[0].Host, "source")
	writeFile(t, source, "x")
	cases := []struct {
		name    string
		targets []string
	}{
		{name: "normalized duplicate", targets: []string{"/opt/x", "/opt/./x/"}},
		{name: "parent", targets: []string{"/opt/x", "/opt/x/a"}},
		{name: "child first", targets: []string{"/opt/x/a", "/opt/x"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := projectconfig.CacheMounts{Static: []projectconfig.CacheBind{}}
			for _, target := range tc.targets {
				cfg.Static = append(cfg.Static, projectconfig.CacheBind{Host: source, Container: target})
			}
			if _, err := resolveCache(
				cfg,
				paths,
				projects,
				nil,
			); err == nil {
				t.Fatal("accepted overlapping destinations")
			}
		})
	}
}

func TestResolveCache_PathSegmentBoundaries(t *testing.T) {
	paths, projects := cacheFixture(t)
	source := filepath.Join(projects[0].Host, "source")
	writeFile(t, source, "x")
	cfg := projectconfig.CacheMounts{Static: []projectconfig.CacheBind{}}
	for _, target := range []string{
		"/opt/tool", "/opt/toolbox", "/tmp-cache", "/device", "/home/user/tool", "/workspace/app-abc123/tool",
	} {
		cfg.Static = append(cfg.Static, projectconfig.CacheBind{Host: source, Container: target})
	}

	if _, err := resolveCache(
		cfg,
		paths,
		projects,
		nil,
	); err != nil {
		t.Fatal(err)
	}
}

func TestResolveCache_ExcludeConflicts(t *testing.T) {
	paths, projects := cacheFixture(t)
	source := filepath.Join(projects[0].Host, "source")
	writeFile(t, source, "x")
	cases := []struct{ name, target, exclude string }{
		{name: "equality", target: "/workspace/app-abc123/docs", exclude: "docs"},
		{name: "child", target: "/workspace/app-abc123/docs", exclude: "docs/key"},
		{name: "ancestor", target: "/workspace/app-abc123/docs/sub", exclude: "docs"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := projectconfig.CacheMounts{
				Static: []projectconfig.CacheBind{{Host: source, Container: tc.target}},
			}
			if _, err := resolveCache(
				cfg,
				paths,
				projects,
				[]string{tc.exclude},
			); err == nil {
				t.Fatal("accepted cache/exclude conflict")
			}
		})
	}
}

func TestResolveCache_CrossGroupConflicts(t *testing.T) {
	paths, projects := cacheFixture(t)
	source := filepath.Join(projects[0].Host, "source")
	writeFile(t, source, "x")
	cases := []projectconfig.CacheMounts{
		{
			Shared: []string{".claude"},
			Static: []projectconfig.CacheBind{{Host: source, Container: "~/.claude/x"}},
		},
		{
			Local:  []string{"docs"},
			Static: []projectconfig.CacheBind{{Host: source, Container: "/workspace/app-abc123/docs"}},
		},
	}

	for _, cfg := range cases {
		if _, err := resolveCache(
			cfg,
			paths,
			projects,
			nil,
		); err == nil {
			t.Fatal("accepted cross-group overlap")
		}
	}
}
