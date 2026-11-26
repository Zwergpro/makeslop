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
	spec                          docker.Spec
	calls                         []string
	rows                          []docker.BGContainer
	argv                          []string
	uuid                          string
	ttyErr, errorRun, errorAttach error
	beforeAttach                  func(string)
}

func (p *bgCLIProbe) CheckTTY() error { p.calls = append(p.calls, "tty"); return p.ttyErr }
func (p *bgCLIProbe) RunBG(_ context.Context, s docker.Spec) error {
	p.calls = append(p.calls, "run")
	p.spec = s
	return p.errorRun
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
}

func (p *bgPolicyProbe) CheckDaemon(ctx context.Context) error {
	p.checks = append(p.checks, "daemon")
	return p.fakeDocker.CheckDaemon(ctx)
}

func (p *bgPolicyProbe) ImageExists(ctx context.Context, image string) (bool, error) {
	p.checks = append(p.checks, "image")
	return p.fakeDocker.ImageExists(ctx, image)
}

func (p *bgPolicyProbe) NetworkExists(ctx context.Context, name string) (bool, error) {
	p.checks = append(p.checks, "network:"+name)
	return p.fakeDocker.NetworkExists(ctx, name)
}

func TestBGCommands(t *testing.T) {
	t.Run("shared policy and preflight", func(t *testing.T) {
		base, root, ws := runCacheFixture(t)
		cache := filepath.Join(ws, "docs")
		writeCacheConfig(t, root, "cache:\n  local: [docs]\nnetworks: [devnet]\n")
		p := &bgPolicyProbe{bgCLIProbe: &bgCLIProbe{fakeDocker: newFakeDocker(0, true)}}
		p.Networks = map[string]bool{"devnet": true}
		deps := dockerDeps{api: p}
		if out, _, err := runCmdWithDeps(t, base, deps, "bg", "run", "-n"); err != nil || !strings.Contains(out, "--network devnet") || len(p.checks) != 0 {
			t.Fatalf("dry run: %q, checks %v, err %v", out, p.checks, err)
		}
		if _, err := os.Stat(cache); !os.IsNotExist(err) {
			t.Fatalf("dry run created cache: %v", err)
		}
		writeCacheConfig(t, root, "cache:\n  static: [./missing:/opt/missing]\nnetworks: [devnet]\n")
		if _, _, err := runCmdWithDeps(t, base, deps, "bg", "run"); err == nil || !slices.Equal(p.checks, []string{"daemon"}) {
			t.Fatalf("invalid mount: checks %v, err %v", p.checks, err)
		}
		writeCacheConfig(t, root, "cache:\n  local: [docs]\nnetworks: [devnet]\n")
		p.checks = nil
		if _, _, err := runCmdWithDeps(t, base, deps, "bg", "run"); err != nil || !slices.Equal(p.checks, []string{"daemon", "image", "network:devnet"}) {
			t.Fatalf("run: checks %v, err %v", p.checks, err)
		}
		if _, err := os.Stat(cache); err != nil {
			t.Fatalf("cache not created: %v", err)
		}
		if _, _, err := runCmdWithDeps(t, base, deps, "run"); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(p.spec.Mounts, p.LastSpec.Mounts) || !slices.Equal(p.spec.Networks, p.LastSpec.Networks) {
			t.Fatalf("foreground/background policy differs: %+v / %+v", p.spec.Mounts, p.LastSpec.Mounts)
		}
	})
	t.Run("creation", func(t *testing.T) {
		setHomeToTestParent(t)
		t.Chdir(t.TempDir())
		base := t.TempDir()
		initWithImage(t, base)
		if _, _, err := runCmd(t, base, "config", "set", "shell", "/bin/fish"); err != nil {
			t.Fatal(err)
		}
		writeFile(t, ".makeslop.yaml", "environments:\n  static:\n    ZMX_DIR: /tmp/overridden\n    COLOR: blue\n")
		uuidLine := regexp.MustCompile(`^[0-9a-f-]{36}\n$`)
		p := &bgCLIProbe{fakeDocker: newFakeDocker(0, true)}
		deps := dockerDeps{api: p}
		out, stderr, err := runCmdWithDeps(t, base, deps, "bg", "run")
		if err != nil || !uuidLine.MatchString(out) || stderr != "" || !slices.Equal(p.calls, []string{"run"}) {
			t.Fatalf("run: %q / %q / %v, %v", out, stderr, err, p.calls)
		}
		cfg := p.spec.ContainerConfig()
		if p.spec.Command != "/bin/fish" || cfg.Labels["io.makeslop.bg.shell"] != "/bin/fish" || !slices.Equal(cfg.Cmd, []string{"zmx", "attach", "agent", "/bin/fish"}) || !slices.Contains(cfg.Env, "COLOR=blue") || !slices.Contains(cfg.Env, "ZMX_DIR=/tmp/makeslop-zmx") || slices.Contains(cfg.Env, "ZMX_DIR=/tmp/overridden") {
			t.Fatalf("cfg: %+v", cfg)
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
		if out, _, err := runCmdWithDeps(t, base, deps, "bg", "run", "--attach"); out != "" || !errors.Is(err, docker.ErrNoTTY) || !slices.Equal(p.calls, []string{"tty"}) {
			t.Fatalf("non-tty attach run: %q %v %v", out, err, p.calls)
		}
		p.calls = nil
		out, _, err = runCmdWithDeps(t, base, deps, "bg", "run", "--dry-run", "--attach")
		if err != nil || len(p.calls) != 0 || !strings.Contains(out, "--name makeslop-") || !strings.Contains(out, "/bin/fish") {
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
		if err := cmd.ExecuteContext(t.Context()); !errors.Is(err, p.errorAttach) || stdout.Len() != 0 || !uuidLine.MatchString(errbuf.String()) || !slices.Equal(p.calls, []string{"tty", "run", "attach"}) {
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
		if err != nil || !strings.Contains(out, bgTestUUID) || !strings.Contains(out, "project?name") || !strings.Contains(out, "exited") {
			t.Fatalf("ls: %q, %v", out, err)
		}
		for _, verb := range []string{"start", "stop", "restart", "remove", "attach"} {
			p.calls = nil
			out, errText, err := runCmdWithDeps(t, base, deps, "bg", verb, bgTestUUID)
			if err != nil || out != "" || errText != "" || p.uuid != bgTestUUID || !slices.Equal(p.calls, []string{verb}) {
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
			if code := runWithExitCodeAndDeps(base, &out, &stderr, deps, cmd); code != 23 || !slices.Equal(p.argv, tt.want) {
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
