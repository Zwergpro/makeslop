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
// when closed. If the handle is backed by a real O_NONBLOCK dup of fd 0, closing
// it causes the Go runtime poller to unblock any pending Read on that fd.
type pollableStdin struct {
	handle  io.Reader // read side; may be a *os.File (pollable dup) or the original reader
	closer  io.Closer // handle's close method (nil if the reader is not closeable)
	restore func()    // called in terminal cleanup to re-set fd 0 to blocking; may be nil
}

// newPollableStdin tries to create a pollable, closeable handle backed by a dup
// of fd 0 with O_NONBLOCK set. On success the Go runtime registers the fd with
// the network poller, so Close() unblocks a pending Read; ps.closer is non-nil.
//
// Falls back gracefully: if any syscall fails, returns an unconverted reference
// to stdinReader with ps.closer == nil. Callers check ps.closer != nil to
// determine joinability. Callers must tolerate the fallback goroutine-leak path
// (documented in runContainer).
func newPollableStdin(stdinReader io.Reader) pollableStdin {
	// Only attempt the dup if the injected reader is actually os.Stdin (fd 0).
	// Injected test readers that implement io.Closer are handled by the caller directly.
	if stdinReader != os.Stdin {
		if c, isCloser := stdinReader.(io.Closer); isCloser {
			return pollableStdin{handle: stdinReader, closer: c}
		}
		return pollableStdin{handle: stdinReader}
	}

	fd := 0

	// Set fd 0 to non-blocking so the Go runtime poller can manage it.
	if err := syscall.SetNonblock(fd, true); err != nil {
		return pollableStdin{handle: stdinReader}
	}

	// Dup the fd so we have an independent file description to close later.
	dupFd, err := syscall.Dup(fd)
	if err != nil {
		// Restore blocking before giving up.
		_ = syscall.SetNonblock(fd, false)
		return pollableStdin{handle: stdinReader}
	}

	// Wrap in os.NewFile so Go registers it with the runtime network poller.
	// This makes Close() on the *os.File unblock pending Read calls.
	handle := os.NewFile(uintptr(dupFd), "stdin-dup")

	restore := func() {
		// Restore fd 0 to blocking mode (mirroring what we did above).
		// The dup fd is already closed by the time this runs.
		_ = syscall.SetNonblock(fd, false)
	}

	return pollableStdin{handle: handle, closer: handle, restore: restore}
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
// Stdin-goroutine join: production path creates a pollable dup of fd 0 with
// O_NONBLOCK set. Closing the dup unblocks the pending Read via the Go runtime
// poller. The blocking flag is restored in the same deferred cleanup as raw mode.
// If dup/fcntl fails (newPollableStdin ok=false), the goroutine is leaked — this
// is the documented fallback, matching what the docker CLI does.
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

	// Build the pollable stdin handle here so its restore func can be bundled
	// with the terminal-restore defer below.
	ps := newPollableStdin(stdin)

	// Deferred cleanup: restore raw mode AND the blocking flag on fd 0.
	// The pollable dup is already closed by the time this runs (closed inline,
	// before return, so the join goroutine exits before att.Conn.Close fires).
	defer func() {
		if oldState != nil {
			_ = term.Restore(fd, oldState) //nolint:errcheck // teardown
		}
		if ps.restore != nil {
			ps.restore()
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
