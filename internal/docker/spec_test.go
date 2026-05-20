package docker

import (
	"encoding/csv"
	"reflect"
	"strings"
	"testing"
)

func sampleOptions() Options {
	return Options{
		ProjectRoot:   "/home/me/code/myproj",
		WorkspaceName: "myproj-abc123",
		BaseDir:       "/home/me/.makeslop",
		Image:         "claudebox",
		Command:       "/bin/zsh",
	}
}

func TestBuildSpec_PopulatesWorkdirAndSecurityFlags(t *testing.T) {
	spec := BuildSpec(sampleOptions())

	if spec.Workdir != "/workspace/myproj-abc123" {
		t.Errorf("Workdir = %q, want %q", spec.Workdir, "/workspace/myproj-abc123")
	}
	if got, want := spec.Tmpfs, []string{"/tmp:size=100m"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Tmpfs = %v, want %v", got, want)
	}
	if got, want := spec.CapDrop, []string{"ALL"}; !reflect.DeepEqual(got, want) {
		t.Errorf("CapDrop = %v, want %v", got, want)
	}
	if got, want := spec.SecOpt, []string{"no-new-privileges"}; !reflect.DeepEqual(got, want) {
		t.Errorf("SecOpt = %v, want %v", got, want)
	}
	if spec.Image != "claudebox" {
		t.Errorf("Image = %q, want %q", spec.Image, "claudebox")
	}
	if spec.Command != "/bin/zsh" {
		t.Errorf("Command = %q, want %q", spec.Command, "/bin/zsh")
	}
}

func TestBuildSpec_MountListMatchesReferenceOrder(t *testing.T) {
	spec := BuildSpec(sampleOptions())

	want := []Mount{
		{Host: "/home/me/code/myproj", Container: "/workspace/myproj-abc123"},
		{Host: "/home/me/.makeslop/.claude/", Container: "/home/user/.claude/"},
		{Host: "/home/me/.makeslop/.claude.json", Container: "/home/user/.claude.json"},
		{Host: "/home/me/.makeslop/.codex/", Container: "/home/user/.codex/"},
		{Host: "/home/me/.makeslop/workspaces/myproj-abc123/.claude/", Container: "/workspace/myproj-abc123/.claude/"},
		{Host: "/home/me/.makeslop/workspaces/myproj-abc123/.codex/", Container: "/workspace/myproj-abc123/.codex/"},
		{Host: "/home/me/.makeslop/workspaces/myproj-abc123/docs/", Container: "/workspace/myproj-abc123/docs/"},
		{Host: "/home/me/.makeslop/workspaces/myproj-abc123/CLAUDE.md", Container: "/workspace/myproj-abc123/CLAUDE.md"},
	}
	if !reflect.DeepEqual(spec.Mounts, want) {
		t.Errorf("Mounts mismatch\n got: %+v\nwant: %+v", spec.Mounts, want)
	}
}

