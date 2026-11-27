package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/Zwergpro/makeslop/internal/config"
	"github.com/Zwergpro/makeslop/internal/docker"
)

const bgTestUUID = "018009ac-5fec-4c37-891a-244c91beda77"

type bgCLIProbe struct {
	*fakeDocker
	bgPreview                     string
	calls                         []string
	rows                          []docker.BGContainer
	argv                          []string
	uuid                          string
	ttyErr, errorRun, errorAttach error
	beforeAttach                  func(string)
	beforeRun                     func(context.Context)
}

func (p *bgCLIProbe) CheckTTY() error {
	p.calls = append(p.calls, "tty")

	return p.ttyErr
}

func (p *bgCLIProbe) RunBG(ctx context.Context, s docker.BackgroundSpec) error {
	p.calls = append(p.calls, "run")

	if p.beforeRun != nil {
		p.beforeRun(ctx)
	}

	if p.errorRun != nil {
		return p.errorRun
	}

	p.bgPreview = s.ShellCommand()

	return nil
}

func (p *bgCLIProbe) ListBG(context.Context) ([]docker.BGContainer, error) {
	p.calls = append(p.calls, "ls")

	return p.rows, nil
}

func (p *bgCLIProbe) AttachBG(_ context.Context, id string) error {
	p.calls = append(p.calls, "attach")
	p.uuid = id

	if p.beforeAttach != nil {
		p.beforeAttach(id)
	}

	return p.errorAttach
}

func (p *bgCLIProbe) ExecBG(_ context.Context, id string, argv []string) error {
	p.calls = append(p.calls, "exec")
	p.uuid = id
	p.argv = slices.Clone(argv)

	return &docker.ExitError{Code: 23}
}

func (p *bgCLIProbe) StopBG(_ context.Context, id string) error {
	p.calls = append(p.calls, "stop")
	p.uuid = id

	return nil
}

func (p *bgCLIProbe) StartBG(_ context.Context, id string) error {
	p.calls = append(p.calls, "start")
	p.uuid = id

	return nil
}

func (p *bgCLIProbe) RemoveBG(_ context.Context, id string) error {
	p.calls = append(p.calls, "remove")
	p.uuid = id

	return nil
}

func (p *bgCLIProbe) RestartBG(_ context.Context, id string) error {
	p.calls = append(p.calls, "restart")
	p.uuid = id

	return nil
}

type bgPolicyProbe struct {
	*bgCLIProbe
	checks []string
	trace  []string
}

func (p *bgPolicyProbe) CheckTTY() error {
	p.trace = append(p.trace, "tty")

	return p.bgCLIProbe.CheckTTY()
}

func (p *bgPolicyProbe) RunBG(ctx context.Context, s docker.BackgroundSpec) error {
	p.trace = append(p.trace, "run")

	return p.bgCLIProbe.RunBG(ctx, s)
}

func (p *bgPolicyProbe) AttachBG(ctx context.Context, id string) error {
	p.trace = append(p.trace, "attach")

	return p.bgCLIProbe.AttachBG(ctx, id)
}

func (p *bgPolicyProbe) CheckDaemon(ctx context.Context) error {
	p.checks = append(p.checks, "daemon")
	p.trace = append(p.trace, "daemon")

	return p.fakeDocker.CheckDaemon(ctx)
}

func (p *bgPolicyProbe) ImageExists(ctx context.Context, image string) (bool, error) {
	p.checks = append(p.checks, "image")
	p.trace = append(p.trace, "image")

	return p.fakeDocker.ImageExists(ctx, image)
}

func (p *bgPolicyProbe) NetworkExists(ctx context.Context, name string) (bool, error) {
	p.checks = append(p.checks, "network:"+name)
	p.trace = append(p.trace, "network:"+name)

	return p.fakeDocker.NetworkExists(ctx, name)
}

