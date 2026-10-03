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
	"time"

	"github.com/moby/moby/api/types/container"
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

// pollableStdin lets Run stop a pending stdin read before returning.
type pollableStdin struct {
	handle io.Reader // read side; may be a *os.File (fresh tty open) or the original reader
	closer io.Closer // handle's close method (nil if the reader is not closeable)
}

// newPollableStdin opens a fresh /dev/tty handle because O_NONBLOCK on fd 0
// would also affect stdout: terminal fds may share an open file description.
// The fresh handle must match stdin's device and support poller deadlines;
// otherwise Close cannot reliably unblock Read, so Run uses the original reader.
func newPollableStdin(stdinReader io.Reader) pollableStdin {
	// Test readers need no tty open and may already be closeable.
	if stdinReader != os.Stdin {
		if c, isCloser := stdinReader.(io.Closer); isCloser {
			return pollableStdin{handle: stdinReader, closer: c}
		}
		return pollableStdin{handle: stdinReader}
	}

	tty, err := os.OpenFile("/dev/tty", os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return pollableStdin{handle: stdinReader}
	}

	// stdin could be a terminal other than the controlling one (exotic
	// redirection); reading /dev/tty would then consume the wrong input.
	if !sameCharDevice(os.Stdin, tty) {
		_ = tty.Close()
		return pollableStdin{handle: stdinReader}
	}

	// Deadlines are poller-backed: an ErrNoDeadline here means the runtime
	// poller rejected the fd and Close would not unblock a pending Read.
	if tty.SetReadDeadline(time.Time{}) != nil {
		_ = tty.Close()
		return pollableStdin{handle: stdinReader}
	}

	return pollableStdin{handle: tty, closer: tty}
}

// sameCharDevice uses SyscallConn because File.Fd may restore blocking mode.
func sameCharDevice(a, b *os.File) bool {
	var sa, sb syscall.Stat_t
	rca, err := a.SyscallConn()
	if err != nil {
		return false
	}
	var aerr error
	if rca.Control(func(fd uintptr) { aerr = syscall.Fstat(int(fd), &sa) }) != nil || aerr != nil {
		return false
	}
	rcb, err := b.SyscallConn()
	if err != nil {
		return false
	}
	var berr error
	if rcb.Control(func(fd uintptr) { berr = syscall.Fstat(int(fd), &sb) }) != nil || berr != nil {
		return false
	}
	return sa.Rdev == sb.Rdev
}

