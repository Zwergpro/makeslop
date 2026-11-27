package docker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	moby "github.com/moby/moby/client"
	"golang.org/x/term"
)

type bgExecClient struct {
	bgIdentityClient
	execID     string
	opts       moby.ExecCreateOptions
	attachOpts moby.ExecAttachOptions
	status     int
	connection net.Conn
	createErr  error
	resizedID  string
	resizeSize moby.ExecResizeOptions
	onAttach   func()
}

func (f *bgExecClient) ExecCreate(
	_ context.Context,
	id string,
	opts moby.ExecCreateOptions,
) (moby.ExecCreateResult, error) {
	f.execID = id
	f.opts = opts

	return moby.ExecCreateResult{ID: "exec-id"}, f.createErr
}

func (f *bgExecClient) ExecAttach(
	_ context.Context,
	_ string,
	opts moby.ExecAttachOptions,
) (moby.ExecAttachResult, error) {
	f.attachOpts = opts

	if f.onAttach != nil {
		f.onAttach()
	}

	return moby.ExecAttachResult{HijackedResponse: moby.NewHijackedResponse(f.connection, "")}, nil
}

func (f *bgExecClient) ExecInspect(
	_ context.Context,
	_ string,
	_ moby.ExecInspectOptions,
) (moby.ExecInspectResult, error) {
	return moby.ExecInspectResult{ExitCode: f.status}, nil
}

func (f *bgExecClient) ExecResize(
	_ context.Context,
	id string,
	opts moby.ExecResizeOptions,
) (moby.ExecResizeResult, error) {
	f.resizedID = id
	f.resizeSize = opts

	return moby.ExecResizeResult{}, nil
}

type bgHalfCloseConn struct {
	net.Conn
	eof chan struct{}
}

func (c bgHalfCloseConn) CloseWrite() error {
	close(c.eof)

	return nil
}

func TestBGExec(t *testing.T) {
	for _, tt := range []struct {
		name        string
		attach, tty bool
		state       container.ContainerState
		argv, want  []string
		code        int
		createErr   error
	}{
		{name: "no TTY", state: "running"},
		{name: "stopped", tty: true, state: "exited", attach: true},
		{
			name:   "new or existing session",
			tty:    true,
			state:  "running",
			attach: true,
			want:   []string{"zmx", "attach", "agent", "/bin/zsh"},
		},
		{name: "independent shell", tty: true, state: "running", want: []string{"/bin/zsh"}},
		{
			name:  "independent argv and exit",
			tty:   true,
			state: "running",
			argv:  []string{"cmd", "two words", "--", "-n"},
			want:  []string{"cmd", "two words", "--", "-n"},
			code:  23,
		},
		{
			name:      "container stops during attach",
			tty:       true,
			state:     "running",
			attach:    true,
			want:      []string{"zmx", "attach", "agent", "/bin/zsh"},
			createErr: errors.New("container is not running"),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			stdinEOF := make(chan struct{})
			f := &bgExecClient{
				bgIdentityClient: bgIdentityClient{result: bgInspection(testBGUUID, tt.state)},
				connection:       bgHalfCloseConn{a, stdinEOF},
				status:           tt.code,
				createErr:        tt.createErr,
			}
			var out bytes.Buffer
			d, err := New(
				WithClient(f),
				WithTTYCheck(func() bool { return tt.tty }),
				WithRawMode(func(int) (*term.State, error) { return nil, nil }),
				WithStreams(strings.NewReader(""), &out),
			)
			if err != nil {
				t.Fatal(err)
			}

			d.getTTYSize = func(int) (int, int, error) { return 91, 37, nil }
			joined := false
			d.resizeGoroutineHook = func() { joined = true }
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()

			if tt.want != nil && tt.createErr == nil {
				go func() {
					defer b.Close()

					select {
					case <-ctx.Done():
					case <-stdinEOF:
						_, _ = io.WriteString(b, "tail")
					}
				}()
			}

			if tt.attach {
				err = d.AttachBG(ctx, testBGUUID)
			} else {
				err = d.ExecBG(ctx, testBGUUID, tt.argv)
			}

			switch {
			case !tt.tty:
				if !errors.Is(err, ErrNoTTY) || f.execID != "" || f.inspected != "" {
					t.Fatalf("TTY: %v", err)
				}

				return
			case tt.state == "exited":
				if err == nil || !strings.Contains(err.Error(), "bg start") || f.execID != "" {
					t.Fatalf("stopped: %v", err)
				}

				return
			case tt.createErr != nil:
				if !errors.Is(err, tt.createErr) {
					t.Fatalf("attach race: %v", err)
				}

				return
			case tt.code != 0:
				var exit *ExitError

				if !errors.As(err, &exit) || exit.Code != tt.code {
					t.Fatalf("status: %v", err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
			}

			correctTarget := f.execID == "daemon-id" && f.opts.WorkingDir == "/workspace/demo" && f.opts.User == "1000"
			if !correctTarget || !f.opts.TTY || !slices.Equal(f.opts.Cmd, tt.want) {
				t.Fatalf("exec %q %+v", f.execID, f.opts)
			}

			if slices.Contains(f.opts.Env, "ZMX_DIR=/tmp/makeslop-zmx") != tt.attach {
				t.Fatalf("env: %v", f.opts.Env)
			}

			resized := f.resizedID == "exec-id" && f.resizeSize.Width == 91 && f.attachOpts.ConsoleSize.Height == 37
			if out.String() != "tail" || !resized || !joined {
				t.Fatalf("stream=%q resize=%q/%+v joined=%v", out.String(), f.resizedID, f.resizeSize, joined)
			}
		})
	}
}

func TestBGExecCancelledWhileIdle(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	stdin, write := io.Pipe()
	defer func() { _ = write.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f := &bgExecClient{
		bgIdentityClient: bgIdentityClient{result: bgInspection(testBGUUID, "running")},
		connection:       a,
		onAttach:         cancel,
	}
	d, err := New(
		WithClient(f),
		WithTTYCheck(func() bool { return true }),
		WithRawMode(func(int) (*term.State, error) { return nil, nil }),
		WithStreams(stdin, io.Discard),
	)
	if err != nil {
		t.Fatal(err)
	}

	if err := d.AttachBG(ctx, testBGUUID); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled idle attach: %v", err)
	}

	if _, err := write.Write([]byte("input")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("stdin wasn't closed: %v", err)
	}
}