func TestBGCommands(t *testing.T) {
	t.Run("attach runs after preflight and cache preparation", func(t *testing.T) {
		base, root, ws := runCacheFixture(t)
		cacheDir := filepath.Join(ws, "docs")
		writeCacheConfig(t, root, "cache:\n  local: [docs]\nnetworks: [devnet]\n")
		p := &bgPolicyProbe{bgCLIProbe: &bgCLIProbe{fakeDocker: newFakeDocker(0, true), ttyErr: docker.ErrNoTTY}}
		p.Networks = map[string]bool{"devnet": true}
		deps := dockerDeps{api: p}
		out, _, err := runCmdWithDeps(t, base, deps, "bg", "run", "--attach", "-n")

		if err != nil || !strings.Contains(out, "# Example UUID; no container was created.") || len(p.trace) != 0 {
			t.Fatalf("preview: stdout=%q trace=%v err=%v", out, p.trace, err)
		}
		if _, err := os.Stat(cacheDir); !os.IsNotExist(err) {
			t.Fatalf("preview touched cache: %v", err)
		}

		p.ttyErr = nil
		p.ImageErr = errors.New("inspect failed")
		out, _, err = runCmdWithDeps(t, base, deps, "bg", "run", "--attach")

		if err == nil || out != "" || !slices.Equal(p.trace, []string{"daemon", "tty", "image"}) {
			t.Fatalf("failed preflight: stdout=%q trace=%v err=%v", out, p.trace, err)
		}
		if _, err := os.Stat(cacheDir); !os.IsNotExist(err) {
			t.Fatalf("failed preflight touched cache: %v", err)
		}

		p.ImageErr, p.trace = nil, nil
		var stdout, stderr bytes.Buffer
		p.beforeRun = func(ctx context.Context) {
			if _, ok := ctx.Deadline(); ok || p.NetworkCtxNoDeadline {
				t.Error("preflight deadline should apply only to preflight")
			}
			if info, err := os.Stat(cacheDir); err != nil || !info.IsDir() {
				t.Errorf("managed cache not prepared at entry to RunBG: %v", err)
			}
		}
		p.beforeAttach = func(id string) {
			if !strings.Contains(stderr.String(), id+"\n") {
				t.Errorf("recovery ID missing before attach: %q", stderr.String())
			}
		}
		code := runWithExitCodeAndDeps(base, &stdout, &stderr, deps, []string{"bg", "run", "--attach", "--quiet"})
		want := []string{"daemon", "tty", "image", "network:devnet", "run", "attach"}

		if code != 0 || stdout.Len() != 0 || !slices.Equal(p.trace, want) {
			t.Fatalf("launch: code=%d stdout=%q stderr=%q trace=%v", code, &stdout, &stderr, p.trace)
		}
	})
	t.Run("shared policy and preflight", func(t *testing.T) {
		base, root, ws := runCacheFixture(t)
		cache := filepath.Join(ws, "docs")
		writeCacheConfig(t, root, "cache:\n  local: [docs]\nnetworks: [devnet]\n")
		p := &bgPolicyProbe{bgCLIProbe: &bgCLIProbe{fakeDocker: newFakeDocker(0, true)}}
		p.Networks = map[string]bool{"devnet": true}
		deps := dockerDeps{api: p}
		out, _, err := runCmdWithDeps(t, base, deps, "bg", "run", "-n")
		if err != nil || !strings.Contains(out, "--network devnet") || len(p.checks) != 0 {
			t.Fatalf("dry run: %q, checks %v, err %v", out, p.checks, err)
		}

		if _, err := os.Stat(cache); !os.IsNotExist(err) {
			t.Fatalf("dry run created cache: %v", err)
		}

		writeCacheConfig(t, root, "cache:\n  static: [./missing:/opt/missing]\nnetworks: [devnet]\n")
		_, _, err = runCmdWithDeps(t, base, deps, "bg", "run")
		if err == nil || !slices.Equal(p.checks, []string{"daemon"}) {
			t.Fatalf("invalid mount: checks %v, err %v", p.checks, err)
		}

		writeCacheConfig(t, root, "cache:\n  local: [docs]\nnetworks: [devnet]\n")
		p.checks = nil
		_, _, err = runCmdWithDeps(t, base, deps, "bg", "run")
		if err != nil || !slices.Equal(p.checks, []string{"daemon", "image", "network:devnet"}) {
			t.Fatalf("run: checks %v, err %v", p.checks, err)
		}

		if _, err := os.Stat(cache); err != nil {
			t.Fatalf("cache not created: %v", err)
		}

		if _, _, err := runCmdWithDeps(t, base, deps, "run"); err != nil {
			t.Fatal(err)
		}

		for _, flag := range []string{"--mount", "--network"} {
			if !slices.Equal(previewFlags(p.bgPreview, flag), previewFlags(p.LastPreview, flag)) {
				t.Errorf("foreground/background %s values differ", flag)
			}
		}
	})
	t.Run("creation", func(t *testing.T) {
		setHomeToTestParent(t)
		t.Chdir(t.TempDir())
		base := t.TempDir()
		initWithImage(t, base)

		const shell = "/bin/fish"
		if _, _, err := runCmd(t, base, "config", "set", "shell", shell); err != nil {
			t.Fatal(err)
		}

		writeFile(t, ".makeslop.yaml", "environments:\n  static:\n    ZMX_DIR: /tmp/overridden\n    COLOR: blue\n")
		uuidLine := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\n$`)
		p := &bgCLIProbe{fakeDocker: newFakeDocker(0, true)}
		deps := dockerDeps{api: p}
		out, stderr, err := runCmdWithDeps(t, base, deps, "bg", "run")
		if err != nil || !uuidLine.MatchString(out) || stderr != "" || !slices.Equal(p.calls, []string{"run"}) {
			t.Fatalf("run: %q / %q / %v, %v", out, stderr, err, p.calls)
		}

		wantName := "makeslop-" + strings.TrimSpace(out)
		hasName := slices.Contains(previewFlags(p.bgPreview, "--name"), wantName)
		hasShell := strings.HasSuffix(p.bgPreview, "  "+shell)
		hasEnv := slices.Contains(previewFlags(p.bgPreview, "-e"), "COLOR=blue")
		if !hasName || !hasShell || !hasEnv {
			t.Fatalf("background spec does not match launch: %s", p.bgPreview)
		}

		another, _, err := runCmdWithDeps(t, base, deps, "bg", "run")
		if err != nil || another == out {
			t.Fatalf("two runs %q %q, %v", out, another, err)
		}

		p.calls = nil
		p.errorRun = errors.New("startup failed")

		if out, _, err := runCmdWithDeps(t, base, deps, "bg", "run"); out != "" || !errors.Is(err, p.errorRun) {
			t.Fatalf("startup: %q %v", out, err)
		}

		p.calls = nil
		p.errorRun = nil
		p.ttyErr = docker.ErrNoTTY
		out, _, err = runCmdWithDeps(t, base, deps, "bg", "run", "--attach")
		if out != "" || !errors.Is(err, docker.ErrNoTTY) || !slices.Equal(p.calls, []string{"tty"}) {
			t.Fatalf("non-tty attach run: %q %v %v", out, err, p.calls)
		}

		p.calls = nil
		out, _, err = runCmdWithDeps(t, base, deps, "bg", "run", "--dry-run", "--attach")
		hasCommand := strings.Contains(out, "--name makeslop-") && strings.Contains(out, shell)
		if err != nil || len(p.calls) != 0 || !hasCommand {
			t.Fatalf("dry run: %q %v %v", out, err, p.calls)
		}

		p.calls = nil
		p.ttyErr = nil
		p.errorAttach = errors.New("disconnected")
		cmd := newRootCmdWithDeps(base, deps)
		var stdout, errbuf bytes.Buffer
		cmd.SetArgs([]string{"bg", "run", "--attach", "--quiet"})
		cmd.SetOut(&stdout)
		cmd.SetErr(&errbuf)
		p.beforeAttach = func(id string) {
			if errbuf.String() != id+"\n" {
				t.Errorf("UUID not published before attach: %q", errbuf.String())
			}
		}
		err = cmd.ExecuteContext(t.Context())
		published := stdout.Len() == 0 && uuidLine.MatchString(errbuf.String())
		if !errors.Is(err, p.errorAttach) || !published || !slices.Equal(p.calls, []string{"tty", "run", "attach"}) {
			t.Fatalf("immediate attach: %v %q %q %v", err, stdout.String(), errbuf.String(), p.calls)
		}
	})
	t.Run("management without settings", func(t *testing.T) {
		base := t.TempDir()
		t.Chdir(t.TempDir())
		writeFile(t, filepath.Join(base, config.SettingsFile), "not-json")
		p := &bgCLIProbe{fakeDocker: newFakeDocker(0, false)}
		deps := dockerDeps{api: p}
		out, _, err := runCmdWithDeps(t, base, deps, "bg", "ls")
		if err != nil || !slices.Equal(strings.Fields(out), []string{"UUID", "PROJECT", "STATE", "IMAGE"}) {
			t.Fatalf("empty ls: %q, %v", out, err)
		}

		p.rows = []docker.BGContainer{{UUID: bgTestUUID, Project: "project\nname", State: "exited", Image: "img"}}
		out, _, err = runCmdWithDeps(t, base, deps, "bg", "ls")
		hasRow := strings.Contains(out, bgTestUUID) && strings.Contains(out, "project?name") &&
			strings.Contains(out, "exited")
		if err != nil || !hasRow {
			t.Fatalf("ls: %q, %v", out, err)
		}

		for _, verb := range []string{"start", "stop", "restart", "remove", "attach"} {
			p.calls = nil
			out, errText, err := runCmdWithDeps(t, base, deps, "bg", verb, bgTestUUID)
			silent := out == "" && errText == ""
			calledTarget := p.uuid == bgTestUUID && slices.Equal(p.calls, []string{verb})
			if err != nil || !silent || !calledTarget {
				t.Fatalf("%s: %q %q %v %v", verb, out, errText, err, p.calls)
			}

			if _, _, err := runCmdWithDeps(t, base, deps, "bg", verb, bgTestUUID, "extra"); err == nil {
				t.Fatalf("%s accepted extra args", verb)
			}
		}

		for _, tt := range []struct{ args, want []string }{
			{[]string{"--", "prog", "two words", "-n"}, []string{"prog", "two words", "-n"}},
			{[]string{"prog", "--", "-n"}, []string{"prog", "--", "-n"}},
			{[]string{"prog", "--"}, []string{"prog", "--"}},
			{[]string{"-n"}, []string{"-n"}},
		} {
			var out, stderr bytes.Buffer
			cmd := append([]string{"bg", "exec", bgTestUUID}, tt.args...)
			code := runWithExitCodeAndDeps(base, &out, &stderr, deps, cmd)
			if code != 23 || !slices.Equal(p.argv, tt.want) {
				t.Fatalf("exec %v: code %d argv %v stderr %q", tt.args, code, p.argv, stderr.String())
			}
		}

		if _, _, err := runCmdWithDeps(t, base, deps, "bg", "exec", "--", bgTestUUID); err == nil {
			t.Fatal("accepted delimiter before UUID")
		}

		bad := dockerDeps{api: dockerNewErrStub{errors.New("docker config failed")}}

		if _, _, err := runCmdWithDeps(t, base, bad, "version"); err != nil {
			t.Fatal(err)
		}

		if _, _, err := runCmdWithDeps(t, base, bad, "bg", "ls"); err == nil {
			t.Fatal("missing daemon error")
		}
	})
}