// Run starts the wait before the container so fast auto-removed exits retain
// their status. It drains output and joins closeable stdin readers before return
// to avoid lost output and goroutine leaks. If stdin cannot be made closeable,
// its blocked copy goroutine may outlive Run.
func (d *Docker) Run(ctx context.Context, s Spec) error {
	cli := d.client
	if !d.isTTYFn() {
		return ErrNoTTY
	}

	createRes, err := cli.ContainerCreate(ctx, moby.ContainerCreateOptions{
		Config:           s.ContainerConfig(),
		HostConfig:       s.HostConfig(),
		NetworkingConfig: s.NetworkingConfig(),
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
	oldState, err := d.makeRaw(fd)
	if err != nil {
		return fmt.Errorf("set terminal raw mode: %w", err)
	}

	ps := newPollableStdin(d.stdin)

	// Deferred cleanup: restore raw mode. The pollable handle is already closed
	// by the time this runs (closed inline, before return, so the join goroutine
	// exits before att.Conn.Close fires).
	defer func() {
		if oldState != nil {
			_ = term.Restore(fd, oldState) //nolint:errcheck // teardown
		}
	}()

	// Register wait BEFORE start so the daemon guarantees delivery of the exit
	// status even when the container auto-removes within milliseconds of starting.
	// A derived cancellable context lets early-return paths (e.g. ContainerStart
	// failure) cancel the SDK wait goroutine so it does not block forever holding
	// its connection.
	waitCtx, waitCancel := context.WithCancel(ctx)
	defer waitCancel()
	wr := cli.ContainerWait(waitCtx, id, moby.ContainerWaitOptions{Condition: container.WaitConditionNextExit})

	// Ensure the pollable dup fd is always closed if drainAndJoin is not reached
	// (e.g. ContainerStart failure returns before the select). drainAndJoin itself
	// nils ps.closer after closing so this defer does not fire a second time.
	defer func() {
		if ps.closer != nil {
			_ = ps.closer.Close() //nolint:errcheck // fd cleanup on early-return paths
		}
	}()

	if _, err = cli.ContainerStart(ctx, id, moby.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("container start: %w", err)
	}
	startedCleanly = true

	// Seed the container's TTY with the host size; it otherwise defaults to
	// 80x24 and full-screen TUIs render wrong until the first SIGWINCH.
	if w, h, sizeErr := term.GetSize(fd); sizeErr == nil {
		_, _ = cli.ContainerResize(ctx, id, moby.ContainerResizeOptions{
			Height: uint(h),
			Width:  uint(w),
		})
	}

	// A pending resize must finish before Run returns.
	if runtime.GOOS != "windows" {
		winchCh := make(chan os.Signal, 1)
		signal.Notify(winchCh, syscall.SIGWINCH)
		resizeDone := make(chan struct{})
		go func() {
			defer close(resizeDone)
			for range winchCh {
				if w, h, sizeErr := term.GetSize(fd); sizeErr == nil {
					_, _ = cli.ContainerResize(ctx, id, moby.ContainerResizeOptions{
						Height: uint(h),
						Width:  uint(w),
					})
				}
			}
			if d.resizeGoroutineHook != nil {
				d.resizeGoroutineHook()
			}
		}()
		// Stop sends before closing; then wait for any in-flight resize.
		defer func() {
			signal.Stop(winchCh)
			close(winchCh)
			<-resizeDone
		}()
	}

	// TTY output is not multiplexed. Drain it before reporting exit status.
	outputDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(d.stdout, att.Reader) //nolint:errcheck
		close(outputDone)
	}()

	// CloseWrite lets programs waiting for stdin EOF exit. A closeable ps.handle
	// also lets drainAndJoin stop this copy after container output ends.
	stdinDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(att.Conn, ps.handle) //nolint:errcheck
		_ = att.CloseWrite()                //nolint:errcheck // propagate stdin EOF to container
		close(stdinDone)
	}()

	select {
	case err := <-wr.Error:
		// Remove before draining: a failed wait may leave a running container and
		// an open attach stream. The parent context may already be cancelled.
		_, _ = cli.ContainerRemove(context.Background(), id, moby.ContainerRemoveOptions{Force: true})
		drainAndJoin(ctx, outputDone, stdinDone, &ps)
		return fmt.Errorf("container wait: %w", err)
	case res := <-wr.Result:
		if res.Error != nil {
			drainAndJoin(ctx, outputDone, stdinDone, &ps)
			return fmt.Errorf("container wait error: %s", res.Error.Message)
		}
		// Drain tail output and finish stdin writes before the deferred conn close.
		drainAndJoin(ctx, outputDone, stdinDone, &ps)
		if res.StatusCode != 0 {
			return &ExitError{Code: int(res.StatusCode)}
		}
		return nil
	case <-ctx.Done():
		// Cancellation prevents a stalled attach stream from blocking cleanup.
		drainAndJoin(ctx, outputDone, stdinDone, &ps)
		return ctx.Err()
	}
}

// drainAndJoin closes stdin after output drains, so a pending read cannot keep
// Run alive. Cancellation bounds both waits if the daemon leaves a stream open.
func drainAndJoin(ctx context.Context, outputDone, stdinDone <-chan struct{}, ps *pollableStdin) {
	select {
	case <-outputDone:
	case <-ctx.Done():
	}
	if ps.closer != nil {
		_ = ps.closer.Close() //nolint:errcheck // unblock the pending stdin read
		ps.closer = nil       // prevent double-close in caller's deferred cleanup
		select {
		case <-stdinDone:
		case <-ctx.Done():
		}
	}
}
