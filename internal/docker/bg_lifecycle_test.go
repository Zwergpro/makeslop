package docker

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	moby "github.com/moby/moby/client"
)

func TestBGSpec(t *testing.T) {
	fg := Spec{Image: "local:test", Command: "/bin/a shell", Workdir: "/workspace/demo", Env: []string{"X=ok", "ZMX_DIR=/tmp/user-controlled"}, CapDrop: []string{"ALL"}, SecOpt: []string{"no-new-privileges"}, Networks: []string{"one", "two"}, Mounts: []Mount{{Type: "tmpfs", Container: "/workspace/demo/private"}}}
	bg := fg.ForBackground(testBGUUID, "demo")
	if fg.ZMXArgv != nil || !fg.HostConfig().AutoRemove || bg.HostConfig().AutoRemove {
		t.Fatal("foreground/background auto-remove mismatch")
	}
	cfg := bg.ContainerConfig()
	want := []string{"zmx", "attach", "agent", "/bin/a shell"}
	if !slices.Equal(cfg.Cmd, want) || cfg.Entrypoint != nil || !cfg.OpenStdin || !cfg.Tty {
		t.Fatalf("bg config: %+v", cfg)
	}
	if cfg.Labels["io.makeslop.bg.shell"] != "/bin/a shell" {
		t.Fatal("original shell not stored")
	}
	if !slices.Contains(cfg.Env, "X=ok") || strings.Count(strings.Join(cfg.Env, "\n"), "ZMX_DIR=") != 1 || !slices.Contains(cfg.Env, "ZMX_DIR=/tmp/makeslop-zmx") || fg.Env[1] != "ZMX_DIR=/tmp/user-controlled" {
		t.Fatalf("bg/fg env: %v / %v", cfg.Env, fg.Env)
	}
	argv := bg.Args()
	if !slices.Equal(argv[len(argv)-4:], want) || slices.Contains(argv, "--rm") || !slices.Contains(argv, "-dit") || !slices.Contains(argv, "makeslop-"+testBGUUID) {
		t.Fatalf("bg argv: %v", argv)
	}
	if dry := bg.ShellCommand(); !strings.HasSuffix(dry, "  '/bin/a shell'") || strings.Contains(dry, "user-controlled") {
		t.Fatalf("dry run: %s", dry)
	}
	hc := bg.HostConfig()
	hc.AutoRemove = true
	if !reflect.DeepEqual(hc, fg.HostConfig()) || !reflect.DeepEqual(bg.NetworkingConfig(), fg.NetworkingConfig()) {
		t.Fatal("background changed launch policy")
	}
}

type bgLifecycleClient struct {
	bgIdentityClient
	calls         []string
	createOptions moby.ContainerCreateOptions
	mutationID    string
	removed       string
	forced        bool
	cleanupCtxErr error
	cleanupErr    error
	startErr      error
	onCreate      func()
	probeOutput   string
	probeErr      error
	probeOptions  moby.ExecCreateOptions
}

func (f *bgLifecycleClient) ContainerCreate(_ context.Context, opts moby.ContainerCreateOptions) (moby.ContainerCreateResult, error) {
	f.calls = append(f.calls, "create")
	f.createOptions = opts
	if f.onCreate != nil {
		f.onCreate()
	}
	return moby.ContainerCreateResult{ID: "new-id"}, nil
}
func (f *bgLifecycleClient) ContainerInspect(_ context.Context, id string, _ moby.ContainerInspectOptions) (moby.ContainerInspectResult, error) {
	f.calls = append(f.calls, "inspect")
	f.inspected = id
	return f.result, f.inspectErr
}
func (f *bgLifecycleClient) ContainerStart(ctx context.Context, id string, _ moby.ContainerStartOptions) (moby.ContainerStartResult, error) {
	f.calls = append(f.calls, "start")
	f.mutationID = id
	if ctx.Err() != nil {
		return moby.ContainerStartResult{}, ctx.Err()
	}
	if f.startErr == nil {
		f.result = bgInspection(testBGUUID, "running")
	}
	return moby.ContainerStartResult{}, f.startErr
}
func (f *bgLifecycleClient) ContainerStop(_ context.Context, id string, _ moby.ContainerStopOptions) (moby.ContainerStopResult, error) {
	f.calls = append(f.calls, "stop")
	f.mutationID = id
	f.result = bgInspection(testBGUUID, "exited")
	return moby.ContainerStopResult{}, nil
}
func (f *bgLifecycleClient) ContainerRemove(ctx context.Context, id string, opts moby.ContainerRemoveOptions) (moby.ContainerRemoveResult, error) {
	f.calls = append(f.calls, "remove")
	f.removed = id
	f.forced = opts.Force
	f.cleanupCtxErr = ctx.Err()
	return moby.ContainerRemoveResult{}, f.cleanupErr
}
func (f *bgLifecycleClient) ExecCreate(_ context.Context, _ string, opts moby.ExecCreateOptions) (moby.ExecCreateResult, error) {
	f.calls = append(f.calls, "probe")
	f.probeOptions = opts
	return moby.ExecCreateResult{ID: "probe-id"}, f.probeErr
}
func (f *bgLifecycleClient) ExecAttach(_ context.Context, _ string, _ moby.ExecAttachOptions) (moby.ExecAttachResult, error) {
	a, b := net.Pipe()
	frame := make([]byte, 8, 8+len(f.probeOutput))
	frame[0] = 1 // Docker stdout stream
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(f.probeOutput)))
	frame = append(frame, f.probeOutput...)
	go func() { defer b.Close(); _, _ = b.Write(frame) }()
	return moby.ExecAttachResult{HijackedResponse: moby.HijackedResponse{Conn: a, Reader: bufio.NewReader(a)}}, nil
}
func (f *bgLifecycleClient) ExecInspect(_ context.Context, _ string, _ moby.ExecInspectOptions) (moby.ExecInspectResult, error) {
	return moby.ExecInspectResult{}, nil
}

