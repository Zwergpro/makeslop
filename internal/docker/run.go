package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	moby "github.com/moby/moby/client"
	"golang.org/x/term"
)

var ErrNoTTY = errors.New("interactive TTY required on stdin and stdout")

var (
	ttyCheck = func() bool { return isTTY(os.Stdin) && isTTY(os.Stdout) }

	// termMakeRaw wraps term.MakeRaw so tests can stub it (e.g. when there is
	// no real PTY available). The default is the real implementation.
	termMakeRaw = func(fd int) (*term.State, error) { return term.MakeRaw(fd) }
)

// Uses term.IsTerminal (ioctl-based) rather than os.ModeCharDevice, which
// also matches /dev/null and /dev/zero.
func isTTY(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

// ExitError is returned by Run when the container exits with a non-zero status
// code. Code is the exit status reported by the daemon (e.g. 137 for SIGKILL).
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("container exited with code %d", e.Code)
}

// Run launches the container described by s interactively. It refuses to start
// (returning ErrNoTTY) unless both stdin and stdout are TTYs.
// On non-zero container exit, Run returns *ExitError with the exit code.
func Run(ctx context.Context, s Spec) error {
	cli, err := newClientFn()
	if err != nil {
		return fmt.Errorf("create docker client: %w", err)
	}
	return run(ctx, cli, s)
}

// run is the internal implementation of Run with an injected apiClient (for tests).
func run(ctx context.Context, cli apiClient, s Spec) error {
	if !ttyCheck() {
		return ErrNoTTY
	}
	defer cli.Close() //nolint:errcheck // teardown; error not actionable here

	// Create container (but do not start it yet).
	createRes, err := cli.ContainerCreate(ctx, moby.ContainerCreateOptions{
		Config:     s.ContainerConfig(),
		HostConfig: s.HostConfig(),
	})
	if err != nil {
		return fmt.Errorf("container create: %w", err)
	}
	id := createRes.ID

	// Track whether container started successfully so the deferred remove
	// knows when to fire. AutoRemove handles cleanup on all exits; the deferred
	// remove here handles pre-start aborts, start failures, and ctx cancellation.
	startedCleanly := false
	defer func() {
		if !startedCleanly || ctx.Err() != nil {
			// Best-effort force-remove to avoid leaked containers on
			// pre-start abort, start failure, or context cancellation.
			_, _ = cli.ContainerRemove(context.Background(), id, moby.ContainerRemoveOptions{Force: true})
		}
	}()

	// Attach before starting so we don't miss any output.
	att, err := cli.ContainerAttach(ctx, id, moby.ContainerAttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		return fmt.Errorf("container attach: %w", err)
	}
	defer att.Conn.Close() //nolint:errcheck // teardown

	// Put the terminal in raw mode so the container gets unmodified input.
	fd := int(os.Stdin.Fd())
	oldState, err := termMakeRaw(fd)
	if err != nil {
		return fmt.Errorf("set terminal raw mode: %w", err)
	}
	if oldState != nil {
		defer term.Restore(fd, oldState) //nolint:errcheck // teardown
	}

	// Start the container.
	if _, err = cli.ContainerStart(ctx, id, moby.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("container start: %w", err)
	}
	startedCleanly = true

	// Send initial terminal size.
	if w, h, sizeErr := term.GetSize(fd); sizeErr == nil {
		_, _ = cli.ContainerResize(ctx, id, moby.ContainerResizeOptions{
			Height: uint(h),
			Width:  uint(w),
		})
	}

	// Install SIGWINCH handler to forward terminal resize events.
	// POSIX-only: SIGWINCH is not available on Windows (guarded by build tag in signal support).
	if runtime.GOOS != "windows" {
		winchCh := make(chan os.Signal, 1)
		signal.Notify(winchCh, syscall.SIGWINCH)
		go func() {
			for range winchCh {
				if w, h, sizeErr := term.GetSize(fd); sizeErr == nil {
					_, _ = cli.ContainerResize(ctx, id, moby.ContainerResizeOptions{
						Height: uint(h),
						Width:  uint(w),
					})
				}
			}
		}()
		defer func() {
			signal.Stop(winchCh)
			close(winchCh)
		}()
	}

	// Pump I/O: container uses a TTY so the stream is NOT multiplexed.
	go io.Copy(att.Conn, os.Stdin)    //nolint:errcheck
	go io.Copy(os.Stdout, att.Reader) //nolint:errcheck

	// Wait for the container to exit.
	wr := cli.ContainerWait(ctx, id, moby.ContainerWaitOptions{})

	select {
	case err := <-wr.Error:
		return fmt.Errorf("container wait: %w", err)
	case res := <-wr.Result:
		if res.Error != nil {
			// WaitExitError carries a message string from the daemon.
			return fmt.Errorf("container wait error: %s", res.Error.Message)
		}
		if res.StatusCode != 0 {
			return &ExitError{Code: int(res.StatusCode)}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
