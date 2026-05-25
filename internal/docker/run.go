package docker

import (
	"context"
	"errors"
	"os"
	"os/exec"

	"golang.org/x/term"
)

// ErrNoTTY is returned by Run when stdin or stdout is not a terminal.
var ErrNoTTY = errors.New("interactive TTY required on stdin and stdout")

// Test-only swap points for the docker binary and TTY check.
var (
	dockerBinary = "docker"
	ttyCheck     = func() bool { return isTTY(os.Stdin) && isTTY(os.Stdout) }
)

// Uses term.IsTerminal (ioctl-based) rather than os.ModeCharDevice, which
// also matches /dev/null and /dev/zero.
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
