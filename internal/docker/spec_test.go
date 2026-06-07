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
//
// Only bind mounts are CSV-checked here; tmpfs mounts emit 2 fields
// (type=tmpfs,target=...) and are covered by TestSpecArgs_TmpfsMountFlagShape.
func TestSpecArgs_MountArgsParseAsRFC4180CSV(t *testing.T) {
	spec := BuildSpec(sampleOptions())
	args := spec.Args()

	type pair struct{ host, container string }
	// Collect bind mounts only (skip tmpfs entries).
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

// ── ShellCommand tests ────────────────────────────────────────────────────────

// TestShellCommand_MinimalSpec_GoldenString tests ShellCommand against a
// minimal hand-built Spec (not via BuildSpec) so the golden stays stable if
// BuildSpec later adds new flags.
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
	// Final line must NOT have trailing backslash.
	lines := strings.Split(got, "\n")
	last := lines[len(lines)-1]
	if strings.HasSuffix(last, `\`) {
		t.Errorf("final line must not have trailing backslash: %q", last)
	}
	// All lines except the last must have trailing backslash.
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
	// Must start with docker run and end without trailing backslash.
	if !strings.HasPrefix(got, "docker run") {
		t.Errorf("must start with 'docker run', got: %q", got)
	}
	lines := strings.Split(got, "\n")
	if last := lines[len(lines)-1]; strings.HasSuffix(last, `\`) {
		t.Errorf("final line must not end with backslash: %q", last)
	}
	// round-trip: parsed tokens must equal ["docker"] + Args()
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

// ── TmpDirSize tests ──────────────────────────────────────────────────────────

// TestBuildSpec_TmpDirSize_Custom verifies that a custom TmpDirSize value is
// reflected verbatim in Spec.Tmpfs and propagated through Args().
func TestBuildSpec_TmpDirSize_Custom(t *testing.T) {
	o := sampleOptions()
	o.TmpDirSize = "1000m"
	spec := BuildSpec(o)

	want := []string{"/tmp:size=1000m"}
	if !reflect.DeepEqual(spec.Tmpfs, want) {
		t.Errorf("Tmpfs = %v, want %v", spec.Tmpfs, want)
	}

	// Verify Args() contains --tmpfs /tmp:size=1000m.
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

// TestShellCommand_TmpDirSize_Custom verifies that ShellCommand renders a
// custom tmp_dir_size — the user-facing --dry-run verification path.
func TestShellCommand_TmpDirSize_Custom(t *testing.T) {
	o := sampleOptions()
	o.TmpDirSize = "1000m"
	spec := BuildSpec(o)
	out := spec.ShellCommand()

	if !strings.Contains(out, "--tmpfs /tmp:size=1000m") {
		t.Errorf("ShellCommand missing '--tmpfs /tmp:size=1000m':\n%s", out)
	}
}

// TestBuildSpec_TmpDirSize_DefaultPath verifies that the default (100m)
// supplied by config.Load passes through unchanged when TmpDirSize = "100m".
func TestBuildSpec_TmpDirSize_DefaultPath(t *testing.T) {
	// sampleOptions() already sets TmpDirSize: "100m" — simulate Load-supplied default.
	spec := BuildSpec(sampleOptions())

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

// ── ReadOnly / NetworkMode / Env tests ───────────────────────────────────────

func TestBuildSpec_ProxyUnconfigured(t *testing.T) {
	spec := BuildSpec(sampleOptions())
	if spec.NetworkMode != "" {
		t.Errorf("NetworkMode = %q, want empty", spec.NetworkMode)
	}
	if len(spec.Env) != 0 {
		t.Errorf("Env = %v, want nil/empty", spec.Env)
	}
	if len(spec.Mounts) != 8 {
		t.Errorf("Mounts len = %d, want 8", len(spec.Mounts))
	}
}

func TestSpecArgs_ProxyUnconfiguredArgvIdentical(t *testing.T) {
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

// TestSpecArgs_ProxyVolumeNameWithComma tests that a volume name containing a
// comma is properly CSV-quoted in the --mount argument.
func TestSpecArgs_ProxyVolumeNameWithComma(t *testing.T) {
	spec := Spec{
		Image:   "img",
		Command: "sh",
		Workdir: "/wd",
		Mounts: []Mount{
			{Type: "volume", Host: "vol,with,commas", Container: "/sockets", ReadOnly: true},
		},
	}
	args := spec.Args()
	mountArgs := collectMountArgs(args)
	if len(mountArgs) != 1 {
		t.Fatalf("want 1 mount arg, got %d", len(mountArgs))
	}
	want := `type=volume,"source=vol,with,commas",target=/sockets,readonly`
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

// ── ContainerConfig tests ─────────────────────────────────────────────────────

func TestContainerConfig_ImageCmdEnvTTYStdin(t *testing.T) {
	tests := []struct {
		name    string
		spec    Spec
		wantImg string
		wantCmd []string
		wantEnv []string
		wantWd  string
	}{
		{
			name:    "minimal spec",
			spec:    Spec{Image: "claudebox", Command: "/bin/zsh", Workdir: "/workspace/foo"},
			wantImg: "claudebox",
			wantCmd: []string{"/bin/zsh"},
			wantEnv: nil,
			wantWd:  "/workspace/foo",
		},
		{
			name: "spec with env vars",
			spec: Spec{
				Image:   "claudebox",
				Command: "/bin/bash",
				Workdir: "/workspace/bar",
				Env:     []string{"HTTP_PROXY=unix:///tmp/proxy.sock", "HTTPS_PROXY=unix:///tmp/proxy.sock"},
			},
			wantImg: "claudebox",
			wantCmd: []string{"/bin/bash"},
			wantEnv: []string{"HTTP_PROXY=unix:///tmp/proxy.sock", "HTTPS_PROXY=unix:///tmp/proxy.sock"},
			wantWd:  "/workspace/bar",
		},
		{
			name:    "from BuildSpec defaults",
			spec:    BuildSpec(sampleOptions()),
			wantImg: "claudebox",
			wantCmd: []string{"/bin/zsh"},
			wantEnv: nil,
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
			if !reflect.DeepEqual(cfg.Env, tc.wantEnv) {
				t.Errorf("Env = %v, want %v", cfg.Env, tc.wantEnv)
			}
			if cfg.WorkingDir != tc.wantWd {
				t.Errorf("WorkingDir = %q, want %q", cfg.WorkingDir, tc.wantWd)
			}
			// TTY and stdin flags must all be true.
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

// ── HostConfig tests ──────────────────────────────────────────────────────────

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

func TestHostConfig_NetworkModeDefault(t *testing.T) {
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
	// Verify that a volume type mount renders correctly in HostConfig.
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
	// Bind, tmpfs, and /dev/null masked-file mounts in one Spec.
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

// ── Drift-guard test ──────────────────────────────────────────────────────────

// TestDriftGuard_ArgsAndSDKProjectionsAgree asserts that the argv projection
// (Args/ShellCommand) and the SDK-struct projection (ContainerConfig/HostConfig)
// agree on all semantically load-bearing fields for a representative Spec that
// exercises masked files, masked dirs, and the default security/network settings.
// This catches silent drift where one projection is updated but not the other.
func TestDriftGuard_ArgsAndSDKProjectionsAgree(t *testing.T) {
	o := sampleOptions()
	o.MaskedFiles = []string{
		"/home/me/code/myproj/.env",
		"/home/me/code/myproj/configs/secret.yaml",
	}
	o.MaskedDirs = []string{"/home/me/code/myproj/node_modules"}

	spec := BuildSpec(o)
	args := spec.Args()
	cfg := spec.ContainerConfig()
	hc := spec.HostConfig()

	// --- image ---
	// Args(): second-to-last element is image name.
	if len(args) < 2 {
		t.Fatal("args too short")
	}
	argsImage := args[len(args)-2]
	if argsImage != cfg.Image {
		t.Errorf("image: Args=%q, ContainerConfig=%q", argsImage, cfg.Image)
	}

	// --- command ---
	argsCmd := args[len(args)-1]
	if len(cfg.Cmd) != 1 || cfg.Cmd[0] != argsCmd {
		t.Errorf("cmd: Args=%q, ContainerConfig.Cmd=%v", argsCmd, cfg.Cmd)
	}

	// --- workdir ---
	argsWorkdir := ""
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--workdir" {
			argsWorkdir = args[i+1]
		}
	}
	if argsWorkdir != cfg.WorkingDir {
		t.Errorf("workdir: Args=%q, ContainerConfig=%q", argsWorkdir, cfg.WorkingDir)
	}

	// --- env ---
	var argsEnv []string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-e" {
			argsEnv = append(argsEnv, args[i+1])
		}
	}
	if !reflect.DeepEqual(argsEnv, cfg.Env) {
		t.Errorf("env: Args=%v, ContainerConfig.Env=%v", argsEnv, cfg.Env)
	}

	// --- caps ---
	var argsCaps []string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--cap-drop" {
			argsCaps = append(argsCaps, args[i+1])
		}
	}
	if !reflect.DeepEqual(argsCaps, hc.CapDrop) {
		t.Errorf("cap-drop: Args=%v, HostConfig=%v", argsCaps, hc.CapDrop)
	}

	// --- secopt ---
	var argsSecOpt []string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--security-opt" {
			argsSecOpt = append(argsSecOpt, args[i+1])
		}
	}
	if !reflect.DeepEqual(argsSecOpt, hc.SecurityOpt) {
		t.Errorf("security-opt: Args=%v, HostConfig=%v", argsSecOpt, hc.SecurityOpt)
	}

	// --- network ---
	argsNetwork := ""
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--network" {
			argsNetwork = args[i+1]
		}
	}
	if argsNetwork != string(hc.NetworkMode) {
		t.Errorf("network: Args=%q, HostConfig=%q", argsNetwork, hc.NetworkMode)
	}

	// --- mounts: count bind/volume/tmpfs mounts in Args, compare to HostConfig ---
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

	// --- AutoRemove: --rm in args <=> AutoRemove=true in HostConfig ---
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

// ── Cache mount group tests ───────────────────────────────────────────────────

// TestBuildSpec_CacheMountCombos exercises all 4 combos of MountAgentCache /
// MountContentCache and asserts which per-workspace mounts are present or absent.
// Global mounts (BaseDir/.claude/, .claude.json, .codex/) must always be present
// regardless of the combination.
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

// TestBuildSpec_CacheMountCombos_Args ensures that when cache groups are
// disabled, the corresponding paths are absent from Args() output.
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

// TestDriftGuard_CacheMountCombos extends the drift-guard to cover all 4 combos
// of MountAgentCache/MountContentCache — ensuring Args() and HostConfig() agree
// on mount counts for each combination.
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

			// Also cross-check total mount counts (bind + tmpfs + volume) agree.
			if len(argsMounts) != len(hc.Mounts) {
				t.Errorf("total mount count: Args=%d, HostConfig=%d (combo: %s)",
					len(argsMounts), len(hc.Mounts), c.name)
			}
		})
	}
}