func TestBGLifecycle(t *testing.T) {
	for _, tt := range []struct {
		name    string
		state   container.ContainerState
		action  func(*Docker, context.Context, string) error
		calls   []string
		wantErr string
		probe   string
	}{
		{"stop running", "running", (*Docker).StopBG, []string{"inspect", "stop"}, "", ""},
		{"stop stopped", "exited", (*Docker).StopBG, []string{"inspect"}, "", ""},
		{"start ready", "running", (*Docker).StartBG, []string{"inspect", "probe"}, "", "agent\n"},
		{"start running unready", "running", (*Docker).StartBG, []string{"inspect", "probe"}, "bg restart", "other\n"},
		{"start stopped", "exited", (*Docker).StartBG, []string{"inspect", "start", "inspect", "probe"}, "", "agent\n"},
		{"restart running", "running", (*Docker).RestartBG, []string{"inspect", "stop", "start", "inspect", "probe"}, "", "agent\n"},
		{"restart stopped", "exited", (*Docker).RestartBG, []string{"inspect", "start", "inspect", "probe"}, "", "agent\n"},
		{"remove stopped", "exited", (*Docker).RemoveBG, []string{"inspect", "remove"}, "", ""},
		{"remove running", "running", (*Docker).RemoveBG, []string{"inspect"}, "bg stop", ""},
		{"remove paused", "paused", (*Docker).RemoveBG, []string{"inspect"}, "paused", ""},
		{"start restarting", "restarting", (*Docker).StartBG, []string{"inspect"}, "restarting", ""},
		{"stop removing", "removing", (*Docker).StopBG, []string{"inspect"}, "removing", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &bgLifecycleClient{bgIdentityClient: bgIdentityClient{result: bgInspection(testBGUUID, tt.state)}, probeOutput: tt.probe}
			d := &Docker{client: f}
			err := tt.action(d, t.Context(), testBGUUID)
			if tt.wantErr == "" && err != nil || tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("error %v, want %q", err, tt.wantErr)
			}
			if !slices.Equal(f.calls, tt.calls) {
				t.Fatalf("calls %v, want %v", f.calls, tt.calls)
			}
			if f.mutationID != "" && f.mutationID != "daemon-id" || f.removed != "" && (f.removed != "daemon-id" || f.forced) {
				t.Fatal("wrong lifecycle mutation")
			}
		})
	}
}

func TestBGRun(t *testing.T) {
	for _, name := range []string{"success", "cancelled", "cleanup failed", "missing zmx"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			f := &bgLifecycleClient{probeOutput: "agent\n"}
			d := &Docker{client: f}
			if name == "cancelled" || name == "cleanup failed" {
				f.onCreate = cancel
			}
			if name == "cleanup failed" {
				f.cleanupErr = errors.New("not removed")
			}
			if name == "missing zmx" {
				f.startErr = errors.New("zmx: executable file not found")
			}
			err := d.RunBG(ctx, Spec{Command: "/bin/zsh"}.ForBackground(testBGUUID, "demo"))
			if f.createOptions.Name != "makeslop-"+testBGUUID || f.mutationID != "new-id" {
				t.Fatalf("create/start: %+v, %q", f.createOptions, f.mutationID)
			}
			if name == "success" {
				if err != nil || f.removed != "" || !slices.Equal(f.calls, []string{"create", "start", "inspect", "probe"}) {
					t.Fatalf("success: %v, %v", f.calls, err)
				}
				if !slices.Equal(f.probeOptions.Cmd, []string{"zmx", "list", "--short"}) || !slices.Contains(f.probeOptions.Env, "ZMX_DIR=/tmp/makeslop-zmx") {
					t.Fatalf("probe %+v", f.probeOptions)
				}
			} else if err == nil || f.removed != "new-id" || !f.forced || f.cleanupCtxErr != nil {
				t.Fatalf("cleanup: %v, %v", f.calls, err)
			}
			if name == "cleanup failed" && !strings.Contains(err.Error(), testBGUUID) {
				t.Fatalf("recovery UUID missing: %v", err)
			}
		})
	}
}
