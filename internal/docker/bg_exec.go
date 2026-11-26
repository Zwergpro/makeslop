package docker

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	moby "github.com/moby/moby/client"
	"golang.org/x/term"
)

// CheckTTY verifies that both sides of an interactive attachment are terminals.
func (d *Docker) CheckTTY() error {
	if !d.isTTYFn() {
		return ErrNoTTY
	}
	return nil
}

// AttachBG connects to the named zmx terminal in an already-running container.
// Zmx may initialize the named terminal if it is missing.
func (d *Docker) AttachBG(ctx context.Context, uuid string) error {
	return d.execBG(ctx, uuid, nil, true)
}

// ExecBG runs an independent interactive process, defaulting to the configured shell.
func (d *Docker) ExecBG(ctx context.Context, uuid string, argv []string) error {
	return d.execBG(ctx, uuid, argv, false)
}

func (d *Docker) execBG(ctx context.Context, uuid string, argv []string, attach bool) error {
	if err := d.CheckTTY(); err != nil {
		return err
	}
	lookupCtx, cancel := WithPreflightTimeout(ctx)
	c, err := d.lookupBG(lookupCtx, uuid)
	cancel()
	if err != nil {
		return err
	}
	if c.stopped {
		return fmt.Errorf("session stopped; use 'makeslop bg start %s'", uuid)
	}
	if !c.running {
		return bgStateError(c)
	}
	var env []string
	if attach || len(argv) == 0 {
		if c.Shell == "" {
			return fmt.Errorf("background container %s has no configured shell metadata", uuid)
		}
		argv = []string{c.Shell}
	}
	if attach {
		argv = []string{"zmx", "attach", bgSession, c.Shell}
		env = bgZMXEnv
	}
	if err := d.runExecTTY(ctx, c, argv, env); err != nil {
		return err
	}
	return nil
}

func (d *Docker) runExecTTY(ctx context.Context, c BGContainer, argv, env []string) error {
	fd := int(os.Stdin.Fd())
	size := d.execConsoleSize(fd)
	created, err := d.client.ExecCreate(ctx, c.ID, moby.ExecCreateOptions{
		Cmd: argv, Env: env, User: c.user, WorkingDir: c.Workdir, TTY: true, ConsoleSize: size,
		AttachStdin: true, AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		return bgDaemonError("create terminal in", c.UUID, err)
	}
	state, err := d.makeRaw(fd)
	if err != nil {
		return fmt.Errorf("set terminal raw mode: %w", err)
	}
	if state != nil {
		defer func() { _ = term.Restore(fd, state) }()
	}
	stream, err := d.client.ExecAttach(ctx, created.ID, moby.ExecAttachOptions{TTY: true, ConsoleSize: size})
	if err != nil {
		return bgDaemonError("attach terminal in", c.UUID, err)
	}
	if err := d.streamExecTTY(ctx, created.ID, fd, &stream.HijackedResponse); err != nil {
		return err
	}
	return d.execExitStatus(ctx, c.UUID, created.ID)
}

func (d *Docker) execConsoleSize(fd int) moby.ConsoleSize {
	w, h, err := d.getTTYSize(fd)
	if err == nil && w > 0 && h > 0 {
		return moby.ConsoleSize{Height: uint(h), Width: uint(w)}
	}
	return moby.ConsoleSize{}
}

func (d *Docker) streamExecTTY(ctx context.Context, id string, fd int, stream *moby.HijackedResponse) error {
	streamCtx, cancel := context.WithCancel(ctx)
	stopClose := context.AfterFunc(streamCtx, stream.Close)
	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	resizeDone := make(chan struct{})
	go func() {
		defer close(resizeDone)
		if d.resizeGoroutineHook != nil {
			defer d.resizeGoroutineHook()
		}
		for {
			select {
			case <-streamCtx.Done():
				return
			case <-resize:
				d.resizeExecTTY(streamCtx, id, fd)
			}
		}
	}()

	stdin := newPollableStdin(d.stdin)
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		_, _ = io.Copy(stream.Conn, stdin.handle) // Closing the connection interrupts pending writes.
		_ = stream.CloseWrite()
	}()
	d.resizeExecTTY(streamCtx, id, fd)
	_, err := io.Copy(d.stdout, stream.Reader)
	cancel()
	stopClose()
	stream.Close()
	signal.Stop(resize)
	<-resizeDone
	if stdin.closer != nil {
		_ = stdin.closer.Close()
		<-inputDone
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("read background terminal: %w", err)
	}
	return nil
}

func (d *Docker) resizeExecTTY(ctx context.Context, id string, fd int) {
	if size := d.execConsoleSize(fd); size.Width != 0 && size.Height != 0 {
		resizeCtx, cancel := WithPreflightTimeout(ctx)
		defer cancel()
		_, _ = d.client.ExecResize(resizeCtx, id, moby.ExecResizeOptions(size))
	}
}

func (d *Docker) execExitStatus(ctx context.Context, uuid, id string) error {
	inspectCtx, cancel := WithPreflightTimeout(ctx)
	defer cancel()
	for {
		result, err := d.client.ExecInspect(inspectCtx, id, moby.ExecInspectOptions{})
		if err != nil {
			return bgDaemonError("inspect terminal exit in", uuid, err)
		}
		if !result.Running {
			if result.ExitCode != 0 {
				return &ExitError{Code: result.ExitCode}
			}
			return nil
		}
		select {
		case <-inspectCtx.Done():
			return fmt.Errorf("waiting for terminal exit in %s: %w", uuid, inspectCtx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}
