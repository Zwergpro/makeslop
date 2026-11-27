package docker

import (
	"reflect"
	"testing"
)

func cacheTestLayout() Layout {
	return Layout{Main: Project{ProjectRoot: ProjectRoot{
		HostRoot: "/host/app", Name: "app", ContainerRoot: "/workspace/app",
	}}}
}

func TestBuildForeground_ConfiguredCacheMounts(t *testing.T) {
	caches := []Mount{
		{Type: MountBind, Host: "/host/shared/custom/", Container: "/home/user/custom/"},
		{Type: MountBind, Host: "/host/file", Container: "/opt/file", ReadOnly: true},
	}
	layout := cacheTestLayout()
	layout.Main.ProtectConfig, layout.Main.MaskGitHooks = true, true
	layout.Main.MaskedFiles = []string{"/host/app/key"}
	layout.Joins = []Project{{ProjectRoot: ProjectRoot{
		HostRoot: "/host/lib", Name: "lib", ContainerRoot: "/workspace/lib",
	}}}
	opts := BuildInputs{Layout: layout, CacheMounts: caches}

	spec := testForegroundCreate(opts)
	// The project bind and two policy mounts precede the configured caches.
	if !reflect.DeepEqual(spec.Mounts[3:5], caches) {
		t.Fatalf("cache order: %#v", spec.Mounts)
	}
	if spec.Mounts[5].Host != "/dev/null" || spec.Mounts[6].Host != "/host/lib" {
		t.Fatalf("mask/join order: %#v", spec.Mounts)
	}
}

func TestBuildForeground_CacheMountsDoNotAliasOptions(t *testing.T) {
	caches := []Mount{{Type: MountBind, Host: "/host/shared/custom/", Container: "/home/user/custom/"}}
	opts := BuildInputs{
		Layout:      cacheTestLayout(),
		CacheMounts: caches,
	}

	spec := testForegroundCreate(opts)
	spec.Mounts[1].Host = "changed"
	if caches[0].Host != "/host/shared/custom/" {
		t.Fatal("mutated caller mounts")
	}
}

func TestBuildForeground_NoImplicitCaches(t *testing.T) {
	spec := testForegroundCreate(BuildInputs{Layout: cacheTestLayout()})
	want := []Mount{{Type: MountBind, Host: "/host/app", Container: "/workspace/app"}}
	if !reflect.DeepEqual(spec.Mounts, want) {
		t.Fatalf("implicit mounts: %#v", spec.Mounts)
	}
}

func TestDriftGuard_ConfiguredCacheMounts(t *testing.T) {
	caches := []Mount{
		{Type: MountBind, Host: "/host/a b,c'd/", Container: "/home/user/a b,c'd/", ReadOnly: true},
		{Type: MountBind, Host: "/host/file", Container: "/opt/file", ReadOnly: true},
		{Type: MountBind, Host: "/host/rw/", Container: "/opt/rw/"},
		{Type: MountBind, Host: "/host/rw-file", Container: "/opt/rw-file"},
	}
	spec := testForegroundCreate(BuildInputs{
		Layout: cacheTestLayout(), CacheMounts: caches,
	})
	sdk := spec.HostConfig().Mounts
	args := collectMountArgs(spec.Args())

	for i, want := range caches {
		// Skip the project-root bind, which is present in both projections.
		got := parseMountArg(t, args[i+1])
		if got["source"] != want.Host || got["target"] != want.Container {
			t.Fatalf("args %#v want %#v", got, want)
		}

		_, readOnly := got["readonly"]
		if readOnly != want.ReadOnly {
			t.Fatalf("args mode %#v want %#v", got, want)
		}
		samePaths := sdk[i+1].Source == want.Host && sdk[i+1].Target == want.Container
		if !samePaths || sdk[i+1].ReadOnly != want.ReadOnly {
			t.Fatalf("SDK %#v", sdk[i+1])
		}
	}
}