func TestSpecArgs_FullArgvForRepresentativeSpec(t *testing.T) {
	spec := BuildSpec(sampleOptions())

	want := []string{
		"run", "--rm", "-it",
		"--workdir", "/workspace/myproj-abc123",
		"--tmpfs", "/tmp:size=100m",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--mount", `type=bind,source=/home/me/code/myproj,target=/workspace/myproj-abc123`,
		"--mount", `type=bind,source=/home/me/.makeslop/.claude/,target=/home/user/.claude/`,
		"--mount", `type=bind,source=/home/me/.makeslop/.claude.json,target=/home/user/.claude.json`,
		"--mount", `type=bind,source=/home/me/.makeslop/.codex/,target=/home/user/.codex/`,
		"--mount", `type=bind,source=/home/me/.makeslop/workspaces/myproj-abc123/.claude/,target=/workspace/myproj-abc123/.claude/`,
		"--mount", `type=bind,source=/home/me/.makeslop/workspaces/myproj-abc123/.codex/,target=/workspace/myproj-abc123/.codex/`,
		"--mount", `type=bind,source=/home/me/.makeslop/workspaces/myproj-abc123/docs/,target=/workspace/myproj-abc123/docs/`,
		"--mount", `type=bind,source=/home/me/.makeslop/workspaces/myproj-abc123/CLAUDE.md,target=/workspace/myproj-abc123/CLAUDE.md`,
		"claudebox", "/bin/zsh",
	}
	if got := spec.Args(); !reflect.DeepEqual(got, want) {
		t.Errorf("Args mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

// Defensive: even though BuildSpec always populates Tmpfs/CapDrop/SecOpt, a
// caller could hand-build a Spec; Args must not emit empty flag tokens.
func TestSpecArgs_EmptyMultiValueSlicesProduceNoFlags(t *testing.T) {
	spec := Spec{
		Image:   "img",
		Command: "sh",
		Workdir: "/wd",
		Mounts:  []Mount{{Host: "/h", Container: "/c"}},
	}
	want := []string{
		"run", "--rm", "-it",
		"--workdir", "/wd",
		"--mount", `type=bind,source=/h,target=/c`,
		"img", "sh",
	}
	if got := spec.Args(); !reflect.DeepEqual(got, want) {
		t.Errorf("Args mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

// A host path containing ',' (a CSV separator) must be wrapped as a whole
// `"source=..."` field per RFC 4180. The target stays unquoted because it has
// no CSV-special characters. Pinned because docker's --mount parser rejects
// "bare \" in non-quoted-field" if the value (not the whole field) is quoted.
func TestSpecArgs_MountValuesQuoteCommaInPath(t *testing.T) {
	spec := Spec{
		Image:   "img",
		Command: "sh",
		Workdir: "/wd",
		Mounts: []Mount{
			{Host: "/path,with,commas/x", Container: "/in/container"},
		},
	}
	want := []string{
		"run", "--rm", "-it",
		"--workdir", "/wd",
		"--mount", `type=bind,"source=/path,with,commas/x",target=/in/container`,
		"img", "sh",
	}
	if got := spec.Args(); !reflect.DeepEqual(got, want) {
		t.Errorf("Args mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

// Pin the actual contract — feed each emitted --mount argument through
// encoding/csv (what docker's parser uses) and assert it yields exactly the
// three logical fields type=bind, source=<host>, target=<container>. A prior
// iteration emitted source="/path",target="/path" which RFC 4180 rejects as
// "bare \" in non-quoted-field"; this test would have caught that regression.
func TestSpecArgs_MountArgsParseAsRFC4180CSV(t *testing.T) {
	spec := BuildSpec(sampleOptions())
	args := spec.Args()

	type pair struct{ host, container string }
	want := make([]pair, 0, len(spec.Mounts))
	for _, m := range spec.Mounts {
		want = append(want, pair{m.Host, m.Container})
	}

	commaSpec := Spec{
		Image:   "img",
		Command: "sh",
		Workdir: "/wd",
		Mounts: []Mount{
			{Host: "/path,with,commas/x", Container: "/c,with,commas/y"},
			{Host: `/has"quote/x`, Container: "/plain"},
		},
	}
	commaArgs := commaSpec.Args()
	for _, m := range commaSpec.Mounts {
		want = append(want, pair{m.Host, m.Container})
	}

	mountArgs := collectMountArgs(append(args, commaArgs...))
	if len(mountArgs) != len(want) {
		t.Fatalf("collected %d --mount args, want %d", len(mountArgs), len(want))
	}
	for i, raw := range mountArgs {
		r := csv.NewReader(strings.NewReader(raw))
		rec, err := r.Read()
		if err != nil {
			t.Fatalf("csv parse failed for %q: %v", raw, err)
		}
		if len(rec) != 3 {
			t.Fatalf("csv fields = %d (%q), want 3", len(rec), rec)
		}
		if rec[0] != "type=bind" {
			t.Errorf("field[0] = %q, want type=bind", rec[0])
		}
		gotSource := strings.TrimPrefix(rec[1], "source=")
		if gotSource == rec[1] {
			t.Errorf("field[1] missing source= prefix: %q", rec[1])
		}
		gotTarget := strings.TrimPrefix(rec[2], "target=")
		if gotTarget == rec[2] {
			t.Errorf("field[2] missing target= prefix: %q", rec[2])
		}
		if gotSource != want[i].host {
			t.Errorf("source = %q, want %q", gotSource, want[i].host)
		}
		if gotTarget != want[i].container {
			t.Errorf("target = %q, want %q", gotTarget, want[i].container)
		}
	}
}

func collectMountArgs(argv []string) []string {
	out := make([]string, 0, 8)
	for i := 0; i < len(argv)-1; i++ {
		if argv[i] == "--mount" {
			out = append(out, argv[i+1])
		}
	}
	return out
}

func TestSpecArgs_MultiValueSlicesRepeatFlag(t *testing.T) {
	spec := Spec{
		Image:   "img",
		Command: "sh",
		Workdir: "/wd",
		Tmpfs:   []string{"/tmp:size=100m", "/run:size=10m"},
		CapDrop: []string{"ALL", "NET_RAW"},
		SecOpt:  []string{"no-new-privileges", "seccomp=unconfined"},
	}
	want := []string{
		"run", "--rm", "-it",
		"--workdir", "/wd",
		"--tmpfs", "/tmp:size=100m",
		"--tmpfs", "/run:size=10m",
		"--cap-drop", "ALL",
		"--cap-drop", "NET_RAW",
		"--security-opt", "no-new-privileges",
		"--security-opt", "seccomp=unconfined",
		"img", "sh",
	}
	if got := spec.Args(); !reflect.DeepEqual(got, want) {
		t.Errorf("Args mismatch\n got: %#v\nwant: %#v", got, want)
	}
}
