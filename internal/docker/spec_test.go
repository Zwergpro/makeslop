package docker

import (
	"encoding/csv"
	"reflect"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/mount"
)

func sampleOptions() Options {
	return Options{
		ProjectRoot:       "/home/me/code/myproj",
		WorkspaceName:     "myproj-abc123",
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
	o.MaskedFiles = []string{
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
	o.MaskedFiles = []string{
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
	o.MaskedDirs = []string{
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
	o.MaskedFiles = []string{"/home/me/code/myproj/.env"}
	o.MaskedDirs = []string{"/home/me/code/myproj/node_modules"}
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
	o.MaskedDirs = []string{"/home/me/code/myproj/node_modules"}
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

// Empty NetworkMode resolves to bridge networking.
func TestHostConfig_NetworkModeIsAlwaysBridge(t *testing.T) {
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
	o.MaskedFiles = []string{
		"/home/me/code/myproj/.env",
		"/home/me/code/myproj/configs/secret.yaml",
	}
	o.MaskedDirs = []string{"/home/me/code/myproj/node_modules"}
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
				ProjectRoot:       "/home/me/code/myproj",
				WorkspaceName:     "myproj-abc123",
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
				ProjectRoot:       "/home/me/code/myproj",
				WorkspaceName:     "myproj-abc123",
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
	oProtect.ProtectProjectConfig = true
	if got, want := len(BuildSpec(oProtect).Mounts), wantBaseline+1; got != want {
		t.Errorf("ProtectProjectConfig=true: mount count = %d, want %d (baseline+1)", got, want)
	}

	oHooks := sampleOptions()
	oHooks.MaskGitHooks = true
	if got, want := len(BuildSpec(oHooks).Mounts), wantBaseline+1; got != want {
		t.Errorf("MaskGitHooks=true: mount count = %d, want %d (baseline+1)", got, want)
	}

	// Both on: exactly two extra mounts.
	oBoth := sampleOptions()
	oBoth.ProtectProjectConfig = true
	oBoth.MaskGitHooks = true
	if got, want := len(BuildSpec(oBoth).Mounts), wantBaseline+2; got != want {
		t.Errorf("both flags on: mount count = %d, want %d (baseline+2)", got, want)
	}
}

// ProtectProjectConfig=true: .makeslop.yaml read-only bind is present at mounts[4]
// (after the 4 base mounts and before any cache overlays).
func TestBuildSpec_ProtectProjectConfig_MountPresent(t *testing.T) {
	o := sampleOptions()
	o.ProtectProjectConfig = true
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

// MaskGitHooks=true: .git/hooks tmpfs mount is present at a fixed position.
func TestBuildSpec_MaskGitHooks_MountPresent(t *testing.T) {
	o := sampleOptions()
	o.MaskGitHooks = true
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
	o.ProtectProjectConfig = true
	o.MaskGitHooks = true
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
	o.ProtectProjectConfig = true
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
	o.MaskGitHooks = true
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
	o.ProtectProjectConfig = true
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
	o.MaskGitHooks = true
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
	o.ProtectProjectConfig = true
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
			o.ProtectProjectConfig = c.protectProjectConfig
			o.MaskGitHooks = c.maskGitHooks

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
