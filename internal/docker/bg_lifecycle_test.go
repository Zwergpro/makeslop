package docker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	moby "github.com/moby/moby/client"
)

func TestBGSpec(t *testing.T) {
	in := modeInputs()
	in.Shell = "/bin/a shell"
	in.Env = []string{"X=ok", "ZMX_DIR=/tmp/user-controlled"}
	in.Network.Networks = []string{"one", "two"}
	in.Layout.Main.MaskedDirs = []string{"/host/demo/private"}
	fg := testForegroundCreate(in)
	spec, err := BuildBackground(in, testBGUUID)
	if err != nil {
		t.Fatal(err)
	}
	bg := spec.create
	if !fg.HostConfig().AutoRemove || bg.HostConfig().AutoRemove {
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

	privateSocket := strings.Count(strings.Join(cfg.Env, "\n"), "ZMX_DIR=") == 1 &&
		slices.Contains(cfg.Env, "ZMX_DIR=/tmp/makeslop-zmx")
	if !privateSocket || !slices.Contains(cfg.Env, "X=ok") || fg.Env[1] != "ZMX_DIR=/tmp/user-controlled" {
		t.Fatalf("bg/fg env: %v / %v", cfg.Env, fg.Env)
	}

	argv := bg.Args()
	detached := !slices.Contains(argv, "--rm") && slices.Contains(argv, "-dit")
	if !slices.Equal(argv[len(argv)-4:], want) || !detached || !slices.Contains(argv, "makeslop-"+testBGUUID) {
		t.Fatalf("bg argv: %v", argv)
	}

	dry := bg.ShellCommand()
	if !strings.HasSuffix(dry, "  '/bin/a shell'") || strings.Contains(dry, "user-controlled") {
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
	calls           []string
	createOptions   moby.ContainerCreateOptions
	mutationID      string
	removed         string
	forced          bool
	cleanupCtxErr   error
	cleanupErr      error
	startErr        error
	startBounded    bool
	lookupBounded   bool
	probeStarted    bool
	onCreate        func()
	probeExitCode   int
	probeInspectErr error
	probeStartErr   error
	probeErr        error
	probeOptions    moby.ExecCreateOptions
}

func (f *bgLifecycleClient) ContainerCreate(
	_ context.Context,
	opts moby.ContainerCreateOptions,
) (moby.ContainerCreateResult, error) {
	f.calls = append(f.calls, "create")
	f.createOptions = opts

	if f.onCreate != nil {
		f.onCreate()
	}

	return moby.ContainerCreateResult{ID: "new-id"}, nil
}

func (f *bgLifecycleClient) ContainerInspect(
	ctx context.Context,
	id string,
	_ moby.ContainerInspectOptions,
) (moby.ContainerInspectResult, error) {
	f.calls = append(f.calls, "inspect")
	f.inspected = id

	if id == bgPrefix+testBGUUID {
		_, f.lookupBounded = ctx.Deadline()
	}

	return f.result, f.inspectErr
}

func (f *bgLifecycleClient) ContainerStart(
	ctx context.Context,
	id string,
	_ moby.ContainerStartOptions,
) (moby.ContainerStartResult, error) {
	f.calls = append(f.calls, "start")
	f.mutationID = id
	_, f.startBounded = ctx.Deadline()

	if ctx.Err() != nil {
		return moby.ContainerStartResult{}, ctx.Err()
	}

	if f.startErr == nil {
		f.result = bgInspection(testBGUUID, "running")
	}

	return moby.ContainerStartResult{}, f.startErr
}

func (f *bgLifecycleClient) ContainerStop(
	_ context.Context,
	id string,
	_ moby.ContainerStopOptions,
) (moby.ContainerStopResult, error) {
	f.calls = append(f.calls, "stop")
	f.mutationID = id
	f.result = bgInspection(testBGUUID, "exited")

	return moby.ContainerStopResult{}, nil
}

func (f *bgLifecycleClient) ContainerRemove(
	ctx context.Context,
	id string,
	opts moby.ContainerRemoveOptions,
) (moby.ContainerRemoveResult, error) {
	f.calls = append(f.calls, "remove")
	f.removed = id
	f.forced = opts.Force
	f.cleanupCtxErr = ctx.Err()

	return moby.ContainerRemoveResult{}, f.cleanupErr
}

func (f *bgLifecycleClient) ExecCreate(
	_ context.Context,
	_ string,
	opts moby.ExecCreateOptions,
) (moby.ExecCreateResult, error) {
	f.calls = append(f.calls, "probe")
	f.probeOptions = opts

	return moby.ExecCreateResult{ID: "probe-id"}, f.probeErr
}

func (f *bgLifecycleClient) ExecInspect(
	_ context.Context,
	_ string,
	_ moby.ExecInspectOptions,
) (moby.ExecInspectResult, error) {
	return moby.ExecInspectResult{ExitCode: f.probeExitCode}, f.probeInspectErr
}

func (f *bgLifecycleClient) ExecStart(
	_ context.Context,
	id string,
	opts moby.ExecStartOptions,
) (moby.ExecStartResult, error) {
	f.probeStarted = opts.Detach && id == "probe-id"

	return moby.ExecStartResult{}, f.probeStartErr
}

func TestBGStartProbeError(t *testing.T) {
	cause := errors.New("daemon unavailable")
	for name, f := range map[string]*bgLifecycleClient{
		"create": {probeErr: cause}, "start": {probeStartErr: cause}, "inspect": {probeInspectErr: cause},
	} {
		t.Run(name, func(t *testing.T) {
			f.result = bgInspection(testBGUUID, "running")
			err := (&Docker{client: f}).StartBG(t.Context(), testBGUUID)
			if !errors.Is(err, cause) || strings.Contains(err.Error(), "restart") {
				t.Fatalf("probe failure: %v", err)
			}
		})
	}
}

func TestBGProbeStalledResponse(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/daemon-id/exec"):
			_, _ = w.Write([]byte(`{"Id":"probe-id"}`))
		case strings.HasSuffix(r.URL.Path, "/exec/probe-id/start"):
			close(started)

			select {
			case <-r.Context().Done():
			case <-release:
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	defer close(release)
	cli, err := moby.New(moby.WithHost("tcp://"+srv.Listener.Addr().String()), moby.WithAPIVersion("1.48"))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := cli.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := (&Docker{client: cli}).bgSessionReady(ctx, "daemon-id"); done <- err }()

	select {
	case <-started:
	case err := <-done:
		t.Fatalf("probe did not start: %v", err)
	case <-time.After(time.Second):
		t.Fatal("probe did not reach daemon")
	}

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("probe error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stalled response ignored cancellation")
	}
}

func TestBGLifecycle(t *testing.T) {
	for _, tt := range []struct {
		name          string
		state         container.ContainerState
		action        func(*Docker, context.Context, string) error
		calls         []string
		wantErr       string
		probeExitCode int
	}{
		{"stop running", "running", (*Docker).StopBG, []string{"inspect", "stop"}, "", 0},
		{"stop stopped", "exited", (*Docker).StopBG, []string{"inspect"}, "", 0},
		{"start ready", "running", (*Docker).StartBG, []string{"inspect", "probe"}, "", 0},
		{"start running unready", "running", (*Docker).StartBG, []string{"inspect", "probe"}, "bg restart", 1},
		{"probe could not execute", "running", (*Docker).StartBG, []string{"inspect", "probe"}, "status 126", 126},
		{"start stopped", "exited", (*Docker).StartBG, []string{"inspect", "start", "inspect", "probe"}, "", 0},
		{
			name:   "restart running",
			state:  "running",
			action: (*Docker).RestartBG,
			calls:  []string{"inspect", "stop", "start", "inspect", "probe"},
		},
		{"restart stopped", "exited", (*Docker).RestartBG, []string{"inspect", "start", "inspect", "probe"}, "", 0},
		{"remove stopped", "exited", (*Docker).RemoveBG, []string{"inspect", "remove"}, "", 0},
		{"remove running", "running", (*Docker).RemoveBG, []string{"inspect"}, "bg stop", 0},
		{"remove paused", "paused", (*Docker).RemoveBG, []string{"inspect"}, "paused", 0},
		{"start restarting", "restarting", (*Docker).StartBG, []string{"inspect"}, "restarting", 0},
		{"stop removing", "removing", (*Docker).StopBG, []string{"inspect"}, "removing", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &bgLifecycleClient{
				bgIdentityClient: bgIdentityClient{result: bgInspection(testBGUUID, tt.state)},
				probeExitCode:    tt.probeExitCode,
			}
			d := &Docker{client: f}
			err := tt.action(d, t.Context(), testBGUUID)
			unexpectedError := tt.wantErr == "" && err != nil
			missingError := tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr))
			if unexpectedError || missingError {
				t.Fatalf("error %v, want %q", err, tt.wantErr)
			}

			if !f.lookupBounded {
				t.Error("identity lookup has no deadline")
			}

			if !slices.Equal(f.calls, tt.calls) {
				t.Fatalf("calls %v, want %v", f.calls, tt.calls)
			}

			wrongTarget := f.mutationID != "" && f.mutationID != "daemon-id"
			unsafeRemoval := f.removed != "" && (f.removed != "daemon-id" || f.forced)
			if wrongTarget || unsafeRemoval {
				t.Fatal("wrong lifecycle mutation")
			}
		})
	}
}

