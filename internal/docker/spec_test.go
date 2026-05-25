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

func collectMountArgs(argv []string) []string {
	out := make([]string, 0, 8)
	for i := 0; i < len(argv)-1; i++ {
		if argv[i] == "--mount" {
			out = append(out, argv[i+1])
		}
	}
	return out
}

// ── Proxy / ReadOnly / NetworkMode / Env tests ────────────────────────────────

func TestBuildSpec_ProxyConfigured(t *testing.T) {
	o := sampleOptions()
	o.ProxySocketHost = "/tmp/makeslop-abc123-42.sock"
	o.ProxySocketContainer = "/tmp/makeslop-proxy.sock"
	spec := BuildSpec(o)

	if spec.NetworkMode != "none" {
		t.Errorf("NetworkMode = %q, want %q", spec.NetworkMode, "none")
	}
	wantEnv := []string{
		"HTTP_PROXY=unix:///tmp/makeslop-proxy.sock",
		"HTTPS_PROXY=unix:///tmp/makeslop-proxy.sock",
	}
	if !reflect.DeepEqual(spec.Env, wantEnv) {
		t.Errorf("Env = %v, want %v", spec.Env, wantEnv)
	}
	lastMount := spec.Mounts[len(spec.Mounts)-1]
	wantMount := Mount{
		Host:      "/tmp/makeslop-abc123-42.sock",
		Container: "/tmp/makeslop-proxy.sock",
		ReadOnly:  true,
	}
	if lastMount != wantMount {
		t.Errorf("last mount = %+v, want %+v", lastMount, wantMount)
	}
}

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

func TestSpecArgs_ProxyArgvContainsNetworkEnvAndMount(t *testing.T) {
	o := sampleOptions()
	o.ProxySocketHost = "/tmp/makeslop-abc123-42.sock"
	o.ProxySocketContainer = "/tmp/makeslop-proxy.sock"
	spec := BuildSpec(o)
	args := spec.Args()

	foundNetwork := false
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--network" && args[i+1] == "none" {
			foundNetwork = true
			break
		}
	}
	if !foundNetwork {
		t.Errorf("argv missing --network none; got: %v", args)
	}

	wantEnvFlags := map[string]bool{
		"HTTP_PROXY=unix:///tmp/makeslop-proxy.sock":  false,
		"HTTPS_PROXY=unix:///tmp/makeslop-proxy.sock": false,
	}
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-e" {
			if _, ok := wantEnvFlags[args[i+1]]; ok {
				wantEnvFlags[args[i+1]] = true
			}
		}
	}
	for kv, found := range wantEnvFlags {
		if !found {
			t.Errorf("argv missing -e %s", kv)
		}
	}

	mountArgs := collectMountArgs(args)
	if len(mountArgs) == 0 {
		t.Fatal("no --mount args found")
	}
	lastMount := mountArgs[len(mountArgs)-1]
	wantLastMount := "type=bind,source=/tmp/makeslop-abc123-42.sock,target=/tmp/makeslop-proxy.sock,readonly"
	if lastMount != wantLastMount {
		t.Errorf("last --mount value = %q, want %q", lastMount, wantLastMount)
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

func TestShellCommand_ProxyFlagsRenderedCorrectly(t *testing.T) {
	o := sampleOptions()
	o.ProxySocketHost = "/tmp/makeslop-abc123-42.sock"
	o.ProxySocketContainer = "/tmp/makeslop-proxy.sock"
	spec := BuildSpec(o)
	out := spec.ShellCommand()

	if !strings.Contains(out, "--network none") {
		t.Errorf("ShellCommand missing '--network none':\n%s", out)
	}
	if !strings.Contains(out, "-e HTTP_PROXY=unix:///tmp/makeslop-proxy.sock") {
		t.Errorf("ShellCommand missing HTTP_PROXY env:\n%s", out)
	}
	if !strings.Contains(out, "-e HTTPS_PROXY=unix:///tmp/makeslop-proxy.sock") {
		t.Errorf("ShellCommand missing HTTPS_PROXY env:\n%s", out)
	}
	if !strings.Contains(out, "readonly") {
		t.Errorf("ShellCommand missing 'readonly' in mount:\n%s", out)
	}

	var got []string
	for _, raw := range strings.Split(out, "\n") {
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

func TestBuildSpec_ProxySocketMountPositionAfterOtherGroups(t *testing.T) {
	o := sampleOptions()
	o.MaskedFiles = []string{"/home/me/code/myproj/.env"}
	o.MaskedDirs = []string{"/home/me/code/myproj/secrets"}
	o.ProxySocketHost = "/tmp/makeslop-abc123-42.sock"
	o.ProxySocketContainer = "/tmp/makeslop-proxy.sock"
	spec := BuildSpec(o)

	n := len(spec.Mounts)
	if n < 3 {
		t.Fatalf("got %d mounts, want at least 3", n)
	}
	// Second-to-last should be the tmpfs (MaskedDirs), last should be socket.
	secondToLast := spec.Mounts[n-2]
	if secondToLast.Type != "tmpfs" {
		t.Errorf("second-to-last mount type = %q, want tmpfs", secondToLast.Type)
	}
	last := spec.Mounts[n-1]
	if last.Host != "/tmp/makeslop-abc123-42.sock" || !last.ReadOnly {
		t.Errorf("last mount = %+v, want proxy socket (ReadOnly=true)", last)
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
