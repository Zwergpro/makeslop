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

// sameCharDevice reports whether a and b refer to the same device. Both are
// inspected via SyscallConn so neither pollable fd is switched back to blocking
// mode (which (*os.File).Fd may do).
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
//
// resizeHook, when non-nil, is called at the end of the resize goroutine body
// (before closing resizeDone). Tests inject a hook to verify the goroutine was
// joined before Run returned; nil in production.
func runContainer(ctx context.Context, cli apiClient, isTTYFn func() bool, makeRawFn func(int) (*term.State, error), stdin io.Reader, stdout io.Writer, resizeHook func(), s Spec) error {
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
	// A derived cancellable context lets early-return paths (e.g. ContainerStart
	// failure) cancel the SDK wait goroutine so it does not block forever holding
	// its connection (finding #4).
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
			if resizeHook != nil {
				resizeHook()
			}
		}()
		// signal.Stop guarantees no further sends to winchCh, so close is race-free.
		// <-resizeDone then guarantees no ContainerResize is in flight when we return.
		defer func() {
			signal.Stop(winchCh)
			close(winchCh)
			<-resizeDone
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
	//
	// After the copy ends (stdin EOF or error), signal stdin EOF to the container
	// by calling att.CloseWrite(). HijackedResponse.CloseWrite is a no-op when
	// the underlying connection does not implement CloseWriter; on real TCP
	// connections it sends a FIN to the container's stdin fd, allowing containers
	// that read stdin to EOF (e.g. "cat", "wc -l") to detect end-of-input and
	// exit cleanly instead of hanging.
	stdinDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(att.Conn, ps.handle) //nolint:errcheck
		_ = att.CloseWrite()                //nolint:errcheck // propagate stdin EOF to container
		close(stdinDone)
	}()

	select {
	case err := <-wr.Error:
		// Force-remove the container BEFORE draining outputDone. The remove kills
		// the container → the attach stream EOFs → io.Copy in the output goroutine
		// returns → outputDone is closed. Without this the drain blocks until the
		// container exits on its own, and the deferred force-remove at the top of
		// runContainer never fires (startedCleanly is true and ctx.Err() is nil),
		// leaving a running container behind (finding #5).
		// Use context.Background() because the parent ctx may already be cancelled.
		_, _ = cli.ContainerRemove(context.Background(), id, moby.ContainerRemoveOptions{Force: true})
		drainAndJoin(ctx, outputDone, stdinDone, &ps)
		return fmt.Errorf("container wait: %w", err)
	case res := <-wr.Result:
		if res.Error != nil {
			drainAndJoin(ctx, outputDone, stdinDone, &ps)
			return fmt.Errorf("container wait error: %s", res.Error.Message)
		}
		// Drain stdout before reporting the exit status so tail output is not lost.
		// Safety net: ctx.Done() unblocks if the attach stream never closes
		// (e.g. daemon misbehaviour). In normal operation, a TTY attach EOFs on
		// container exit.
		//
		// Join ordering: drainAndJoin closes ps.closer INLINE so the stdin goroutine
		// exits before the deferred att.Conn.Close fires — the goroutine writes into
		// att.Conn, so it must finish before the conn is closed.
		drainAndJoin(ctx, outputDone, stdinDone, &ps)
		if res.StatusCode != 0 {
			return &ExitError{Code: int(res.StatusCode)}
		}
		return nil
	case <-ctx.Done():
		// Context is already cancelled; ctx.Done() in drainAndJoin's select fallbacks
		// fires immediately, giving each goroutine one cycle to exit before we proceed.
		drainAndJoin(ctx, outputDone, stdinDone, &ps)
		return ctx.Err()
	}
}

// drainAndJoin waits for the output goroutine to finish (outputDone) and then
// joins the stdin goroutine (stdinDone). It closes ps.closer to unblock the
// stdin goroutine's pending read when the closer is non-nil. ctx.Done() is used
// as a fallback escape hatch so neither wait blocks indefinitely on a
// misbehaving daemon.
//
// After drainAndJoin returns, ps.closer is nil — the caller's deferred cleanup
// checks ps.closer != nil, so it does not fire a second time.
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
