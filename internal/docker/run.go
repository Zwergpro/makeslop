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

// pollableStdin holds a closeable handle to stdin that unblocks pending reads
// when closed. If the handle is backed by a poller-managed fresh open of the
// terminal, closing it causes the Go runtime poller to unblock a pending Read.
type pollableStdin struct {
	handle io.Reader // read side; may be a *os.File (fresh tty open) or the original reader
	closer io.Closer // handle's close method (nil if the reader is not closeable)
}

// newPollableStdin tries to create a pollable, closeable handle for reading
// stdin. For the real os.Stdin it opens /dev/tty with O_NONBLOCK. The fresh
// open is the load-bearing detail: O_NONBLOCK lives in the open file
// description, and a terminal session's fd 0/1/2 are dups of a single
// description — so flipping the flag on fd 0 itself (a previous approach)
// silently made os.Stdout non-blocking too. Go treats os.Stdout as a blocking
// fd, so the first output burst that filled the pty buffer (any TUI redraw)
// made the stdout pump die on EAGAIN and froze the session.
//
// The fresh handle is used only when it refers to the same character device as
// fd 0 and the runtime poller accepted it (probed via SetReadDeadline — some
// platforms cannot poll /dev/tty, e.g. kqueue on macOS, and os then reverts
// the file to blocking mode, where Close would no longer unblock Read).
//
// Falls back gracefully: on any failure, returns an unconverted reference to
// stdinReader with ps.closer == nil. Callers check ps.closer != nil to
// determine joinability and must tolerate the fallback goroutine-leak path
// (documented in runContainer).
func newPollableStdin(stdinReader io.Reader) pollableStdin {
	// Only attempt the tty open if the injected reader is actually os.Stdin.
	// Injected test readers that implement io.Closer are handled directly.
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

// sameCharDevice reports whether a and b refer to the same device. b is
// inspected via SyscallConn so a pollable fd is not switched back to blocking
// mode (which (*os.File).Fd may do).
func sameCharDevice(a, b *os.File) bool {
	var sa, sb syscall.Stat_t
	if syscall.Fstat(int(a.Fd()), &sa) != nil {
		return false
	}
	rc, err := b.SyscallConn()
	if err != nil {
		return false
	}
	var ferr error
	if rc.Control(func(fd uintptr) { ferr = syscall.Fstat(int(fd), &sb) }) != nil || ferr != nil {
		return false
	}
	return sa.Rdev == sb.Rdev
}

// runContainer implements (*Docker).Run with injected apiClient, TTY predicate,
// raw-mode function, and I/O streams. The caller owns the client lifetime.
//
// Lifecycle order (fixes AutoRemove + wait-before-start race, output truncation,
// and stdin goroutine leak — findings #1, #2, #3):
//
//	Create → Attach → raw mode → ContainerWait(next-exit) → ContainerStart →
//	  stream copies → drain stdout → close stdin handle → join stdin copy →
//	  map StatusCode → ExitError
//
// Registering the wait before start guarantees the daemon delivers the exit
// status even when the container exits and is auto-removed within milliseconds.
//
// Stdin-goroutine join: production path opens /dev/tty as a fresh O_NONBLOCK
// file description (never altering fd 0's shared flags — see newPollableStdin).
// Closing it unblocks the pending Read via the Go runtime poller. If the open
// or poller registration fails, the goroutine is leaked — this is the
// documented fallback, matching what the docker CLI does.
// Injected test readers implementing io.Closer (e.g. os.Pipe read-end) are
// treated identically to the pollable dup path and the goroutine is always joined.
func runContainer(ctx context.Context, cli apiClient, isTTYFn func() bool, makeRawFn func(int) (*term.State, error), stdin io.Reader, stdout io.Writer, s Spec) error {
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

	ps := newPollableStdin(stdin)

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
	wr := cli.ContainerWait(ctx, id, moby.ContainerWaitOptions{Condition: container.WaitConditionNextExit})

	// Ensure the pollable dup fd is always closed on non-happy-path exits (wr.Error,
	// WaitExitError, ctx.Done). On the normal result path the close is done inline
	// before the join select; the closedInline flag prevents a double-close there.
	closedInline := false
	defer func() {
		if ps.closer != nil && !closedInline {
			_ = ps.closer.Close() //nolint:errcheck // fd cleanup on error paths
		}
	}()

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
	// stdout copy goroutine: closes outputDone when EOF'd so we can drain before
	// mapping the exit status (fixes output truncation race — finding #1).
	outputDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(stdout, att.Reader) //nolint:errcheck
		close(outputDone)
	}()

	// stdin copy goroutine: reads from the pollable handle (ps.handle) and writes
	// into the attach connection. If ps.closer != nil, closing ps.closer after
	// the output drain will unblock this Read and allow the goroutine to exit.
	// If ps.closer == nil (fallback: dup/fcntl failed and reader has no
	// io.Closer), the goroutine is leaked — this is the documented fallback path.
	stdinDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(att.Conn, ps.handle) //nolint:errcheck
		close(stdinDone)
	}()

	select {
	case err := <-wr.Error:
		// Drain and join before returning so goroutines don't outlive this call.
		select {
		case <-outputDone:
		case <-ctx.Done():
		}
		if ps.closer != nil {
			closedInline = true
			_ = ps.closer.Close() //nolint:errcheck
			select {
			case <-stdinDone:
			case <-ctx.Done():
			}
		}
		return fmt.Errorf("container wait: %w", err)
	case res := <-wr.Result:
		if res.Error != nil {
			// Drain and join before returning so goroutines don't outlive this call.
			// The closedInline flag prevents the deferred closer from firing twice.
			select {
			case <-outputDone:
			case <-ctx.Done():
			}
			if ps.closer != nil {
				closedInline = true
				_ = ps.closer.Close() //nolint:errcheck
				select {
				case <-stdinDone:
				case <-ctx.Done():
				}
			}
			return fmt.Errorf("container wait error: %s", res.Error.Message)
		}
		// Drain stdout before reporting the exit status so tail output is not lost.
		// Safety net: ctx.Done() unblocks if the attach stream never closes
		// (e.g. daemon misbehaviour). In normal operation, a TTY attach EOFs on
		// container exit.
		select {
		case <-outputDone:
		case <-ctx.Done():
		}

		// Join the stdin copy goroutine. Ordering: this join is INLINE (not deferred)
		// so it completes before the deferred att.Conn.Close fires — the goroutine
		// writes into att.Conn, so it must finish before the conn is closed.
		if ps.closer != nil {
			closedInline = true
			_ = ps.closer.Close() //nolint:errcheck // unblock the pending stdin read
			select {
			case <-stdinDone:
			case <-ctx.Done():
			}
		}

		if res.StatusCode != 0 {
			return &ExitError{Code: int(res.StatusCode)}
		}
		return nil
	case <-ctx.Done():
		// Drain and join before returning so goroutines don't outlive this call.
		// Context is already cancelled; ctx.Done() in the select fallbacks fires
		// immediately, giving each goroutine one cycle to exit before we proceed.
		select {
		case <-outputDone:
		case <-ctx.Done():
		}
		if ps.closer != nil {
			closedInline = true
			_ = ps.closer.Close() //nolint:errcheck
			select {
			case <-stdinDone:
			case <-ctx.Done():
			}
		}
		return ctx.Err()
	}
}
