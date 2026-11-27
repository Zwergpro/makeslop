package cache

import (
	"path/filepath"
	"reflect"
	"syscall"
	"testing"

	"github.com/Zwergpro/makeslop/internal/docker"
	"github.com/Zwergpro/makeslop/internal/projectconfig"
)

func cacheFixture(t *testing.T) (Sources, docker.Layout) {
	t.Helper()

	base := evalSymlinks(t, t.TempDir())
	root := evalSymlinks(t, t.TempDir())
	home := evalSymlinks(t, t.TempDir())
	paths := Sources{
		BaseDir: base, WorkspaceDir: filepath.Join(base, "workspaces/app-abc123"), HostHome: home,
	}

	return paths, docker.Layout{Main: docker.Project{ProjectRoot: testRoot("app-abc123", root)}}
}

func staticCacheFixture(t *testing.T) (Sources, docker.Layout) {
	t.Helper()

	paths, projects := cacheFixture(t)
	for _, root := range []string{paths.HostHome, projects.Main.HostRoot} {
		writeFile(t, filepath.Join(root, "a b,c'd"), "keep")
	}

	dir := filepath.Join(projects.Main.HostRoot, "tools")
	mkdirAll(t, dir)
	symlink(t, dir, filepath.Join(projects.Main.HostRoot, "link"))

	return paths, projects
}

