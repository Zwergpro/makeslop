package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"golang.org/x/term"
)

var ErrNoTTY = errors.New("interactive TTY required on stdin and stdout")

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

// Build executes `docker build` with the options in o. When o.ContextDir is
// empty, Build creates an empty temp directory to use as the build context and
// removes it on return. DOCKER_BUILDKIT=1 is always set in the child
// environment so cache mounts (--mount=type=cache) work. Build never checks
// for a TTY and can be used safely in CI/pipes.
func Build(ctx context.Context, o BuildOptions, stdout, stderr io.Writer) error {
	if o.ContextDir == "" {
		dir, err := os.MkdirTemp("", "makeslop-build-*")
		if err != nil {
			return fmt.Errorf("create build context dir: %w", err)
		}
		defer os.RemoveAll(dir) //nolint:errcheck // best-effort cleanup; failure is non-actionable
		o.ContextDir = dir
	}
	cmd := exec.CommandContext(ctx, dockerBinary, BuildArgv(o)...)
	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Stdin = nil
	return cmd.Run()
}