func TestBGRestartStartFailureKeepsContainer(t *testing.T) {
	f := &bgLifecycleClient{
		bgIdentityClient: bgIdentityClient{result: bgInspection(testBGUUID, "running")},
		startErr:         context.DeadlineExceeded,
	}
	d := &Docker{client: f}
	err := d.RestartBG(t.Context(), testBGUUID)
	if !f.startBounded || !errors.Is(err, context.DeadlineExceeded) || f.removed != "" {
		t.Fatalf("restart: deadline=%v, removed=%q, err=%v", f.startBounded, f.removed, err)
	}
}

func TestBGRun(t *testing.T) {
	for _, name := range []string{"success", "cancelled", "cleanup failed", "missing zmx"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			f := &bgLifecycleClient{}
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

			in := modeInputs()
			in.Shell = "/bin/zsh"
			spec, err := BuildBackground(in, testBGUUID)
			if err != nil {
				t.Fatal(err)
			}
			err = d.RunBG(ctx, spec)

			if f.createOptions.Name != "makeslop-"+testBGUUID || f.mutationID != "new-id" {
				t.Fatalf("create/start: %+v, %q", f.createOptions, f.mutationID)
			}

			if name == "success" {
				got := f.createOptions
				if !reflect.DeepEqual(got, spec.create.createOptions()) {
					t.Fatalf("runner create differs from planned options: %+v", got)
				}

				wantCalls := []string{"create", "start", "inspect", "probe"}
				if err != nil || f.removed != "" || !slices.Equal(f.calls, wantCalls) {
					t.Fatalf("success: %v, %v", f.calls, err)
				}

				queriedSession := f.probeStarted && slices.Equal(f.probeOptions.Cmd, []string{"zmx", "get", "agent"})
				if !queriedSession || !slices.Contains(f.probeOptions.Env, "ZMX_DIR=/tmp/makeslop-zmx") {
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
