package docker

import (
	"context"
	"errors"
	"os"
	"os/exec"

	"golang.org/x/term"
)

// ErrNoTTY is returned by Run when stdin or stdout is not a character device.
// The cobra layer is the only authorized caller; it translates this sentinel
// into a user-facing message and exits silently.
var ErrNoTTY = errors.New("interactive TTY required on stdin and stdout")

// Package-level swap points for tests (see testing.go), preferred over PATH
// manipulation to keep swaps scoped per-test.
var (
	dockerBinary = "docker"
	ttyCheck     = func() bool { return isTTY(os.Stdin) && isTTY(os.Stdout) }
)

// isTTY reports whether f is a terminal. Uses golang.org/x/term.IsTerminal
// (an ioctl-based check) rather than os.ModeCharDevice, which would also
// accept non-terminal char devices like /dev/null and /dev/zero.
func isTTY(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

// Run executes `docker run` with the argv derived from s. It refuses to start
// (returning ErrNoTTY) unless both stdin and stdout are TTYs. On non-zero
// container exit the returned error wraps *exec.ExitError so callers can
// propagate the code via errors.As.
func Run(ctx context.Context, s Spec) error {
	if !ttyCheck() {
		return ErrNoTTY
	}
	cmd := exec.CommandContext(ctx, dockerBinary, s.Args()...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
