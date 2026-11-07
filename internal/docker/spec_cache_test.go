package docker

import (
	"reflect"
	"testing"
)

func TestBuildSpec_ConfiguredCacheMounts(t *testing.T) {
	caches := []Mount{
		{Host: "/host/shared/custom/", Container: "/home/user/custom/"},
		{Host: "/host/file", Container: "/opt/file", ReadOnly: true},
	}
	opts := Options{
		Projects: []Project{
			{
				Host: "/host/app", Name: "app", ProtectConfig: true, MaskGitHooks: true,
				MaskedFiles: []string{"/host/app/key"},
			},
			{Host: "/host/lib", Name: "lib"},
		},
		CacheMounts: caches,
	}

	spec := BuildSpec(opts)
	// The project bind and two policy mounts precede the configured caches.
	if !reflect.DeepEqual(spec.Mounts[3:5], caches) {
		t.Fatalf("cache order: %#v", spec.Mounts)
	}
	if spec.Mounts[5].Host != "/dev/null" || spec.Mounts[6].Host != "/host/lib" {
		t.Fatalf("mask/join order: %#v", spec.Mounts)
	}
}

func TestBuildSpec_CacheMountsDoNotAliasOptions(t *testing.T) {
	caches := []Mount{{Host: "/host/shared/custom/", Container: "/home/user/custom/"}}
	opts := Options{
		Projects:    []Project{{Host: "/host/app", Name: "app"}},
		CacheMounts: caches,
	}

	spec := BuildSpec(opts)
	spec.Mounts[1].Host = "changed"
	if caches[0].Host != "/host/shared/custom/" {
		t.Fatal("mutated caller mounts")
	}
}

func TestBuildSpec_NoImplicitCaches(t *testing.T) {
	spec := BuildSpec(Options{Projects: []Project{{Host: "/host/app", Name: "app"}}})
	want := []Mount{{Host: "/host/app", Container: "/workspace/app"}}
	if !reflect.DeepEqual(spec.Mounts, want) {
		t.Fatalf("implicit mounts: %#v", spec.Mounts)
	}
}

func TestDriftGuard_ConfiguredCacheMounts(t *testing.T) {
	caches := []Mount{
		{Host: "/host/a b,c'd/", Container: "/home/user/a b,c'd/", ReadOnly: true},
		{Host: "/host/file", Container: "/opt/file", ReadOnly: true},
		{Host: "/host/rw/", Container: "/opt/rw/"},
		{Host: "/host/rw-file", Container: "/opt/rw-file"},
	}
	spec := BuildSpec(Options{
		Projects: []Project{{Host: "/host/app", Name: "app"}}, CacheMounts: caches,
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
