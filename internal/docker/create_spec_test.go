package docker

import (
	"slices"
	"strings"
	"testing"
)

func TestBuildModes_CreateParity(t *testing.T) {
	in := modeInputs()
	fg, err := BuildForeground(in)
	if err != nil {
		t.Fatal(err)
	}
	bg, err := BuildBackground(in, testBGUUID)
	if err != nil {
		t.Fatal(err)
	}

	if got := fg.create.createOptions(); got.Name != "" || len(got.Config.Labels) != 0 ||
		!slices.Equal(got.Config.Cmd, []string{"/bin/sh"}) || !slices.Equal(got.Config.Env, in.Env) ||
		!got.Config.Tty || !got.Config.OpenStdin || !got.Config.AttachStdin || !got.Config.AttachStdout ||
		!got.Config.AttachStderr || !got.HostConfig.AutoRemove {
		t.Fatalf("foreground: config=%+v host=%+v", got.Config, got.HostConfig)
	}
	if dry := fg.ShellCommand(); !slices.Equal(fg.create.Args()[:3], []string{"run", "--rm", "-it"}) ||
		!strings.Contains(dry, "ZMX_DIR=/wrong") || !strings.HasSuffix(dry, "  /bin/sh") {
		t.Fatalf("foreground preview: %s", dry)
	}

	in.Env[0] = "KEEP=changed"
	wantEnv := []string{
		"KEEP=1", "ZMX_DIR=/tmp/makeslop-zmx", "ZMX_DIR_MODE=0700", "ZMX_SESSION=", "ZMX_SESSION_PREFIX=",
	}
	got := bg.create.createOptions()
	wantCmd := []string{"zmx", "attach", "agent", "/bin/sh"}

	if got.Name != bgPrefix+testBGUUID || !slices.Equal(got.Config.Cmd, wantCmd) ||
		!slices.Equal(got.Config.Env, wantEnv) || !got.Config.Tty || !got.Config.OpenStdin ||
		got.Config.AttachStdin || got.Config.AttachStdout || got.Config.AttachStderr || got.HostConfig.AutoRemove {
		t.Fatalf("background %q: config=%+v host=%+v", got.Name, got.Config, got.HostConfig)
	}
	dry := bg.ShellCommand()
	if !strings.Contains(dry, "  -dit") || !strings.Contains(dry, got.Name) || strings.Contains(dry, "/wrong") {
		t.Fatalf("background preview: %s", dry)
	}
	argv := bg.create.Args()
	if !slices.Equal(argv[len(argv)-4:], wantCmd) {
		t.Fatalf("preview argv: %q", argv)
	}

	previous := -1
	for _, pair := range []struct{ key, value string }{
		{bgUUIDLabel, testBGUUID}, {bgProjectLabel, "demo"}, {bgShellLabel, "/bin/sh"},
	} {
		index := strings.Index(dry, pair.key+"="+pair.value)
		if got.Config.Labels[pair.key] != pair.value || index <= previous {
			t.Fatalf("label %s: SDK=%q preview=%q", pair.key, got.Config.Labels[pair.key], dry)
		}
		previous = index
	}
	for _, entry := range wantEnv {
		if strings.Count(dry, entry) != 1 {
			t.Fatalf("environment %s missing or repeated: %s", entry, dry)
		}
	}
}

func TestBuildModes_InvalidInput(t *testing.T) {
	bad := modeInputs()
	bad.CacheMounts = []Mount{{Type: "unknown", Container: "/data"}}
	missingRoot := modeInputs()
	missingRoot.Layout.Main.ContainerRoot = ""

	for _, root := range []ProjectRoot{
		{HostRoot: "relative", ContainerRoot: "/src/lib"},
		{HostRoot: "/host/lib", ContainerRoot: "relative"},
		{HostRoot: "/host/lib", ContainerRoot: "/src/lib/"},
	} {
		in := modeInputs()
		in.Layout.Joins = []Project{{ProjectRoot: root}}
		if _, err := BuildForeground(in); err == nil {
			t.Errorf("foreground accepted invalid join %+v", root)
		}

		if _, err := BuildBackground(in, testBGUUID); err == nil {
			t.Errorf("background accepted invalid join %+v", root)
		}
	}

	for _, in := range []BuildInputs{bad, missingRoot} {
		if _, err := BuildForeground(in); err == nil {
			t.Error("foreground accepted invalid inputs")
		}
		if _, err := BuildBackground(in, testBGUUID); err == nil {
			t.Error("background accepted invalid inputs")
		}
	}
	if _, err := BuildBackground(modeInputs(), "bad-uuid"); err == nil {
		t.Error("background accepted invalid UUID")
	}
}

func TestRunModes_RejectZeroSpec(t *testing.T) {
	fg := &fakeRunClient{}
	bg := &bgLifecycleClient{}
	d := &Docker{client: fg, isTTYFn: func() bool { return true }}

	if err := d.Run(t.Context(), ForegroundSpec{}); err == nil || fg.LastContainerCreateOpts.Config != nil {
		t.Fatalf("foreground zero spec: err=%v create=%+v", err, fg.LastContainerCreateOpts)
	}
	if err := (&Docker{client: bg}).RunBG(t.Context(), BackgroundSpec{}); err == nil || len(bg.calls) != 0 {
		t.Fatalf("background zero spec: err=%v calls=%v", err, bg.calls)
	}
}

func modeInputs() BuildInputs {
	return BuildInputs{
		Layout: Layout{Main: Project{ProjectRoot: ProjectRoot{
			Name: "demo", HostRoot: "/host/demo", ContainerRoot: "/workspace/demo",
		}}},
		Image: "local:test", Shell: "/bin/sh", TmpDirSize: "1g",
		Env: []string{"KEEP=1", "ZMX_DIR=/wrong", "ZMX_DIR_MODE=bad", "ZMX_SESSION=bad", "ZMX_SESSION_PREFIX=bad"},
	}
}