func TestResolveCache_UsesResolvedRoots(t *testing.T) {
	paths, projects := cacheFixture(t)
	main := docker.Project{ProjectRoot: docker.ProjectRoot{
		Name: "app", HostRoot: projects.Main.HostRoot, ContainerRoot: "/src/app",
	}}
	join := docker.Project{ProjectRoot: docker.ProjectRoot{
		Name: "lib", HostRoot: t.TempDir(), ContainerRoot: "/src/lib",
	}}
	layout := docker.Layout{Main: main, Joins: []docker.Project{join}}
	source := filepath.Join(main.HostRoot, "source")
	writeFile(t, source, "keep")
	cfg := projectconfig.CacheMounts{Static: []projectconfig.CacheBind{{Host: "./source", Container: "build"}}}
	plan, err := Resolve(cfg, paths, layout, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Mounts()[0]; got.Host != source || got.Container != "/src/app/build" {
		t.Fatalf("mount=%+v", got)
	}

	cfg.Static[0].Container = "/src/lib/build"
	if _, err := Resolve(cfg, paths, layout, nil); err == nil {
		t.Fatal("cache mount under joined root accepted")
	}

	layout.Joins[0].ContainerRoot = "/src/lib/"
	if _, err := Resolve(cfg, paths, layout, nil); err == nil {
		t.Fatal("noncanonical join root bypassed cache validation")
	}
}

func TestResolveCache_Mappings(t *testing.T) {
	paths, projects := cacheFixture(t)
	file := filepath.Join(projects.Main.HostRoot, "settings.json")
	writeFile(t, file, "keep")
	cfg := projectconfig.CacheMounts{
		Shared: []projectconfig.CacheDirectory{{Name: ".claude"}}, Local: []projectconfig.CacheDirectory{{Name: "docs"}},
		Static: []projectconfig.CacheBind{{Host: "./settings.json", Container: "~/.settings.json"}},
	}

	got, err := Resolve(
		cfg,
		paths,
		projects,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []docker.Mount{
		{Type: docker.MountBind, Host: paths.BaseDir + "/shared/.claude/", Container: "/home/user/.claude/"},
		{Type: docker.MountBind, Host: paths.WorkspaceDir + "/docs/", Container: "/workspace/app-abc123/docs/"},
		{Type: docker.MountBind, Host: file, Container: "/home/user/.settings.json"},
	}
	if !reflect.DeepEqual(got.Mounts(), want) {
		t.Fatalf("got %#v, want %#v", got.Mounts(), want)
	}
	if len(got.dirs) != 2 {
		t.Fatalf("dirs %#v", got.dirs)
	}

	assertAbsent(t, filepath.Join(paths.BaseDir, "shared"))
	assertAbsent(t, paths.WorkspaceDir)
}

func TestResolveCache_Modes(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		name := "rw"
		if readOnly {
			name = "ro"
		}
		t.Run(name, func(t *testing.T) {
			paths, projects := staticCacheFixture(t)
			cfg := projectconfig.CacheMounts{
				Shared: []projectconfig.CacheDirectory{{Name: "shared", ReadOnly: readOnly}},
				Local:  []projectconfig.CacheDirectory{{Name: "local", ReadOnly: readOnly}},
				Static: []projectconfig.CacheBind{
					{Host: "./a b,c'd", Container: "/opt/file", ReadOnly: readOnly},
					{Host: "./tools", Container: "/opt/tools", ReadOnly: readOnly},
				},
			}
			plan, err := Resolve(cfg, paths, projects, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := []docker.Mount{
				{
					Type:      docker.MountBind,
					Host:      paths.BaseDir + "/shared/shared/",
					Container: "/home/user/shared/",
					ReadOnly:  readOnly,
				},
				{
					Type:      docker.MountBind,
					Host:      paths.WorkspaceDir + "/local/",
					Container: "/workspace/app-abc123/local/",
					ReadOnly:  readOnly,
				},
				{
					Type:      docker.MountBind,
					Host:      projects.Main.HostRoot + "/a b,c'd",
					Container: "/opt/file",
					ReadOnly:  readOnly,
				},
				{
					Type:      docker.MountBind,
					Host:      projects.Main.HostRoot + "/tools/",
					Container: "/opt/tools/",
					ReadOnly:  readOnly,
				},
			}
			if !reflect.DeepEqual(plan.Mounts(), want) {
				t.Fatalf("got %#v, want %#v", plan.Mounts(), want)
			}
		})
	}
}

func TestResolveCache_StaticFilePaths(t *testing.T) {
	paths, projects := staticCacheFixture(t)
	cases := []struct{ name, host, target, wantHost, wantTarget string }{
		{
			name: "bare relative target", host: "./a b,c'd", target: "settings.json",
			wantHost: projects.Main.HostRoot + "/a b,c'd", wantTarget: "/workspace/app-abc123/settings.json",
		},
		{
			name: "dot relative target", host: "./a b,c'd", target: "./settings.json",
			wantHost: projects.Main.HostRoot + "/a b,c'd", wantTarget: "/workspace/app-abc123/settings.json",
		},
		{
			name: "nested relative target", host: "./a b,c'd", target: "nested/settings.json",
			wantHost: projects.Main.HostRoot + "/a b,c'd", wantTarget: "/workspace/app-abc123/nested/settings.json",
		},
		{
			name: "file characters", host: "./a b,c'd", target: "/opt/a b,c'd",
			wantHost: projects.Main.HostRoot + "/a b,c'd", wantTarget: "/opt/a b,c'd",
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
			plan, err := Resolve(
				cfg,
				paths,
				projects,
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			if m := plan.Mounts()[0]; m.Host != tc.wantHost || m.Container != tc.wantTarget {
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
			plan, err := Resolve(
				cfg,
				paths,
				projects,
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}

			want := docker.Mount{
				Type:      docker.MountBind,
				Host:      projects.Main.HostRoot + "/tools/",
				Container: "/opt/tools/",
			}
			if plan.Mounts()[0] != want {
				t.Fatalf("got %#v, want %#v", plan.Mounts()[0], want)
			}
		})
	}
}

func TestResolveCache_MissingStaticDoesNotCreateManaged(t *testing.T) {
	paths, projects := cacheFixture(t)
	cfg := projectconfig.CacheMounts{
		Shared: []projectconfig.CacheDirectory{{Name: "new"}},
		Static: []projectconfig.CacheBind{{Host: "missing", Container: "/opt/x"}},
	}

	if _, err := Resolve(
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
	fifo := filepath.Join(projects.Main.HostRoot, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(projects.Main.HostRoot, "dangling")
	symlink(t, filepath.Join(projects.Main.HostRoot, "missing"), dangling)

	for _, source := range []string{fifo, dangling} {
		cfg := projectconfig.CacheMounts{
			Static: []projectconfig.CacheBind{{Host: source, Container: "/opt/x"}},
		}
		if _, err := Resolve(
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
	cfg := projectconfig.CacheMounts{
		Shared: []projectconfig.CacheDirectory{{Name: ".claude"}},
		Local:  []projectconfig.CacheDirectory{{Name: ".claude"}},
	}
	first, err := Resolve(
		cfg,
		paths,
		projects,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	projects.Main.ProjectRoot = testRoot("other-def456", projects.Main.HostRoot)
	paths.WorkspaceDir = filepath.Join(paths.BaseDir, "workspaces/other-def456")
	second, err := Resolve(
		cfg,
		paths,
		projects,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.Mounts()[0].Host != second.Mounts()[0].Host || first.Mounts()[1].Host == second.Mounts()[1].Host {
		t.Fatal("wrong scope")
	}
}

func TestResolveCache_Empty(t *testing.T) {
	_, projects := cacheFixture(t)
	empty, err := Resolve(
		projectconfig.CacheMounts{},
		Sources{},
		projects,
		nil,
	)
	if err != nil || len(empty.Mounts()) != 0 {
		t.Fatalf("empty: %#v %v", empty, err)
	}
}

func TestResolveCache_ProtectedDestinations(t *testing.T) {
	paths, projects := cacheFixture(t)
	projects.Joins = []docker.Project{{ProjectRoot: testRoot("joined", t.TempDir())}}
	source := filepath.Join(projects.Main.HostRoot, "source")
	writeFile(t, source, "x")
	targets := []string{
		"/", "/home", "/home/user", "~", "/workspace", "/workspace/app-abc123",
		"/workspace/joined", "/workspace/joined/data",
		"/workspace/app-abc123/.makeslop.yaml", "/workspace/app-abc123/.makeslop.yaml/x",
		"/workspace/app-abc123/.git", "/workspace/app-abc123/.git/hooks",
		"/workspace/app-abc123/.git/hooks/x", "/tmp", "/tmp/a", "/tmp/makeslop-zmx", "/proc/x", "/sys", "/dev/x",
	}

	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			cfg := projectconfig.CacheMounts{
				Static: []projectconfig.CacheBind{{Host: source, Container: target, ReadOnly: true}},
			}
			if _, err := Resolve(
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
	source := filepath.Join(projects.Main.HostRoot, "source")
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
			if _, err := Resolve(
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
	source := filepath.Join(projects.Main.HostRoot, "source")
	writeFile(t, source, "x")
	cfg := projectconfig.CacheMounts{Static: []projectconfig.CacheBind{}}
	for _, target := range []string{
		"/opt/tool", "/opt/toolbox", "/tmp-cache", "/device", "/home/user/tool", "/workspace/app-abc123/tool",
	} {
		cfg.Static = append(cfg.Static, projectconfig.CacheBind{Host: source, Container: target})
	}

	if _, err := Resolve(
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
	source := filepath.Join(projects.Main.HostRoot, "source")
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
			if _, err := Resolve(
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
	source := filepath.Join(projects.Main.HostRoot, "source")
	writeFile(t, source, "x")
	cases := []projectconfig.CacheMounts{
		{
			Shared: []projectconfig.CacheDirectory{{Name: ".claude"}},
			Static: []projectconfig.CacheBind{{Host: source, Container: "~/.claude/x"}},
		},
		{
			Local:  []projectconfig.CacheDirectory{{Name: "docs"}},
			Static: []projectconfig.CacheBind{{Host: source, Container: "/workspace/app-abc123/docs"}},
		},
	}

	for _, cfg := range cases {
		if _, err := Resolve(
			cfg,
			paths,
			projects,
			nil,
		); err == nil {
			t.Fatal("accepted cross-group overlap")
		}
	}
}
