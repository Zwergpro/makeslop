package docker

import (
	"encoding/csv"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/mount"
)

func sampleOptions() Options {
	return Options{
		Projects:          []Project{{Host: "/home/me/code/myproj", Name: "myproj-abc123"}},
		BaseDir:           "/home/me/.makeslop",
		WorkspaceHost:     "/home/me/.makeslop/workspaces/myproj-abc123",
		Image:             "claudebox",
		Command:           "/bin/zsh",
		TmpDirSize:        "100m",
		MountAgentCache:   true,
		MountContentCache: true,
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

// A host path with a comma must wrap the whole `"source=..."` field per RFC 4180;
// quoting only the value (not the field) makes docker's --mount parser reject it.
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

// Each emitted --mount arg must parse via encoding/csv (docker's parser) into
// exactly three fields type=bind, source=<host>, target=<container>. A prior
// iteration emitted source="/path",target="/path" which RFC 4180 rejects.
// tmpfs mounts (2 fields) are covered by TestSpecArgs_TmpfsMountFlagShape.
func TestSpecArgs_MountArgsParseAsRFC4180CSV(t *testing.T) {
	spec := BuildSpec(sampleOptions())
	args := spec.Args()

	type pair struct{ host, container string }
	want := make([]pair, 0, len(spec.Mounts))
	for _, m := range spec.Mounts {
		if m.Type != "tmpfs" {
			want = append(want, pair{m.Host, m.Container})
		}
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

	allMountArgs := collectMountArgs(append(args, commaArgs...))
	var mountArgs []string
	for _, raw := range allMountArgs {
		if !strings.HasPrefix(raw, "type=tmpfs") {
			mountArgs = append(mountArgs, raw)
		}
	}

	if len(mountArgs) != len(want) {
		t.Fatalf("collected %d bind --mount args, want %d", len(mountArgs), len(want))
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

func TestBuildSpec_MaskedFilesAppendDevNullMounts(t *testing.T) {
	o := sampleOptions()
	o.Projects[0].MaskedFiles = []string{
		"/home/me/code/myproj/.env",
		"/home/me/code/myproj/configs/env/local.env",
	}
	spec := BuildSpec(o)

	n := len(spec.Mounts)
	if n < 2 {
		t.Fatalf("got %d mounts, want at least 2", n)
	}
	wantTail := []Mount{
		{Host: "/dev/null", Container: "/workspace/myproj-abc123/.env"},
		{Host: "/dev/null", Container: "/workspace/myproj-abc123/configs/env/local.env"},
	}
	gotTail := spec.Mounts[n-2:]
	if !reflect.DeepEqual(gotTail, wantTail) {
		t.Errorf("tail mounts mismatch\n got: %+v\nwant: %+v", gotTail, wantTail)
	}
}

func TestSpecArgs_MaskedFilesProduceDevNullMountArgs(t *testing.T) {
	o := sampleOptions()
	o.Projects[0].MaskedFiles = []string{
		"/home/me/code/myproj/.env",
		"/home/me/code/myproj/configs/env/local.env",
	}
	spec := BuildSpec(o)
	args := spec.Args()

	mountArgs := collectMountArgs(args)
	if len(mountArgs) < 2 {
		t.Fatalf("got %d --mount args, want at least 2", len(mountArgs))
	}
	gotTailMountVals := mountArgs[len(mountArgs)-2:]

	wantTailMountVals := []string{
		"type=bind,source=/dev/null,target=/workspace/myproj-abc123/.env",
		"type=bind,source=/dev/null,target=/workspace/myproj-abc123/configs/env/local.env",
	}
	if !reflect.DeepEqual(gotTailMountVals, wantTailMountVals) {
		t.Errorf("tail --mount args mismatch\n got: %+v\nwant: %+v", gotTailMountVals, wantTailMountVals)
	}

	lastTwo := args[len(args)-2:]
	if !reflect.DeepEqual(lastTwo, []string{"claudebox", "/bin/zsh"}) {
		t.Errorf("argv tail = %v, want [claudebox /bin/zsh]", lastTwo)
	}
}

func TestBuildSpec_MaskedDirsAppendTmpfsMounts(t *testing.T) {
	o := sampleOptions()
	o.Projects[0].MaskedDirs = []string{
		"/home/me/code/myproj/node_modules",
		"/home/me/code/myproj/secrets",
	}
	spec := BuildSpec(o)

	n := len(spec.Mounts)
	if n < 2 {
		t.Fatalf("got %d mounts, want at least 2", n)
	}
	wantTail := []Mount{
		{Type: "tmpfs", Container: "/workspace/myproj-abc123/node_modules"},
		{Type: "tmpfs", Container: "/workspace/myproj-abc123/secrets"},
	}
	gotTail := spec.Mounts[n-2:]
	if !reflect.DeepEqual(gotTail, wantTail) {
		t.Errorf("tail mounts mismatch\n got: %+v\nwant: %+v", gotTail, wantTail)
	}
}

func TestBuildSpec_MaskedFilesAndDirsInteract(t *testing.T) {
	o := sampleOptions()
	o.Projects[0].MaskedFiles = []string{"/home/me/code/myproj/.env"}
	o.Projects[0].MaskedDirs = []string{"/home/me/code/myproj/node_modules"}
	spec := BuildSpec(o)

	n := len(spec.Mounts)
	if n < 2 {
		t.Fatalf("got %d mounts, want at least 2", n)
	}
	wantTail := []Mount{
		{Host: "/dev/null", Container: "/workspace/myproj-abc123/.env"},
		{Type: "tmpfs", Container: "/workspace/myproj-abc123/node_modules"},
	}
	gotTail := spec.Mounts[n-2:]
	if !reflect.DeepEqual(gotTail, wantTail) {
		t.Errorf("tail mounts mismatch\n got: %+v\nwant: %+v", gotTail, wantTail)
	}
}

func TestSpecArgs_TmpfsMountFlagShape(t *testing.T) {
	o := sampleOptions()
	o.Projects[0].MaskedDirs = []string{"/home/me/code/myproj/node_modules"}
	spec := BuildSpec(o)
	args := spec.Args()

	mountArgs := collectMountArgs(args)
	if len(mountArgs) == 0 {
		t.Fatal("no --mount args found")
	}
	last := mountArgs[len(mountArgs)-1]
	want := "type=tmpfs,target=/workspace/myproj-abc123/node_modules"
	if last != want {
		t.Errorf("last --mount value = %q, want %q", last, want)
	}
	if strings.Contains(last, "source=") {
		t.Errorf("tmpfs mount must not contain source=, got %q", last)
	}
}

// Uses a minimal hand-built Spec (not BuildSpec) so the golden stays stable if
// BuildSpec later adds flags.
func TestShellCommand_MinimalSpec_GoldenString(t *testing.T) {
	spec := Spec{
		Image:   "claudebox",
		Command: "/bin/zsh",
		Workdir: "/workspace/myproj-abc123",
		Mounts:  []Mount{{Host: "/home/me/code/myproj", Container: "/workspace/myproj-abc123"}},
		Tmpfs:   []string{"/tmp:size=100m"},
	}
	want := "docker run \\\n" +
		"  --rm \\\n" +
		"  -it \\\n" +
		"  --workdir /workspace/myproj-abc123 \\\n" +
		"  --tmpfs /tmp:size=100m \\\n" +
		"  --mount type=bind,source=/home/me/code/myproj,target=/workspace/myproj-abc123 \\\n" +
		"  claudebox \\\n" +
		"  /bin/zsh"
	got := spec.ShellCommand()
	if got != want {
		t.Errorf("ShellCommand mismatch\ngot:\n%s\n\nwant:\n%s", got, want)
	}
	lines := strings.Split(got, "\n")
	last := lines[len(lines)-1]
	if strings.HasSuffix(last, `\`) {
		t.Errorf("final line must not have trailing backslash: %q", last)
	}
	for i, line := range lines[:len(lines)-1] {
		if !strings.HasSuffix(line, ` \`) {
			t.Errorf("line %d missing trailing backslash: %q", i, line)
		}
	}
}

func TestShellCommand_ShellQuoting(t *testing.T) {
	tests := []struct {
		name    string
		spec    Spec
		wantSub string // substring that must appear in ShellCommand output
	}{
		{
			name: "image with space is single-quoted",
			spec: Spec{
				Image:   "my image",
				Command: "sh",
				Workdir: "/wd",
			},
			wantSub: `'my image'`,
		},
		{
			name: "command with embedded single-quote uses POSIX escape",
			spec: Spec{
				Image:   "img",
				Command: "it's-a-shell",
				Workdir: "/wd",
			},
			wantSub: `'it'\''s-a-shell'`,
		},
		{
			name: "mount value with CSV-quoted double-quote triggers single-quote wrap",
			spec: Spec{
				Image:   "img",
				Command: "sh",
				Workdir: "/wd",
				Mounts:  []Mount{{Host: `/has"quote/x`, Container: "/plain"}},
			},
			// csvField doubles the embedded " to "" per RFC 4180, producing
			// type=bind,"source=/has""quote/x",target=/plain in Args().
			// shellQuote wraps it in single quotes because it contains `"`.
			wantSub: `'type=bind,"source=/has""quote/x",target=/plain'`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.spec.ShellCommand()
			if !strings.Contains(got, tc.wantSub) {
				t.Errorf("ShellCommand output does not contain %q\ngot:\n%s", tc.wantSub, got)
			}
		})
	}
}

func TestShellCommand_NilSlices_DegenerateCase(t *testing.T) {
	spec := Spec{
		Image:   "img",
		Command: "sh",
		Workdir: "/wd",
		// Mounts, Tmpfs, CapDrop, SecOpt all nil.
	}
	got := spec.ShellCommand()
	if !strings.HasPrefix(got, "docker run") {
		t.Errorf("must start with 'docker run', got: %q", got)
	}
	lines := strings.Split(got, "\n")
	if last := lines[len(lines)-1]; strings.HasSuffix(last, `\`) {
		t.Errorf("final line must not end with backslash: %q", last)
	}
	// Round-trip: parsed tokens must equal ["docker"] + Args().
	var parsed []string
	for _, raw := range lines {
		line := strings.TrimSuffix(raw, ` \`)
		for _, field := range strings.Fields(line) {
			parsed = append(parsed, shellUnquote(field))
		}
	}
	want := append([]string{"docker"}, spec.Args()...)
	if !reflect.DeepEqual(parsed, want) {
		t.Errorf("round-trip mismatch\n got: %#v\nwant: %#v", parsed, want)
	}
}

// TestShellCommand_Deterministic guards against map-iteration leaking.
func TestShellCommand_Deterministic(t *testing.T) {
	spec := BuildSpec(sampleOptions())
	first := spec.ShellCommand()
	for i := 0; i < 20; i++ {
		if got := spec.ShellCommand(); got != first {
			t.Fatalf("ShellCommand is not deterministic (iteration %d differs)", i+1)
		}
	}
}

func TestShellCommand_AgreeWithArgs(t *testing.T) {
	spec := BuildSpec(sampleOptions())
	output := spec.ShellCommand()

	var got []string
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimSuffix(raw, ` \`)
		for _, field := range strings.Fields(line) {
			got = append(got, shellUnquote(field))
		}
	}

	want := append([]string{"docker"}, spec.Args()...)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

// shellUnquote reverses shellQuote. Limitation: uses strings.Fields, so only
// works when no argv token contains embedded whitespace.
func shellUnquote(s string) string {
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		inner := s[1 : len(s)-1]
		return strings.ReplaceAll(inner, `'\''`, "'")
	}
	return s
}

func TestBuildSpec_TmpDirSize_Custom(t *testing.T) {
	o := sampleOptions()
	o.TmpDirSize = "1000m"
	spec := BuildSpec(o)

	want := []string{"/tmp:size=1000m"}
	if !reflect.DeepEqual(spec.Tmpfs, want) {
		t.Errorf("Tmpfs = %v, want %v", spec.Tmpfs, want)
	}

	args := spec.Args()
	found := false
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--tmpfs" && args[i+1] == "/tmp:size=1000m" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Args() missing --tmpfs /tmp:size=1000m; args: %v", args)
	}
}

// Renders custom tmp_dir_size via ShellCommand — the user-facing --dry-run path.
func TestShellCommand_TmpDirSize_Custom(t *testing.T) {
	o := sampleOptions()
	o.TmpDirSize = "1000m"
	spec := BuildSpec(o)
	out := spec.ShellCommand()

	if !strings.Contains(out, "--tmpfs /tmp:size=1000m") {
		t.Errorf("ShellCommand missing '--tmpfs /tmp:size=1000m':\n%s", out)
	}
}

// The config.Load default (100m) must pass through unchanged.
func TestBuildSpec_TmpDirSize_DefaultPath(t *testing.T) {
	spec := BuildSpec(sampleOptions()) // sampleOptions sets TmpDirSize "100m"

	want := []string{"/tmp:size=100m"}
	if !reflect.DeepEqual(spec.Tmpfs, want) {
		t.Errorf("Tmpfs = %v, want %v (default regression)", spec.Tmpfs, want)
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

// Default spec must produce exactly 8 mounts (no extra proxy/network mounts).
func TestBuildSpec_DefaultMountCount(t *testing.T) {
	spec := BuildSpec(sampleOptions())
	if len(spec.Mounts) != 8 {
		t.Errorf("Mounts len = %d, want 8", len(spec.Mounts))
	}
}

// Default argv must contain no --network or -e flags (bridge, no env injection).
func TestSpecArgs_DefaultArgvHasNoNetworkOrEnv(t *testing.T) {
	spec := BuildSpec(sampleOptions())
	args := spec.Args()

	for i, tok := range args {
		if tok == "--network" {
			t.Errorf("unexpected --network at index %d", i)
		}
		if tok == "-e" {
			t.Errorf("unexpected -e at index %d", i)
		}
	}
}

func TestSpecArgs_VolumeNameWithComma(t *testing.T) {
	spec := Spec{
		Image:   "img",
		Command: "sh",
		Workdir: "/wd",
		Mounts: []Mount{
			{Type: "volume", Host: "vol,with,commas", Container: "/data", ReadOnly: true},
		},
	}
	args := spec.Args()
	mountArgs := collectMountArgs(args)
	if len(mountArgs) != 1 {
		t.Fatalf("want 1 mount arg, got %d", len(mountArgs))
	}
	want := `type=volume,"source=vol,with,commas",target=/data,readonly`
	if mountArgs[0] != want {
		t.Errorf("mount value = %q, want %q", mountArgs[0], want)
	}
}

// ReadOnly: false must render byte-identically to pre-ReadOnly behavior.
func TestMount_ReadOnlyFalseRendersIdentical(t *testing.T) {
	spec := Spec{
		Image:   "img",
		Command: "sh",
		Workdir: "/wd",
		Mounts:  []Mount{{Host: "/h", Container: "/c", ReadOnly: false}},
	}
	args := spec.Args()
	mountArgs := collectMountArgs(args)
	if len(mountArgs) != 1 {
		t.Fatalf("want 1 mount arg, got %d", len(mountArgs))
	}
	want := "type=bind,source=/h,target=/c"
	if mountArgs[0] != want {
		t.Errorf("mount value = %q, want %q", mountArgs[0], want)
	}
}

func TestMount_ReadOnlyTrueAddsReadonlySuffix(t *testing.T) {
	spec := Spec{
		Image:   "img",
		Command: "sh",
		Workdir: "/wd",
		Mounts:  []Mount{{Host: "/h", Container: "/c", ReadOnly: true}},
	}
	args := spec.Args()
	mountArgs := collectMountArgs(args)
	if len(mountArgs) != 1 {
		t.Fatalf("want 1 mount arg, got %d", len(mountArgs))
	}
	want := "type=bind,source=/h,target=/c,readonly"
	if mountArgs[0] != want {
		t.Errorf("mount value = %q, want %q", mountArgs[0], want)
	}
}

func TestMount_ReadOnlyIgnoredForTmpfs(t *testing.T) {
	spec := Spec{
		Image:   "img",
		Command: "sh",
		Workdir: "/wd",
		Mounts:  []Mount{{Type: "tmpfs", Container: "/c", ReadOnly: true}},
	}
	args := spec.Args()
	mountArgs := collectMountArgs(args)
	if len(mountArgs) != 1 {
		t.Fatalf("want 1 mount arg, got %d", len(mountArgs))
	}
	want := "type=tmpfs,target=/c"
	if mountArgs[0] != want {
		t.Errorf("mount value = %q, want %q", mountArgs[0], want)
	}
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

func TestContainerConfig_ImageCmdTTYStdin(t *testing.T) {
	tests := []struct {
		name    string
		spec    Spec
		wantImg string
		wantCmd []string
		wantWd  string
	}{
		{
			name:    "minimal spec",
			spec:    Spec{Image: "claudebox", Command: "/bin/zsh", Workdir: "/workspace/foo"},
			wantImg: "claudebox",
			wantCmd: []string{"/bin/zsh"},
			wantWd:  "/workspace/foo",
		},
		{
			name:    "from BuildSpec defaults",
			spec:    BuildSpec(sampleOptions()),
			wantImg: "claudebox",
			wantCmd: []string{"/bin/zsh"},
			wantWd:  "/workspace/myproj-abc123",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.spec.ContainerConfig()
			if cfg.Image != tc.wantImg {
				t.Errorf("Image = %q, want %q", cfg.Image, tc.wantImg)
			}
			if !reflect.DeepEqual(cfg.Cmd, tc.wantCmd) {
				t.Errorf("Cmd = %v, want %v", cfg.Cmd, tc.wantCmd)
			}
			if len(cfg.Env) != 0 {
				t.Errorf("Env = %v, want nil/empty (no env injection)", cfg.Env)
			}
			if cfg.WorkingDir != tc.wantWd {
				t.Errorf("WorkingDir = %q, want %q", cfg.WorkingDir, tc.wantWd)
			}
			if !cfg.Tty {
				t.Error("Tty must be true")
			}
			if !cfg.OpenStdin {
				t.Error("OpenStdin must be true")
			}
			if !cfg.AttachStdin {
				t.Error("AttachStdin must be true")
			}
			if !cfg.AttachStdout {
				t.Error("AttachStdout must be true")
			}
			if !cfg.AttachStderr {
				t.Error("AttachStderr must be true")
			}
		})
	}
}

func TestHostConfig_AutoRemoveCapDropSecOpt(t *testing.T) {
	spec := BuildSpec(sampleOptions())
	hc := spec.HostConfig()

	if !hc.AutoRemove {
		t.Error("AutoRemove must be true (matches --rm)")
	}
	if !reflect.DeepEqual(hc.CapDrop, []string{"ALL"}) {
		t.Errorf("CapDrop = %v, want [ALL]", hc.CapDrop)
	}
	if !reflect.DeepEqual(hc.SecurityOpt, []string{"no-new-privileges"}) {
		t.Errorf("SecurityOpt = %v, want [no-new-privileges]", hc.SecurityOpt)
	}
}

// With no network settings, NetworkMode is empty (Docker default bridge).
func TestHostConfig_NetworkModeDefaultsToEmpty(t *testing.T) {
	spec := Spec{Image: "img", Command: "sh", Workdir: "/wd"}
	hc := spec.HostConfig()
	if hc.NetworkMode != "" {
		t.Errorf("NetworkMode = %q, want empty string (default bridge)", hc.NetworkMode)
	}
}

func TestHostConfig_TmpfsMapWithColon(t *testing.T) {
	spec := Spec{
		Image:   "img",
		Command: "sh",
		Workdir: "/wd",
		Tmpfs:   []string{"/tmp:size=100m"},
	}
	hc := spec.HostConfig()
	want := map[string]string{"/tmp": "size=100m"}
	if !reflect.DeepEqual(hc.Tmpfs, want) {
		t.Errorf("Tmpfs = %v, want %v", hc.Tmpfs, want)
	}
}

func TestHostConfig_TmpfsMapWithoutColon(t *testing.T) {
	spec := Spec{
		Image:   "img",
		Command: "sh",
		Workdir: "/wd",
		Tmpfs:   []string{"/tmp"},
	}
	hc := spec.HostConfig()
	want := map[string]string{"/tmp": ""}
	if !reflect.DeepEqual(hc.Tmpfs, want) {
		t.Errorf("Tmpfs = %v, want %v", hc.Tmpfs, want)
	}
}

func TestHostConfig_TmpfsMapEmpty(t *testing.T) {
	spec := Spec{Image: "img", Command: "sh", Workdir: "/wd"}
	hc := spec.HostConfig()
	if hc.Tmpfs != nil {
		t.Errorf("Tmpfs = %v, want nil for empty input", hc.Tmpfs)
	}
}

func TestHostConfig_TmpfsMapMultipleEntries(t *testing.T) {
	spec := Spec{
		Image:   "img",
		Command: "sh",
		Workdir: "/wd",
		Tmpfs:   []string{"/tmp:size=100m", "/run:size=10m", "/var/run"},
	}
	hc := spec.HostConfig()
	want := map[string]string{
		"/tmp":     "size=100m",
		"/run":     "size=10m",
		"/var/run": "",
	}
	if !reflect.DeepEqual(hc.Tmpfs, want) {
		t.Errorf("Tmpfs = %v, want %v", hc.Tmpfs, want)
	}
}

func TestHostConfig_BindMountTranslation(t *testing.T) {
	spec := Spec{
		Image:   "img",
		Command: "sh",
		Workdir: "/wd",
		Mounts: []Mount{
			{Host: "/host/path", Container: "/container/path"},
			{Host: "/host/ro", Container: "/container/ro", ReadOnly: true},
		},
	}
	hc := spec.HostConfig()
	want := []mount.Mount{
		{Type: mount.TypeBind, Source: "/host/path", Target: "/container/path", ReadOnly: false},
		{Type: mount.TypeBind, Source: "/host/ro", Target: "/container/ro", ReadOnly: true},
	}
	if !reflect.DeepEqual(hc.Mounts, want) {
		t.Errorf("Mounts = %+v, want %+v", hc.Mounts, want)
	}
}

func TestHostConfig_TmpfsMountTranslation(t *testing.T) {
	spec := Spec{
		Image:   "img",
		Command: "sh",
		Workdir: "/wd",
		Mounts: []Mount{
			{Type: "tmpfs", Container: "/workspace/secrets"},
		},
	}
	hc := spec.HostConfig()
	if len(hc.Mounts) != 1 {
		t.Fatalf("want 1 mount, got %d", len(hc.Mounts))
	}
	m := hc.Mounts[0]
	if m.Type != mount.TypeTmpfs {
		t.Errorf("Type = %q, want TypeTmpfs", m.Type)
	}
	if m.Target != "/workspace/secrets" {
		t.Errorf("Target = %q, want /workspace/secrets", m.Target)
	}
	if m.Source != "" {
		t.Errorf("Source = %q, want empty for tmpfs", m.Source)
	}
}

func TestHostConfig_DevNullMountTranslation(t *testing.T) {
	// /dev/null masked-file overlays are bind mounts with Source=/dev/null.
	spec := Spec{
		Image:   "img",
		Command: "sh",
		Workdir: "/wd",
		Mounts: []Mount{
			{Host: "/dev/null", Container: "/workspace/foo/.env"},
		},
	}
	hc := spec.HostConfig()
	if len(hc.Mounts) != 1 {
		t.Fatalf("want 1 mount, got %d", len(hc.Mounts))
	}
	m := hc.Mounts[0]
	if m.Type != mount.TypeBind {
		t.Errorf("Type = %q, want TypeBind", m.Type)
	}
	if m.Source != "/dev/null" {
		t.Errorf("Source = %q, want /dev/null", m.Source)
	}
	if m.Target != "/workspace/foo/.env" {
		t.Errorf("Target = %q, want /workspace/foo/.env", m.Target)
	}
}

func TestHostConfig_ReadOnlyPropagated(t *testing.T) {
	spec := Spec{
		Image:   "img",
		Command: "sh",
		Workdir: "/wd",
		Mounts: []Mount{
			{Host: "/h", Container: "/c", ReadOnly: true},
			{Host: "/h2", Container: "/c2", ReadOnly: false},
		},
	}
	hc := spec.HostConfig()
	if len(hc.Mounts) != 2 {
		t.Fatalf("want 2 mounts, got %d", len(hc.Mounts))
	}
	if !hc.Mounts[0].ReadOnly {
		t.Error("Mounts[0].ReadOnly must be true")
	}
	if hc.Mounts[1].ReadOnly {
		t.Error("Mounts[1].ReadOnly must be false")
	}
}

func TestHostConfig_VolumeMountTranslation(t *testing.T) {
	spec := Spec{
		Image:   "img",
		Command: "sh",
		Workdir: "/wd",
		Mounts: []Mount{
			{Type: "volume", Host: "my-vol", Container: "/mnt/data"},
			{Type: "volume", Host: "ro-vol", Container: "/mnt/ro", ReadOnly: true},
		},
	}
	hc := spec.HostConfig()
	if len(hc.Mounts) != 2 {
		t.Fatalf("want 2 mounts, got %d", len(hc.Mounts))
	}
	if hc.Mounts[0].Type != mount.TypeVolume {
		t.Errorf("Mounts[0].Type = %q, want TypeVolume", hc.Mounts[0].Type)
	}
	if hc.Mounts[0].Source != "my-vol" {
		t.Errorf("Mounts[0].Source = %q, want my-vol", hc.Mounts[0].Source)
	}
	if hc.Mounts[0].Target != "/mnt/data" {
		t.Errorf("Mounts[0].Target = %q, want /mnt/data", hc.Mounts[0].Target)
	}
	if hc.Mounts[0].ReadOnly {
		t.Error("Mounts[0].ReadOnly must be false")
	}
	if hc.Mounts[1].Type != mount.TypeVolume {
		t.Errorf("Mounts[1].Type = %q, want TypeVolume", hc.Mounts[1].Type)
	}
	if hc.Mounts[1].Source != "ro-vol" {
		t.Errorf("Mounts[1].Source = %q, want ro-vol", hc.Mounts[1].Source)
	}
	if !hc.Mounts[1].ReadOnly {
		t.Error("Mounts[1].ReadOnly must be true")
	}
}

func TestHostConfig_MixedMountTypesOrder(t *testing.T) {
	spec := Spec{
		Image:   "img",
		Command: "sh",
		Workdir: "/wd",
		Mounts: []Mount{
			{Host: "/host/proj", Container: "/workspace/proj"},
			{Type: "tmpfs", Container: "/workspace/proj/secrets"},
			{Host: "/dev/null", Container: "/workspace/proj/.env"},
		},
	}
	hc := spec.HostConfig()
	if len(hc.Mounts) != 3 {
		t.Fatalf("want 3 mounts, got %d", len(hc.Mounts))
	}
	if hc.Mounts[0].Type != mount.TypeBind {
		t.Errorf("Mounts[0].Type = %q, want TypeBind", hc.Mounts[0].Type)
	}
	if hc.Mounts[1].Type != mount.TypeTmpfs {
		t.Errorf("Mounts[1].Type = %q, want TypeTmpfs", hc.Mounts[1].Type)
	}
	if hc.Mounts[2].Type != mount.TypeBind {
		t.Errorf("Mounts[2].Type = %q, want TypeBind (/dev/null masked-file)", hc.Mounts[2].Type)
	}
}

// Catches silent drift: the argv projection (Args) and the SDK-struct projection
// (ContainerConfig/HostConfig) must agree on every load-bearing field for a Spec
// exercising masked files/dirs, env injection, and default security/network.
func TestDriftGuard_ArgsAndSDKProjectionsAgree(t *testing.T) {
	o := sampleOptions()
	o.Projects[0].MaskedFiles = []string{
		"/home/me/code/myproj/.env",
		"/home/me/code/myproj/configs/secret.yaml",
	}
	o.Projects[0].MaskedDirs = []string{"/home/me/code/myproj/node_modules"}
	o.Env = []string{"DEBUG=true", "PORT=8080"}

	spec := BuildSpec(o)
	args := spec.Args()
	cfg := spec.ContainerConfig()
	hc := spec.HostConfig()

	// image: Args() second-to-last element is the image name.
	if len(args) < 2 {
		t.Fatal("args too short")
	}
	argsImage := args[len(args)-2]
	if argsImage != cfg.Image {
		t.Errorf("image: Args=%q, ContainerConfig=%q", argsImage, cfg.Image)
	}

	argsCmd := args[len(args)-1]
	if len(cfg.Cmd) != 1 || cfg.Cmd[0] != argsCmd {
		t.Errorf("cmd: Args=%q, ContainerConfig.Cmd=%v", argsCmd, cfg.Cmd)
	}

	argsWorkdir := ""
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--workdir" {
			argsWorkdir = args[i+1]
		}
	}
	if argsWorkdir != cfg.WorkingDir {
		t.Errorf("workdir: Args=%q, ContainerConfig=%q", argsWorkdir, cfg.WorkingDir)
	}

	// env: -e values from Args must equal ContainerConfig.Env.
	var argsEnv []string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-e" {
			argsEnv = append(argsEnv, args[i+1])
		}
	}
	if !reflect.DeepEqual(argsEnv, cfg.Env) {
		t.Errorf("env: Args(-e values)=%v, ContainerConfig.Env=%v", argsEnv, cfg.Env)
	}

	var argsCaps []string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--cap-drop" {
			argsCaps = append(argsCaps, args[i+1])
		}
	}
	if !reflect.DeepEqual(argsCaps, hc.CapDrop) {
		t.Errorf("cap-drop: Args=%v, HostConfig=%v", argsCaps, hc.CapDrop)
	}

	var argsSecOpt []string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--security-opt" {
			argsSecOpt = append(argsSecOpt, args[i+1])
		}
	}
	if !reflect.DeepEqual(argsSecOpt, hc.SecurityOpt) {
		t.Errorf("security-opt: Args=%v, HostConfig=%v", argsSecOpt, hc.SecurityOpt)
	}

	// network: no --network flag (default bridge).
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--network" {
			t.Errorf("unexpected --network at index %d (default bridge networking expected)", i)
		}
	}
	if hc.NetworkMode != "" {
		t.Errorf("HostConfig.NetworkMode = %q, want empty string (default bridge)", hc.NetworkMode)
	}

	// mounts: bind/volume/tmpfs counts in Args must equal HostConfig.
	argsMounts := collectMountArgs(args)
	var argsBindCount, argsTmpfsCount, argsVolumeCount int
	for _, raw := range argsMounts {
		switch {
		case strings.HasPrefix(raw, "type=tmpfs"):
			argsTmpfsCount++
		case strings.HasPrefix(raw, "type=volume"):
			argsVolumeCount++
		default:
			argsBindCount++
		}
	}
	var hcBindCount, hcTmpfsCount, hcVolumeCount int
	for _, m := range hc.Mounts {
		switch m.Type {
		case mount.TypeBind:
			hcBindCount++
		case mount.TypeTmpfs:
			hcTmpfsCount++
		case mount.TypeVolume:
			hcVolumeCount++
		}
	}
	if argsBindCount != hcBindCount {
		t.Errorf("bind mount count: Args=%d, HostConfig=%d", argsBindCount, hcBindCount)
	}
	if argsTmpfsCount != hcTmpfsCount {
		t.Errorf("tmpfs mount count: Args=%d, HostConfig=%d", argsTmpfsCount, hcTmpfsCount)
	}
	if argsVolumeCount != hcVolumeCount {
		t.Errorf("volume mount count: Args=%d, HostConfig=%d", argsVolumeCount, hcVolumeCount)
	}

	// AutoRemove: --rm in args <=> HostConfig.AutoRemove.
	argsHasRM := false
	for _, a := range args {
		if a == "--rm" {
			argsHasRM = true
			break
		}
	}
	if argsHasRM != hc.AutoRemove {
		t.Errorf("--rm/AutoRemove mismatch: Args.hasRM=%v, HostConfig.AutoRemove=%v", argsHasRM, hc.AutoRemove)
	}
}

// All 4 MountAgentCache/MountContentCache combos: per-workspace mounts toggle,
// but global mounts (BaseDir/.claude/, .claude.json, .codex/) are always present.
func TestBuildSpec_CacheMountCombos(t *testing.T) {
	base := "/home/me/.makeslop"
	ws := "/home/me/.makeslop/workspaces/myproj-abc123"
	wcp := "/workspace/myproj-abc123"

	globalMounts := []Mount{
		{Host: "/home/me/code/myproj", Container: wcp},
		{Host: base + "/.claude/", Container: "/home/user/.claude/"},
		{Host: base + "/.claude.json", Container: "/home/user/.claude.json"},
		{Host: base + "/.codex/", Container: "/home/user/.codex/"},
	}

	agentMounts := []Mount{
		{Host: ws + "/.claude/", Container: wcp + "/.claude/"},
		{Host: ws + "/.codex/", Container: wcp + "/.codex/"},
	}

	contentMounts := []Mount{
		{Host: ws + "/docs/", Container: wcp + "/docs/"},
		{Host: ws + "/CLAUDE.md", Container: wcp + "/CLAUDE.md"},
	}

	tests := []struct {
		name              string
		mountAgentCache   bool
		mountContentCache bool
		wantMounts        []Mount
	}{
		{
			name:              "both true — full mount set (current default behavior)",
			mountAgentCache:   true,
			mountContentCache: true,
			wantMounts:        append(append(globalMounts, agentMounts...), contentMounts...),
		},
		{
			name:              "agent off, content on — no per-workspace .claude/.codex",
			mountAgentCache:   false,
			mountContentCache: true,
			wantMounts:        append(globalMounts, contentMounts...),
		},
		{
			name:              "agent on, content off — no per-workspace docs/CLAUDE.md",
			mountAgentCache:   true,
			mountContentCache: false,
			wantMounts:        append(globalMounts, agentMounts...),
		},
		{
			name:              "both off — global mounts only (--global-only mode)",
			mountAgentCache:   false,
			mountContentCache: false,
			wantMounts:        globalMounts,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := Options{
				Projects:          []Project{{Host: "/home/me/code/myproj", Name: "myproj-abc123"}},
				BaseDir:           "/home/me/.makeslop",
				WorkspaceHost:     "/home/me/.makeslop/workspaces/myproj-abc123",
				Image:             "claudebox",
				Command:           "/bin/zsh",
				TmpDirSize:        "100m",
				MountAgentCache:   tc.mountAgentCache,
				MountContentCache: tc.mountContentCache,
			}
			spec := BuildSpec(o)
			if !reflect.DeepEqual(spec.Mounts, tc.wantMounts) {
				t.Errorf("Mounts mismatch for %s\n got: %+v\nwant: %+v",
					tc.name, spec.Mounts, tc.wantMounts)
			}
		})
	}
}

// Disabled cache groups must drop the corresponding paths from Args() output.
func TestBuildSpec_CacheMountCombos_Args(t *testing.T) {
	base := "/home/me/.makeslop"

	tests := []struct {
		name              string
		mountAgentCache   bool
		mountContentCache bool
		absent            []string // substrings that must NOT appear in any --mount arg
		present           []string // substrings that must appear in at least one --mount arg
	}{
		{
			name:              "agent off — workspace .claude and .codex absent",
			mountAgentCache:   false,
			mountContentCache: true,
			absent:            []string{"workspaces/myproj-abc123/.claude", "workspaces/myproj-abc123/.codex"},
			present:           []string{base + "/.claude/", base + "/.codex/", "docs/", "CLAUDE.md"},
		},
		{
			name:              "content off — workspace docs and CLAUDE.md absent",
			mountAgentCache:   true,
			mountContentCache: false,
			absent:            []string{"workspaces/myproj-abc123/docs", "workspaces/myproj-abc123/CLAUDE.md"},
			present:           []string{base + "/.claude/", base + "/.codex/", "workspaces/myproj-abc123/.claude", "workspaces/myproj-abc123/.codex"},
		},
		{
			name:              "both off — only global paths present",
			mountAgentCache:   false,
			mountContentCache: false,
			absent:            []string{"workspaces/myproj-abc123/.claude", "workspaces/myproj-abc123/.codex", "workspaces/myproj-abc123/docs", "workspaces/myproj-abc123/CLAUDE.md"},
			present:           []string{base + "/.claude/", base + "/.codex/"},
		},
		{
			name:              "both on — all per-workspace paths present",
			mountAgentCache:   true,
			mountContentCache: true,
			absent:            nil,
			present: []string{
				base + "/.claude/", base + "/.codex/",
				"workspaces/myproj-abc123/.claude", "workspaces/myproj-abc123/.codex",
				"docs/", "CLAUDE.md",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := Options{
				Projects:          []Project{{Host: "/home/me/code/myproj", Name: "myproj-abc123"}},
				BaseDir:           "/home/me/.makeslop",
				WorkspaceHost:     "/home/me/.makeslop/workspaces/myproj-abc123",
				Image:             "claudebox",
				Command:           "/bin/zsh",
				TmpDirSize:        "100m",
				MountAgentCache:   tc.mountAgentCache,
				MountContentCache: tc.mountContentCache,
			}
			spec := BuildSpec(o)
			args := spec.Args()

			mountArgs := collectMountArgs(args)
			allMountStr := strings.Join(mountArgs, " ")

			for _, sub := range tc.absent {
				if strings.Contains(allMountStr, sub) {
					t.Errorf("--mount args should NOT contain %q but do:\n%v", sub, mountArgs)
				}
			}
			for _, sub := range tc.present {
				if !strings.Contains(allMountStr, sub) {
					t.Errorf("--mount args should contain %q but don't:\n%v", sub, mountArgs)
				}
			}
		})
	}
}

// -e flags must appear after all --security-opt and before the first --mount.
func TestArgs_EnvFlagsEmittedAfterSecOptBeforeMounts(t *testing.T) {
	o := sampleOptions()
	o.Env = []string{"NODE_ENV=production", "PORT=3000"}
	spec := BuildSpec(o)
	args := spec.Args()

	lastSecOpt := -1
	firstEnv := -1
	firstMount := -1
	for i, a := range args {
		switch a {
		case "--security-opt":
			lastSecOpt = i
		case "-e":
			if firstEnv == -1 {
				firstEnv = i
			}
		case "--mount":
			if firstMount == -1 {
				firstMount = i
			}
		}
	}

	if firstEnv == -1 {
		t.Fatal("-e flag not found in Args()")
	}
	// Assert SecOpt present so the boundary check below is not vacuous.
	if lastSecOpt == -1 {
		t.Fatal("--security-opt not found in Args(); boundary check would be vacuous")
	}
	if firstEnv <= lastSecOpt {
		t.Errorf("-e at %d appears before or at last --security-opt at %d; want after", firstEnv, lastSecOpt)
	}
	// Assert a mount present so the boundary check below is not vacuous.
	if firstMount == -1 {
		t.Fatal("--mount not found in Args(); boundary check would be vacuous")
	}
	if firstEnv >= firstMount {
		t.Errorf("-e at %d appears at or after first --mount at %d; want before", firstEnv, firstMount)
	}

	var got []string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-e" {
			got = append(got, args[i+1])
		}
	}
	if !reflect.DeepEqual(got, o.Env) {
		t.Errorf("-e values: got %v, want %v", got, o.Env)
	}
}

func TestShellCommand_EnvLinesRendered(t *testing.T) {
	spec := Spec{
		Image:   "img",
		Command: "sh",
		Workdir: "/wd",
		Env:     []string{"FOO=bar", "BAZ=qux"},
		CapDrop: []string{"ALL"},
		SecOpt:  []string{"no-new-privileges"},
		Tmpfs:   []string{"/tmp:size=100m"},
	}
	out := spec.ShellCommand()
	if !strings.Contains(out, "-e FOO=bar") {
		t.Errorf("ShellCommand() missing '-e FOO=bar'; got:\n%s", out)
	}
	if !strings.Contains(out, "-e BAZ=qux") {
		t.Errorf("ShellCommand() missing '-e BAZ=qux'; got:\n%s", out)
	}
}

// Host values pass verbatim, so a value may hold a newline and a single quote
// (PEM keys, tokens). ShellCommand must render it as one quoted token that a
// POSIX shell reads back as the exact value, and Args/ContainerConfig must
// carry it unchanged.
func TestEnv_NewlineAndQuoteValue_AllProjectionsAgree(t *testing.T) {
	const pair = "K=line1\nline2'x"
	o := sampleOptions()
	o.Env = []string{pair}
	spec := BuildSpec(o)

	var argsEnv []string
	args := spec.Args()
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-e" {
			argsEnv = append(argsEnv, args[i+1])
		}
	}
	if !reflect.DeepEqual(argsEnv, []string{pair}) {
		t.Errorf("Args -e values = %q, want [%q]", argsEnv, pair)
	}
	if !reflect.DeepEqual(spec.ContainerConfig().Env, []string{pair}) {
		t.Errorf("ContainerConfig().Env = %q, want [%q]", spec.ContainerConfig().Env, pair)
	}

	wantTok := `'K=line1` + "\n" + `line2'\''x'`
	if !strings.Contains(spec.ShellCommand(), "  -e "+wantTok+" \\\n") {
		t.Errorf("ShellCommand() missing single quoted token %q; got:\n%s", wantTok, spec.ShellCommand())
	}
}

func TestContainerConfig_EnvPropagated(t *testing.T) {
	spec := Spec{
		Image:   "img",
		Command: "sh",
		Workdir: "/wd",
		Env:     []string{"A=1", "B=2"},
	}
	cfg := spec.ContainerConfig()
	if !reflect.DeepEqual(cfg.Env, spec.Env) {
		t.Errorf("ContainerConfig().Env = %v, want %v", cfg.Env, spec.Env)
	}
}

// Empty Env must emit no -e flags and produce output byte-identical to nil Env
// (backward compatibility).
func TestArgs_EmptyEnv_NoEFlag(t *testing.T) {
	oNil := sampleOptions() // Env nil by default
	oEmpty := sampleOptions()
	oEmpty.Env = []string{}

	argsNil := BuildSpec(oNil).Args()
	argsEmpty := BuildSpec(oEmpty).Args()

	for i, a := range argsNil {
		if a == "-e" {
			t.Errorf("nil Env: unexpected -e at index %d", i)
		}
	}
	for i, a := range argsEmpty {
		if a == "-e" {
			t.Errorf("empty Env: unexpected -e at index %d", i)
		}
	}
	if !reflect.DeepEqual(argsNil, argsEmpty) {
		t.Errorf("nil Env vs empty Env produce different Args():\nnil:   %v\nempty: %v", argsNil, argsEmpty)
	}
}

func TestBuildSpec_EnvDeterminism(t *testing.T) {
	o := sampleOptions()
	o.Env = []string{"LOG_LEVEL=debug", "NODE_ENV=test", "PORT=8080"}

	spec1 := BuildSpec(o)
	spec2 := BuildSpec(o)

	if !reflect.DeepEqual(spec1.Env, spec2.Env) {
		t.Errorf("non-deterministic Env: first=%v, second=%v", spec1.Env, spec2.Env)
	}
	if !reflect.DeepEqual(spec1.Args(), spec2.Args()) {
		t.Errorf("non-deterministic Args(): first=%v, second=%v", spec1.Args(), spec2.Args())
	}
}

// ---- ProtectProjectConfig and MaskGitHooks tests ----

// Both flags off: no sandbox-policy mounts appended. The baseline count is
// pinned to an explicit expected value (4 base + 2 agent-cache + 2 content-cache
// from sampleOptions) so that a silent change to sampleOptions defaults causes
// this test to fail loudly rather than masking the regression.
func TestBuildSpec_SandboxFlags_BothOff(t *testing.T) {
	o := sampleOptions()
	// flags default to false
	specOff := BuildSpec(o)

	// sampleOptions has MountAgentCache=true and MountContentCache=true:
	//   4 base mounts + 2 agent-cache + 2 content-cache = 8.
	const wantBaseline = 8
	if got := len(specOff.Mounts); got != wantBaseline {
		t.Fatalf("baseline mount count = %d, want %d (4 base + 2 agent-cache + 2 content-cache); sampleOptions may have changed", got, wantBaseline)
	}

	// Enabling each flag individually must increase the mount count by exactly 1.
	oProtect := sampleOptions()
	oProtect.Projects[0].ProtectConfig = true
	if got, want := len(BuildSpec(oProtect).Mounts), wantBaseline+1; got != want {
		t.Errorf("ProtectProjectConfig=true: mount count = %d, want %d (baseline+1)", got, want)
	}

	oHooks := sampleOptions()
	oHooks.Projects[0].MaskGitHooks = true
	if got, want := len(BuildSpec(oHooks).Mounts), wantBaseline+1; got != want {
		t.Errorf("MaskGitHooks=true: mount count = %d, want %d (baseline+1)", got, want)
	}

	// Both on: exactly two extra mounts.
	oBoth := sampleOptions()
	oBoth.Projects[0].ProtectConfig = true
	oBoth.Projects[0].MaskGitHooks = true
	if got, want := len(BuildSpec(oBoth).Mounts), wantBaseline+2; got != want {
		t.Errorf("both flags on: mount count = %d, want %d (baseline+2)", got, want)
	}
}

// ProtectProjectConfig=true: .makeslop.yaml read-only bind is present at mounts[4]
// (after the 4 base mounts and before any cache overlays).
func TestBuildSpec_ProtectProjectConfig_MountPresent(t *testing.T) {
	o := sampleOptions()
	o.Projects[0].ProtectConfig = true
	spec := BuildSpec(o)

	wantMount := Mount{
		Host:      "/home/me/code/myproj/.makeslop.yaml",
		Container: "/workspace/myproj-abc123/.makeslop.yaml",
		ReadOnly:  true,
	}

	if len(spec.Mounts) < 5 {
		t.Fatalf("expected at least 5 mounts, got %d", len(spec.Mounts))
	}
	// Must appear at index 4 — after the 4 base mounts, before cache overlays.
	if spec.Mounts[4] != wantMount {
		t.Errorf("mounts[4] = %+v, want %+v", spec.Mounts[4], wantMount)
	}
}

// ProtectProjectConfig=true with .makeslop.yaml in MaskedFiles (e.g. a broad
// scan pattern like "*.yaml"): the /dev/null mask for the config file must be
// dropped — docker applies mounts last-write-wins, so a mask emitted after the
// read-only self-bind would silently override the protection. Other masked
// files are unaffected, and with the flag off the mask is emitted normally.
func TestBuildSpec_ProtectProjectConfig_DropsConfigMask(t *testing.T) {
	configHost := "/home/me/code/myproj/.makeslop.yaml"
	configTarget := "/workspace/myproj-abc123/.makeslop.yaml"
	otherHost := "/home/me/code/myproj/.env"

	o := sampleOptions()
	o.Projects[0].ProtectConfig = true
	o.Projects[0].MaskedFiles = []string{otherHost, configHost}
	spec := BuildSpec(o)

	var sawConfigMask, sawOtherMask, sawReadOnlyBind bool
	for _, m := range spec.Mounts {
		switch {
		case m.Host == "/dev/null" && m.Container == configTarget:
			sawConfigMask = true
		case m.Host == "/dev/null" && m.Container == "/workspace/myproj-abc123/.env":
			sawOtherMask = true
		case m.Host == configHost && m.ReadOnly:
			sawReadOnlyBind = true
		}
	}
	if sawConfigMask {
		t.Errorf("/dev/null mask for .makeslop.yaml must be dropped when ProtectProjectConfig is set; mounts: %+v", spec.Mounts)
	}
	if !sawOtherMask {
		t.Errorf("other masked files must still be emitted; mounts: %+v", spec.Mounts)
	}
	if !sawReadOnlyBind {
		t.Errorf("read-only config self-bind missing; mounts: %+v", spec.Mounts)
	}

	// Flag off: the mask is emitted as usual.
	o = sampleOptions()
	o.Projects[0].MaskedFiles = []string{configHost}
	spec = BuildSpec(o)
	found := false
	for _, m := range spec.Mounts {
		if m.Host == "/dev/null" && m.Container == configTarget {
			found = true
		}
	}
	if !found {
		t.Errorf("config mask must be emitted when ProtectProjectConfig is off; mounts: %+v", spec.Mounts)
	}
}

// MaskGitHooks=true: .git/hooks tmpfs mount is present at a fixed position.
func TestBuildSpec_MaskGitHooks_MountPresent(t *testing.T) {
	o := sampleOptions()
	o.Projects[0].MaskGitHooks = true
	spec := BuildSpec(o)

	wantMount := Mount{
		Type:      "tmpfs",
		Container: "/workspace/myproj-abc123/.git/hooks",
	}

	if len(spec.Mounts) < 5 {
		t.Fatalf("expected at least 5 mounts, got %d", len(spec.Mounts))
	}
	// When only MaskGitHooks is set (ProtectProjectConfig=false), it appears at mounts[4].
	if spec.Mounts[4] != wantMount {
		t.Errorf("mounts[4] = %+v, want %+v", spec.Mounts[4], wantMount)
	}
}

// Both flags on: .makeslop.yaml bind is at mounts[4], .git/hooks tmpfs at mounts[5],
// both before cache overlays.
func TestBuildSpec_BothSandboxFlags_Order(t *testing.T) {
	o := sampleOptions()
	o.Projects[0].ProtectConfig = true
	o.Projects[0].MaskGitHooks = true
	spec := BuildSpec(o)

	if len(spec.Mounts) < 6 {
		t.Fatalf("expected at least 6 mounts, got %d", len(spec.Mounts))
	}

	wantConfig := Mount{
		Host:      "/home/me/code/myproj/.makeslop.yaml",
		Container: "/workspace/myproj-abc123/.makeslop.yaml",
		ReadOnly:  true,
	}
	wantHooks := Mount{
		Type:      "tmpfs",
		Container: "/workspace/myproj-abc123/.git/hooks",
	}

	// mounts[4] = .makeslop.yaml bind
	if spec.Mounts[4] != wantConfig {
		t.Errorf("mounts[4] = %+v, want %+v", spec.Mounts[4], wantConfig)
	}
	// mounts[5] = .git/hooks tmpfs
	if spec.Mounts[5] != wantHooks {
		t.Errorf("mounts[5] = %+v, want %+v", spec.Mounts[5], wantHooks)
	}

	// Both appear before any cache-overlay mounts (mounts from sampleOptions cache are
	// the agent/content overlays; with both sandbox flags they'd start at index 6).
	// Assert mounts[6] is a cache mount (host contains "workspaces/") and that no
	// sandbox mount leaks into the cache range.
	if len(spec.Mounts) < 7 {
		t.Fatalf("expected at least 7 mounts (4 base + 2 sandbox + ≥1 cache), got %d", len(spec.Mounts))
	}
	if !strings.Contains(spec.Mounts[6].Host, "workspaces/") {
		t.Errorf("mounts[6] expected to be first cache-overlay mount (host containing 'workspaces/'), got %+v", spec.Mounts[6])
	}
	for i := 6; i < len(spec.Mounts); i++ {
		m := spec.Mounts[i]
		if m.Host == "/home/me/code/myproj/.makeslop.yaml" || (m.Type == "tmpfs" && m.Container == "/workspace/myproj-abc123/.git/hooks") {
			t.Errorf("sandbox mount found at index %d (expected before index 6)", i)
		}
	}
}

// ProtectProjectConfig: readonly=true must render as ",readonly" in Args() output.
func TestArgs_ProtectProjectConfig_ReadonlySuffix(t *testing.T) {
	o := sampleOptions()
	o.Projects[0].ProtectConfig = true
	spec := BuildSpec(o)
	args := spec.Args()

	mountArgs := collectMountArgs(args)
	found := false
	for _, raw := range mountArgs {
		if strings.Contains(raw, ".makeslop.yaml") {
			found = true
			want := "type=bind,source=/home/me/code/myproj/.makeslop.yaml,target=/workspace/myproj-abc123/.makeslop.yaml,readonly"
			if raw != want {
				t.Errorf(".makeslop.yaml mount arg = %q, want %q", raw, want)
			}
		}
	}
	if !found {
		t.Error(".makeslop.yaml mount not found in Args() output")
	}
}

// MaskGitHooks: tmpfs mount for .git/hooks must render correctly in Args().
func TestArgs_MaskGitHooks_TmpfsMountShape(t *testing.T) {
	o := sampleOptions()
	o.Projects[0].MaskGitHooks = true
	spec := BuildSpec(o)
	args := spec.Args()

	mountArgs := collectMountArgs(args)
	found := false
	for _, raw := range mountArgs {
		if strings.Contains(raw, ".git/hooks") {
			found = true
			want := "type=tmpfs,target=/workspace/myproj-abc123/.git/hooks"
			if raw != want {
				t.Errorf(".git/hooks mount arg = %q, want %q", raw, want)
			}
		}
	}
	if !found {
		t.Error(".git/hooks mount not found in Args() output")
	}
}

// ProtectProjectConfig mount must appear after the base mounts (mounts[0]) and
// before the first cache overlay mount.
func TestBuildSpec_ProtectProjectConfig_PositionAfterBase_BeforeCache(t *testing.T) {
	o := sampleOptions() // both cache flags true
	o.Projects[0].ProtectConfig = true
	spec := BuildSpec(o)

	// Find the index of the .makeslop.yaml mount.
	sandboxIdx := -1
	for i, m := range spec.Mounts {
		if m.Container == "/workspace/myproj-abc123/.makeslop.yaml" {
			sandboxIdx = i
			break
		}
	}
	if sandboxIdx == -1 {
		t.Fatal(".makeslop.yaml mount not found")
	}
	// Must be after index 3 (last of 4 base mounts).
	if sandboxIdx <= 3 {
		t.Errorf("sandbox mount at index %d, want > 3 (after base mounts)", sandboxIdx)
	}
	// Must be before cache overlay mounts. Rather than asserting a fixed index,
	// we search for the first per-workspace cache overlay and verify the sandbox
	// mount appears before it — tolerant of mount ordering changes.
	firstCacheIdx := -1
	for i, m := range spec.Mounts {
		if strings.Contains(m.Host, "workspaces/") {
			firstCacheIdx = i
			break
		}
	}
	if firstCacheIdx != -1 && sandboxIdx >= firstCacheIdx {
		t.Errorf("sandbox mount at index %d is not before first cache mount at index %d", sandboxIdx, firstCacheIdx)
	}
}

// MaskGitHooks: verify the HostConfig translation produces a proper tmpfs mount.
func TestHostConfig_MaskGitHooks_TmpfsMount(t *testing.T) {
	o := sampleOptions()
	o.Projects[0].MaskGitHooks = true
	spec := BuildSpec(o)
	hc := spec.HostConfig()

	found := false
	for _, m := range hc.Mounts {
		if m.Target == "/workspace/myproj-abc123/.git/hooks" {
			found = true
			if m.Type != "tmpfs" {
				t.Errorf("Type = %q, want tmpfs", m.Type)
			}
			if m.Source != "" {
				t.Errorf("Source = %q, want empty for tmpfs", m.Source)
			}
		}
	}
	if !found {
		t.Error(".git/hooks mount not found in HostConfig().Mounts")
	}
}

// ProtectProjectConfig: verify the HostConfig translation produces a read-only bind mount.
func TestHostConfig_ProtectProjectConfig_ReadOnlyBind(t *testing.T) {
	o := sampleOptions()
	o.Projects[0].ProtectConfig = true
	spec := BuildSpec(o)
	hc := spec.HostConfig()

	found := false
	for _, m := range hc.Mounts {
		if m.Target == "/workspace/myproj-abc123/.makeslop.yaml" {
			found = true
			if m.Type != "bind" {
				t.Errorf("Type = %q, want bind", m.Type)
			}
			if m.Source != "/home/me/code/myproj/.makeslop.yaml" {
				t.Errorf("Source = %q, want /home/me/code/myproj/.makeslop.yaml", m.Source)
			}
			if !m.ReadOnly {
				t.Error("ReadOnly must be true for .makeslop.yaml mount")
			}
		}
	}
	if !found {
		t.Error(".makeslop.yaml mount not found in HostConfig().Mounts")
	}
}

// Drift-guard: sandbox flags × cache combos — Args() and HostConfig() mount counts agree.
func TestDriftGuard_SandboxFlags(t *testing.T) {
	combos := []struct {
		name                 string
		protectProjectConfig bool
		maskGitHooks         bool
	}{
		{"neither", false, false},
		{"protect_only", true, false},
		{"hooks_only", false, true},
		{"both", true, true},
	}

	for _, c := range combos {
		t.Run(c.name, func(t *testing.T) {
			o := sampleOptions()
			o.Projects[0].ProtectConfig = c.protectProjectConfig
			o.Projects[0].MaskGitHooks = c.maskGitHooks

			spec := BuildSpec(o)
			args := spec.Args()
			hc := spec.HostConfig()

			argsMounts := collectMountArgs(args)
			if len(argsMounts) != len(hc.Mounts) {
				t.Errorf("total mount count: Args=%d, HostConfig=%d (combo: %s)",
					len(argsMounts), len(hc.Mounts), c.name)
			}

			// Bind count parity.
			var argsBindCount int
			for _, raw := range argsMounts {
				if !strings.HasPrefix(raw, "type=tmpfs") && !strings.HasPrefix(raw, "type=volume") {
					argsBindCount++
				}
			}
			var hcBindCount int
			for _, m := range hc.Mounts {
				if m.Type == "bind" || m.Type == "" {
					hcBindCount++
				}
			}
			if argsBindCount != hcBindCount {
				t.Errorf("bind count: Args=%d, HostConfig=%d (combo: %s)", argsBindCount, hcBindCount, c.name)
			}

			// Tmpfs count parity.
			var argsTmpfsCount int
			for _, raw := range argsMounts {
				if strings.HasPrefix(raw, "type=tmpfs") {
					argsTmpfsCount++
				}
			}
			var hcTmpfsCount int
			for _, m := range hc.Mounts {
				if m.Type == "tmpfs" {
					hcTmpfsCount++
				}
			}
			if argsTmpfsCount != hcTmpfsCount {
				t.Errorf("tmpfs count: Args=%d, HostConfig=%d (combo: %s)", argsTmpfsCount, hcTmpfsCount, c.name)
			}
		})
	}
}

// Drift-guard across all 4 cache combos: Args() and HostConfig() mount counts must agree.
func TestDriftGuard_CacheMountCombos(t *testing.T) {
	combos := []struct {
		name              string
		mountAgentCache   bool
		mountContentCache bool
	}{
		{"both_true", true, true},
		{"agent_only", true, false},
		{"content_only", false, true},
		{"both_false", false, false},
	}

	for _, c := range combos {
		t.Run(c.name, func(t *testing.T) {
			o := sampleOptions()
			o.MountAgentCache = c.mountAgentCache
			o.MountContentCache = c.mountContentCache

			spec := BuildSpec(o)
			args := spec.Args()
			hc := spec.HostConfig()

			argsMounts := collectMountArgs(args)
			var argsBindCount int
			for _, raw := range argsMounts {
				if !strings.HasPrefix(raw, "type=tmpfs") && !strings.HasPrefix(raw, "type=volume") {
					argsBindCount++
				}
			}
			var hcBindCount int
			for _, m := range hc.Mounts {
				if m.Type == "bind" || m.Type == "" {
					hcBindCount++
				}
			}
			if argsBindCount != hcBindCount {
				t.Errorf("bind mount count: Args=%d, HostConfig=%d (combo: %s)",
					argsBindCount, hcBindCount, c.name)
			}

			if len(argsMounts) != len(hc.Mounts) {
				t.Errorf("total mount count: Args=%d, HostConfig=%d (combo: %s)",
					len(argsMounts), len(hc.Mounts), c.name)
			}
		})
	}
}

func TestBuildSpec_Network(t *testing.T) {
	cases := []struct {
		name        string
		mode        string
		networks    []string
		wantFlags   []string // --network values in Args() order
		wantHCMode  string
		wantEPNames []string // nil → NetworkingConfig must be nil
	}{
		{name: "unset"},
		{name: "container_proxy", mode: "container:proxy",
			wantFlags: []string{"container:proxy"}, wantHCMode: "container:proxy"},
		{name: "host", mode: "host", wantFlags: []string{"host"}, wantHCMode: "host"},
		{name: "single_network", networks: []string{"myapp_default"},
			wantFlags: []string{"myapp_default"}, wantHCMode: "myapp_default",
			wantEPNames: []string{"myapp_default"}},
		{name: "multiple_ordered", networks: []string{"b_net", "a_net", "c_net"},
			wantFlags: []string{"b_net", "a_net", "c_net"}, wantHCMode: "b_net",
			wantEPNames: []string{"b_net", "a_net", "c_net"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := sampleOptions()
			o.NetworkMode = c.mode
			o.Networks = c.networks
			spec := BuildSpec(o)

			if spec.NetworkMode != c.mode {
				t.Errorf("Spec.NetworkMode = %q, want %q", spec.NetworkMode, c.mode)
			}
			if !reflect.DeepEqual(spec.Networks, c.networks) {
				t.Errorf("Spec.Networks = %v, want %v", spec.Networks, c.networks)
			}
			if got := collectFlagValues(spec.Args(), "--network"); !reflect.DeepEqual(got, c.wantFlags) {
				t.Errorf("--network values = %v, want %v", got, c.wantFlags)
			}
			if got := string(spec.HostConfig().NetworkMode); got != c.wantHCMode {
				t.Errorf("HostConfig.NetworkMode = %q, want %q", got, c.wantHCMode)
			}
			nc := spec.NetworkingConfig()
			if c.wantEPNames == nil {
				if nc != nil {
					t.Errorf("NetworkingConfig = %+v, want nil", nc)
				}
				return
			}
			if nc == nil {
				t.Fatal("NetworkingConfig = nil, want endpoints")
			}
			if len(nc.EndpointsConfig) != len(c.wantEPNames) {
				t.Errorf("EndpointsConfig has %d entries, want %d", len(nc.EndpointsConfig), len(c.wantEPNames))
			}
			for _, n := range c.wantEPNames {
				if ep, ok := nc.EndpointsConfig[n]; !ok || ep == nil {
					t.Errorf("EndpointsConfig missing non-nil entry for %q", n)
				}
			}
		})
	}
}

// --network is emitted after --security-opt and before -e / --mount.
func TestArgs_NetworkFlagPosition(t *testing.T) {
	o := sampleOptions()
	o.NetworkMode = "container:proxy"
	o.Env = []string{"A=1"}
	args := BuildSpec(o).Args()
	idx := func(flag string) int {
		for i, a := range args {
			if a == flag {
				return i
			}
		}
		return -1
	}
	sec, net, env, mnt := idx("--security-opt"), idx("--network"), idx("-e"), idx("--mount")
	if sec < 0 || net < 0 || env < 0 || mnt < 0 {
		t.Fatalf("missing flag in args: %v", args)
	}
	if sec >= net || net >= env || env >= mnt {
		t.Errorf("order: --security-opt=%d --network=%d -e=%d --mount=%d; want ascending", sec, net, env, mnt)
	}
}

func TestShellCommand_NetworkLines(t *testing.T) {
	cases := []struct {
		name     string
		mode     string
		networks []string
		want     []string
	}{
		{name: "mode", mode: "container:proxy", want: []string{"  --network container:proxy \\"}},
		{name: "networks", networks: []string{"a", "b"},
			want: []string{"  --network a \\", "  --network b \\"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := Spec{Image: "img", Command: "sh", Workdir: "/wd", NetworkMode: c.mode, Networks: c.networks}
			out := spec.ShellCommand()
			lines := strings.Split(out, "\n")
			var got []string
			for _, l := range lines {
				if strings.HasPrefix(l, "  --network") {
					got = append(got, l)
				}
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("network lines = %q, want %q\nfull:\n%s", got, c.want, out)
			}
		})
	}
}

func TestShellCommand_NoNetworkLineWhenUnset(t *testing.T) {
	if out := BuildSpec(sampleOptions()).ShellCommand(); strings.Contains(out, "--network") {
		t.Errorf("unexpected --network in default ShellCommand:\n%s", out)
	}
}

// Drift-guard: --network values in Args() match HostConfig.NetworkMode and the
// NetworkingConfig endpoint keys.
func TestDriftGuard_Network(t *testing.T) {
	cases := []struct {
		name     string
		mode     string
		networks []string
	}{
		{"unset", "", nil},
		{"bridge", "bridge", nil},
		{"none", "none", nil},
		{"container", "container:proxy", nil},
		{"custom_mode", "myapp_default", nil},
		{"one_network", "", []string{"n1"}},
		{"two_networks", "", []string{"n2", "n1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := sampleOptions()
			o.NetworkMode = c.mode
			o.Networks = c.networks
			spec := BuildSpec(o)
			flags := collectFlagValues(spec.Args(), "--network")
			hc := spec.HostConfig()
			nc := spec.NetworkingConfig()

			if len(flags) == 0 {
				if hc.NetworkMode != "" || nc != nil {
					t.Errorf("no --network flag but HostConfig.NetworkMode=%q NetworkingConfig=%v", hc.NetworkMode, nc)
				}
				return
			}
			if string(hc.NetworkMode) != flags[0] {
				t.Errorf("HostConfig.NetworkMode = %q, first --network = %q", hc.NetworkMode, flags[0])
			}
			if len(c.networks) == 0 {
				if nc != nil {
					t.Errorf("network_mode only: NetworkingConfig = %v, want nil", nc)
				}
				if len(flags) != 1 {
					t.Errorf("network_mode only: %d --network flags, want 1", len(flags))
				}
				return
			}
			if nc == nil || len(nc.EndpointsConfig) != len(flags) {
				t.Fatalf("NetworkingConfig endpoints %v do not match --network flags %v", nc, flags)
			}
			for _, f := range flags {
				if _, ok := nc.EndpointsConfig[f]; !ok {
					t.Errorf("--network %q has no NetworkingConfig endpoint", f)
				}
			}
		})
	}
}

// collectFlagValues returns the values following each occurrence of flag in args.
func collectFlagValues(args []string, flag string) []string {
	var out []string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			out = append(out, args[i+1])
			i++
		}
	}
	return out
}

// Projects[0] is always bound read-write: ReadOnly applies to joins only.
func TestBuildSpec_MainProjectReadOnlyIgnored(t *testing.T) {
	o := sampleOptions()
	o.Projects[0].ReadOnly = true
	spec := BuildSpec(o)
	want := Mount{Host: "/home/me/code/myproj", Container: "/workspace/myproj-abc123"}
	if spec.Mounts[0] != want {
		t.Errorf("mounts[0] = %+v, want %+v", spec.Mounts[0], want)
	}
	if !reflect.DeepEqual(spec.Mounts, BuildSpec(sampleOptions()).Mounts) {
		t.Errorf("ReadOnly on Projects[0] must not change mounts")
	}
}

// joinOptions returns sampleOptions with labels set on main; joins are
// appended by the caller.
func joinOptions(joins ...Project) Options {
	o := sampleOptions()
	o.Projects[0].Label = "project: /home/me/code/myproj"
	o.Projects = append(o.Projects, joins...)
	return o
}

// mainMountCount is the number of mounts sampleOptions emits for the main
// project with both cache flags on and no sandbox/masks.
const mainMountCount = 8

func TestBuildSpec_Join_RW(t *testing.T) {
	o := joinOptions(Project{
		Host:          "/home/me/code/lib",
		Name:          "lib",
		Label:         "join: /home/me/code/lib (rw)",
		MaskedFiles:   []string{"/home/me/code/lib/.env"},
		MaskedDirs:    []string{"/home/me/code/lib/secrets"},
		ProtectConfig: true,
		MaskGitHooks:  true,
	})
	spec := BuildSpec(o)
	got := spec.Mounts[mainMountCount:]
	want := []Mount{
		{Host: "/home/me/code/lib", Container: "/workspace/lib"},
		{Host: "/home/me/code/lib/.makeslop.yaml", Container: "/workspace/lib/.makeslop.yaml", ReadOnly: true},
		{Type: "tmpfs", Container: "/workspace/lib/.git/hooks"},
		{Host: "/dev/null", Container: "/workspace/lib/.env"},
		{Type: "tmpfs", Container: "/workspace/lib/secrets"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("join mounts =\n%+v\nwant\n%+v", got, want)
	}
	if !reflect.DeepEqual(spec.Mounts[:mainMountCount], BuildSpec(sampleOptions()).Mounts) {
		t.Errorf("main group changed by join")
	}
	if spec.Workdir != "/workspace/myproj-abc123" {
		t.Errorf("Workdir = %q, want main project", spec.Workdir)
	}
}

func TestBuildSpec_Join_RO(t *testing.T) {
	o := joinOptions(Project{
		Host:          "/home/me/code/lib",
		Name:          "lib",
		ReadOnly:      true,
		MaskedFiles:   []string{"/home/me/code/lib/.makeslop.yaml", "/home/me/code/lib/.env"},
		ProtectConfig: true,
		MaskGitHooks:  true,
	})
	got := BuildSpec(o).Mounts[mainMountCount:]
	want := []Mount{
		{Host: "/home/me/code/lib", Container: "/workspace/lib", ReadOnly: true},
		{Host: "/dev/null", Container: "/workspace/lib/.makeslop.yaml"},
		{Host: "/dev/null", Container: "/workspace/lib/.env"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ro join mounts =\n%+v\nwant\n%+v", got, want)
	}
}

func TestBuildSpec_Join_RWConfigMaskFiltered(t *testing.T) {
	o := joinOptions(Project{
		Host:          "/home/me/code/lib",
		Name:          "lib",
		MaskedFiles:   []string{"/home/me/code/lib/.makeslop.yaml", "/home/me/code/lib/a.yaml"},
		ProtectConfig: true,
	})
	got := BuildSpec(o).Mounts[mainMountCount:]
	want := []Mount{
		{Host: "/home/me/code/lib", Container: "/workspace/lib"},
		{Host: "/home/me/code/lib/.makeslop.yaml", Container: "/workspace/lib/.makeslop.yaml", ReadOnly: true},
		{Host: "/dev/null", Container: "/workspace/lib/a.yaml"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rw join mounts =\n%+v\nwant\n%+v", got, want)
	}
}

// A join without ProtectConfig keeps a mask on its config path: nothing binds it.
func TestBuildSpec_Join_RWNoProtectKeepsConfigMask(t *testing.T) {
	o := joinOptions(Project{
		Host:        "/home/me/code/lib",
		Name:        "lib",
		MaskedFiles: []string{"/home/me/code/lib/.makeslop.yaml"},
	})
	got := BuildSpec(o).Mounts[mainMountCount:]
	want := []Mount{
		{Host: "/home/me/code/lib", Container: "/workspace/lib"},
		{Host: "/dev/null", Container: "/workspace/lib/.makeslop.yaml"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("join mounts =\n%+v\nwant\n%+v", got, want)
	}
}

// Join mask targets are relative to the join's Host, never the main root.
func TestBuildSpec_Join_MaskTargetsRelativeToJoinHost(t *testing.T) {
	o := joinOptions(Project{
		Host:        "/srv/other/deep/lib",
		Name:        "lib",
		MaskedFiles: []string{"/srv/other/deep/lib/conf/prod.key"},
		MaskedDirs:  []string{"/srv/other/deep/lib/a/b"},
	})
	got := BuildSpec(o).Mounts[mainMountCount:]
	want := []Mount{
		{Host: "/srv/other/deep/lib", Container: "/workspace/lib"},
		{Host: "/dev/null", Container: "/workspace/lib/conf/prod.key"},
		{Type: "tmpfs", Container: "/workspace/lib/a/b"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("join mounts =\n%+v\nwant\n%+v", got, want)
	}
}

// Main masks stay under the main workspace and join masks under the join.
func TestBuildSpec_Join_MasksIsolatedPerProject(t *testing.T) {
	o := joinOptions(Project{
		Host:        "/home/me/code/lib",
		Name:        "lib",
		MaskedFiles: []string{"/home/me/code/lib/.env"},
	})
	o.Projects[0].MaskedFiles = []string{"/home/me/code/myproj/.env"}
	spec := BuildSpec(o)
	var targets []string
	for _, m := range spec.Mounts {
		if m.Host == "/dev/null" {
			targets = append(targets, m.Container)
		}
	}
	want := []string{"/workspace/myproj-abc123/.env", "/workspace/lib/.env"}
	if !reflect.DeepEqual(targets, want) {
		t.Errorf("mask targets = %v, want %v", targets, want)
	}
}

func TestBuildSpec_TwoJoins_OrderAndSections(t *testing.T) {
	o := joinOptions(
		Project{
			Host: "/home/me/code/lib", Name: "lib", Label: "join: /home/me/code/lib (rw)",
			MaskedFiles: []string{"/home/me/code/lib/.env"}, ProtectConfig: true, MaskGitHooks: true,
		},
		Project{
			Host: "/home/me/code/util", Name: "util", Label: "join: /home/me/code/util (ro)",
			ReadOnly: true, MaskedDirs: []string{"/home/me/code/util/keys"},
		},
	)
	o.Projects[0].MaskedFiles = []string{"/home/me/code/myproj/.env"}
	spec := BuildSpec(o)

	wantSections := []Section{
		{Label: "project: /home/me/code/myproj", Start: 0},
		{Label: "join: /home/me/code/lib (rw)", Start: 9},
		{Label: "join: /home/me/code/util (ro)", Start: 13},
	}
	if !reflect.DeepEqual(spec.Sections, wantSections) {
		t.Errorf("Sections = %+v, want %+v", spec.Sections, wantSections)
	}
	wantTail := []Mount{
		{Host: "/home/me/code/lib", Container: "/workspace/lib"},
		{Host: "/home/me/code/lib/.makeslop.yaml", Container: "/workspace/lib/.makeslop.yaml", ReadOnly: true},
		{Type: "tmpfs", Container: "/workspace/lib/.git/hooks"},
		{Host: "/dev/null", Container: "/workspace/lib/.env"},
		{Host: "/home/me/code/util", Container: "/workspace/util", ReadOnly: true},
		{Type: "tmpfs", Container: "/workspace/util/keys"},
	}
	if got := spec.Mounts[9:]; !reflect.DeepEqual(got, wantTail) {
		t.Errorf("join mounts =\n%+v\nwant\n%+v", got, wantTail)
	}
	for _, s := range spec.Sections {
		if want := "/workspace/"; !strings.HasPrefix(spec.Mounts[s.Start].Container, want) ||
			strings.Count(spec.Mounts[s.Start].Container, "/") != 2 {
			t.Errorf("section %q Start=%d points at %+v, want a project bind", s.Label, s.Start, spec.Mounts[s.Start])
		}
	}
}

func TestBuildSpec_NoJoins_SectionsNil(t *testing.T) {
	o := sampleOptions()
	o.Projects[0].Label = "project: /home/me/code/myproj"
	if s := BuildSpec(o).Sections; s != nil {
		t.Errorf("Sections = %+v, want nil", s)
	}
}

// parseMountArg splits a --mount value into its CSV key=value fields.
func parseMountArg(t *testing.T, raw string) map[string]string {
	t.Helper()
	rec, err := csv.NewReader(strings.NewReader(raw)).Read()
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	out := map[string]string{}
	for _, f := range rec {
		k, v, _ := strings.Cut(f, "=")
		out[k] = v
	}
	return out
}

// With joins, Args() --mount tokens and HostConfig().Mounts agree in count,
// order, type, source, target and readonly.
func TestDriftGuard_Joins(t *testing.T) {
	o := joinOptions(
		Project{
			Host: "/home/me/code/lib", Name: "lib", Label: "join: /home/me/code/lib (rw)",
			MaskedFiles: []string{"/home/me/code/lib/.env"}, MaskedDirs: []string{"/home/me/code/lib/d"},
			ProtectConfig: true, MaskGitHooks: true,
		},
		Project{
			Host: "/home/me/code/util", Name: "util", Label: "join: /home/me/code/util (ro)",
			ReadOnly: true, MaskedFiles: []string{"/home/me/code/util/.makeslop.yaml"},
			ProtectConfig: true, MaskGitHooks: true,
		},
	)
	o.Projects[0].ProtectConfig = true
	o.Projects[0].MaskGitHooks = true
	spec := BuildSpec(o)
	argsMounts := collectMountArgs(spec.Args())
	hc := spec.HostConfig()
	if len(argsMounts) != len(hc.Mounts) || len(argsMounts) != len(spec.Mounts) {
		t.Fatalf("mount count: Args=%d, HostConfig=%d, Spec=%d", len(argsMounts), len(hc.Mounts), len(spec.Mounts))
	}
	for i, raw := range argsMounts {
		f := parseMountArg(t, raw)
		m := hc.Mounts[i]
		if f["type"] != string(m.Type) {
			t.Errorf("[%d] type: Args=%q, HostConfig=%q", i, f["type"], m.Type)
		}
		if f["target"] != m.Target {
			t.Errorf("[%d] target: Args=%q, HostConfig=%q", i, f["target"], m.Target)
		}
		if f["source"] != m.Source {
			t.Errorf("[%d] source: Args=%q, HostConfig=%q", i, f["source"], m.Source)
		}
		_, ro := f["readonly"]
		if ro != m.ReadOnly {
			t.Errorf("[%d] readonly: Args=%v, HostConfig=%v", i, ro, m.ReadOnly)
		}
	}
}

// joinSectionSpec is a minimal hand-built Spec with two sections, so the
// golden stays stable if BuildSpec later adds flags.
func joinSectionSpec(mainLabel, joinLabel string) Spec {
	return Spec{
		Image:   "claudebox",
		Command: "/bin/zsh",
		Workdir: "/workspace/app",
		Mounts: []Mount{
			{Host: "/home/me/app", Container: "/workspace/app"},
			{Host: "/dev/null", Container: "/workspace/app/.env"},
			{Host: "/home/me/lib", Container: "/workspace/lib", ReadOnly: true},
			{Type: "tmpfs", Container: "/workspace/lib/keys"},
		},
		Tmpfs: []string{"/tmp:size=100m"},
		Sections: []Section{
			{Label: mainLabel, Start: 0},
			{Label: joinLabel, Start: 2},
		},
	}
}

func TestShellCommand_Sections_GoldenString(t *testing.T) {
	spec := joinSectionSpec("project: /home/me/app", "join: /home/me/lib (ro)")
	want := "docker run \\\n" +
		"  --rm \\\n" +
		"  -it \\\n" +
		"  --workdir /workspace/app \\\n" +
		"  --tmpfs /tmp:size=100m \\\n" +
		"  `: '--- project: /home/me/app ---'` \\\n" +
		"  --mount type=bind,source=/home/me/app,target=/workspace/app \\\n" +
		"  --mount type=bind,source=/dev/null,target=/workspace/app/.env \\\n" +
		"  `: '--- join: /home/me/lib (ro) ---'` \\\n" +
		"  --mount type=bind,source=/home/me/lib,target=/workspace/lib,readonly \\\n" +
		"  --mount type=tmpfs,target=/workspace/lib/keys \\\n" +
		"  claudebox \\\n" +
		"  /bin/zsh"
	if got := spec.ShellCommand(); got != want {
		t.Errorf("ShellCommand mismatch\ngot:\n%s\n\nwant:\n%s", got, want)
	}
}

// Sections come from BuildSpec: one separator per project, each directly
// before that project's bind mount line.
func TestShellCommand_Sections_FromBuildSpec(t *testing.T) {
	o := joinOptions(
		Project{Host: "/home/me/code/lib", Name: "lib", Label: "join: /home/me/code/lib (rw)",
			MaskedFiles: []string{"/home/me/code/lib/.env"}, ProtectConfig: true, MaskGitHooks: true},
		Project{Host: "/home/me/code/util", Name: "util", Label: "join: /home/me/code/util (ro)", ReadOnly: true},
	)
	lines := strings.Split(BuildSpec(o).ShellCommand(), "\n")
	wantBefore := map[string]string{
		"  `: '--- project: /home/me/code/myproj ---'` \\": "  --mount type=bind,source=/home/me/code/myproj,target=/workspace/myproj-abc123 \\",
		"  `: '--- join: /home/me/code/lib (rw) ---'` \\":  "  --mount type=bind,source=/home/me/code/lib,target=/workspace/lib \\",
		"  `: '--- join: /home/me/code/util (ro) ---'` \\": "  --mount type=bind,source=/home/me/code/util,target=/workspace/util,readonly \\",
	}
	seps := 0
	for i, line := range lines {
		if !strings.HasPrefix(line, "  `:") {
			continue
		}
		seps++
		next, ok := wantBefore[line]
		if !ok {
			t.Errorf("unexpected separator line %q", line)
			continue
		}
		if i+1 >= len(lines) || lines[i+1] != next {
			t.Errorf("separator %q followed by %q, want %q", line, lines[min(i+1, len(lines)-1)], next)
		}
	}
	if seps != len(wantBefore) {
		t.Errorf("got %d separators, want %d:\n%s", seps, len(wantBefore), strings.Join(lines, "\n"))
	}
}

// Without joins the output has no separator, even when the main Label is set.
func TestShellCommand_NoJoins_NoSeparator(t *testing.T) {
	o := sampleOptions()
	o.Projects[0].Label = "project: /home/me/code/myproj"
	labeled := BuildSpec(o).ShellCommand()
	if strings.Contains(labeled, "`") {
		t.Errorf("unexpected separator without joins:\n%s", labeled)
	}
	if plain := BuildSpec(sampleOptions()).ShellCommand(); labeled != plain {
		t.Errorf("Label changed output without joins:\ngot:\n%s\nwant:\n%s", labeled, plain)
	}
}

func TestArgs_NeverContainsSeparators(t *testing.T) {
	spec := joinSectionSpec("project: /home/me/app", "join: /home/me/lib (ro)")
	withSections := spec.Args()
	spec.Sections = nil
	if !reflect.DeepEqual(withSections, spec.Args()) {
		t.Errorf("Sections changed Args():\n%q\nvs\n%q", withSections, spec.Args())
	}
	for _, a := range withSections {
		if strings.Contains(a, "---") || strings.Contains(a, "`") {
			t.Errorf("Args() contains separator text: %q", a)
		}
	}
}

func TestSanitizeLabel(t *testing.T) {
	cases := map[string]string{
		"join: /home/me/lib (ro)": "join: /home/me/lib (ro)",
		"a`b":                     "a?b",
		"a$(id)":                  "a?(id)",
		`a\b`:                     "a?b",
		"a\nb\rc\td\x00e\x7f":     "a?b?c?d?e?",
		`it's "q" (x); y`:         `it's "q" (x); y`,
	}
	for in, want := range cases {
		if got := sanitizeLabel(in); got != want {
			t.Errorf("sanitizeLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// hostileLabel exercises every character class the separator must neutralize.
const hostileLabel = "join: /home/me/a`id`$(id)${HOME}\\x\n; echo pwned 'q' \"dq\" (ro) ; rm"

func TestShellCommand_HostileLabel_SingleLine(t *testing.T) {
	out := joinSectionSpec("project: /home/me/app", hostileLabel).ShellCommand()
	var sep []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "  `:") {
			sep = append(sep, line)
		}
	}
	if len(sep) != 2 {
		t.Fatalf("want 2 separator lines, got %d:\n%s", len(sep), out)
	}
	line := sep[1]
	if strings.Count(line, "`") != 2 || strings.Contains(line, "$") ||
		strings.Contains(strings.ReplaceAll(strings.TrimSuffix(line, " \\"), `'\''`, ""), `\`) {
		t.Errorf("separator not neutralized: %q", line)
	}
	want := "  `: '--- join: /home/me/a?id??(id)?{HOME}?x?; echo pwned '\\''q'\\'' \"dq\" (ro) ; rm ---'` \\"
	if line != want {
		t.Errorf("separator =\n%s\nwant\n%s", line, want)
	}
}

// The rendered command must parse cleanly and pass exactly Args() to docker in
// every shell the dry-run output may be pasted into. One spec carries value
// flags (-e, --network) whose values look like --mount, so separator placement
// must count only --mount flag tokens.
func TestShellCommand_Sections_PasteableInShells(t *testing.T) {
	plain := joinSectionSpec("project: /home/me/app", hostileLabel)
	withValues := joinSectionSpec("project: /home/me/app", "join: /home/me/lib (ro)")
	withValues.Env = []string{"A=--mount", "B=x y"}
	withValues.NetworkMode = "host"
	if out := withValues.ShellCommand(); !strings.Contains(out,
		"`: '--- join: /home/me/lib (ro) ---'` \\\n  --mount type=bind,source=/home/me/lib,") {
		t.Fatalf("join separator not directly before the join bind:\n%s", out)
	}

	shells := [][]string{
		{"bash", "--norc", "--noprofile", "-c"},
		{"bash", "--posix", "-c"},
		{"dash", "-c"},
		{"zsh", "-f", "-c"},
		{"zsh", "-f", "-i", "-c"}, // interactive: # is not a comment here
	}
	specs := []struct {
		name string
		spec Spec
	}{{"hostile label", plain}, {"value flags", withValues}}

	for _, tc := range specs {
		script := "docker() { printf '%s\\n' \"$@\"; }\n" + tc.spec.ShellCommand() + "\n"
		want := strings.Join(tc.spec.Args(), "\n") + "\n"
		for _, sh := range shells {
			t.Run(tc.name+"/"+strings.Join(sh, " "), func(t *testing.T) {
				if _, err := exec.LookPath(sh[0]); err != nil {
					if os.Getenv("CI") != "" {
						t.Fatalf("%s not installed (required in CI)", sh[0])
					}
					t.Skipf("%s not installed", sh[0])
				}
				cmd := exec.Command(sh[0], append(sh[1:], script)...)
				var stdout, stderr strings.Builder
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}
				if err := cmd.Run(); err != nil {
					t.Fatalf("%v: %v\nstderr: %s", sh, err, stderr.String())
				}
				if got := stripTTYNoise(stderr.String()); got != "" {
					t.Errorf("stderr not empty: %q", got)
				}
				if stdout.String() != want {
					t.Errorf("argv mismatch\ngot:\n%s\nwant:\n%s", stdout.String(), want)
				}
			})
		}
	}
}

// stripTTYNoise drops the job-control/terminal warnings an interactive shell
// prints when it has no controlling terminal (CI runners, piped go test).
func stripTTYNoise(stderr string) string {
	noise := []string{"job control", "tty", "TTY", "terminal", "zle", "ioctl"}
	var kept []string
	for _, line := range strings.Split(stderr, "\n") {
		if line == "" {
			continue
		}
		isNoise := false
		for _, n := range noise {
			if strings.Contains(line, n) {
				isNoise = true
				break
			}
		}
		if !isNoise {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}
