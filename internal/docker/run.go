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

// isTTY uses ioctl-based term.IsTerminal rather than os.ModeCharDevice (which
// would also match /dev/null and /dev/zero).
func isTTY(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

// ExitError is returned by Run on non-zero container exit. Code is the daemon's
// status code (e.g. 137 for SIGKILL).
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("container exited with code %d", e.Code)
}

// runContainer implements (*Docker).Run with injected apiClient, TTY predicate,
// and raw-mode function. The caller owns the client lifetime.
func runContainer(ctx context.Context, cli apiClient, isTTYFn func() bool, makeRawFn func(int) (*term.State, error), s Spec) error {
	if !isTTYFn() {
		return ErrNoTTY
	}

	createRes, err := cli.ContainerCreate(ctx, moby.ContainerCreateOptions{
		Config:     s.ContainerConfig(),
		HostConfig: s.HostConfig(),
	})
	if err != nil {
		return fmt.Errorf("container create: %w", err)
	}
	id := createRes.ID

	// AutoRemove covers clean exits; this deferred force-remove covers pre-start
	// aborts, start failures, and context cancellation.
	startedCleanly := false
	defer func() {
		if !startedCleanly || ctx.Err() != nil {
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

	// Raw mode so the container gets unmodified input.
	fd := int(os.Stdin.Fd())
	oldState, err := makeRawFn(fd)
	if err != nil {
		return fmt.Errorf("set terminal raw mode: %w", err)
	}
	if oldState != nil {
		defer term.Restore(fd, oldState) //nolint:errcheck // teardown
	}

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

	// Forward terminal resize events. SIGWINCH is POSIX-only (absent on Windows).
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

	// Container uses a TTY, so the stream is NOT multiplexed.
	go io.Copy(att.Conn, os.Stdin)    //nolint:errcheck
	go io.Copy(os.Stdout, att.Reader) //nolint:errcheck

	wr := cli.ContainerWait(ctx, id, moby.ContainerWaitOptions{})

	select {
	case err := <-wr.Error:
		return fmt.Errorf("container wait: %w", err)
	case res := <-wr.Result:
		if res.Error != nil {
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
