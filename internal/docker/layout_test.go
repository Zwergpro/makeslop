package docker

import (
	"slices"
	"testing"
)

func TestBuildSpec_UsesResolvedRoots(t *testing.T) {
	o := Options{Layout: Layout{
		Main: Project{
			ProjectRoot:   ProjectRoot{Name: "app", HostRoot: "/host/app", ContainerRoot: "/src/app"},
			ProtectConfig: true,
		},
		Joins: []Project{{
			ProjectRoot: ProjectRoot{Name: "lib", HostRoot: "/host/lib", ContainerRoot: "/src/lib"},
			MaskedFiles: []string{"/host/lib/.env"},
		}},
	}}
	s := BuildSpec(o)
	want := []string{"/src/app", "/src/app/.makeslop.yaml", "/src/lib", "/src/lib/.env"}
	var got []string

	for _, m := range s.Mounts {
		got = append(got, m.Container)
	}

	if s.Workdir != "/src/app" || !slices.Equal(got, want) {
		t.Fatalf("workdir=%q targets=%v, want /src/app and %v", s.Workdir, got, want)
	}
}
