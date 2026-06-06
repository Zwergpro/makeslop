package docker

import (
	"context"
	"fmt"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	moby "github.com/moby/moby/client"
)

// preflightTimeout is the deadline applied to daemon-ping and image-inspect
// calls on the preflight paths (runRun, status). It does NOT apply to the
// long-lived docker.Run or docker.Build calls.
const preflightTimeout = 10 * time.Second

// WithPreflightTimeout wraps parent with a preflightTimeout deadline and
// returns the derived context and its cancel function. Callers must defer
// the returned cancel.
func WithPreflightTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, preflightTimeout)
}

// ErrDaemonUnreachable is returned by CheckDaemon when the Docker daemon cannot
// be reached. The wrapped error carries the underlying cause.
type ErrDaemonUnreachable struct {
	Endpoint string
	Cause    error
}

func (e *ErrDaemonUnreachable) Error() string {
	if e.Endpoint != "" {
		return fmt.Sprintf("docker daemon unreachable at %s: %v", e.Endpoint, e.Cause)
	}
	return fmt.Sprintf("docker daemon unreachable: %v", e.Cause)
}

func (e *ErrDaemonUnreachable) Unwrap() error { return e.Cause }

// CheckDaemon pings the Docker daemon. It returns *ErrDaemonUnreachable if the
// daemon cannot be reached, and nil on success.
func CheckDaemon(ctx context.Context) error {
	c, err := newClientFn()
	if err != nil {
		return &ErrDaemonUnreachable{Cause: err}
	}
	defer c.Close() //nolint:errcheck

	_, err = c.Ping(ctx, moby.PingOptions{})
	if err != nil {
		// Attempt to extract DOCKER_HOST from environment for richer messages.
		// The real moby.Client exposes DaemonHost() but our narrow interface
		// does not. Use the error text only.
		return &ErrDaemonUnreachable{Cause: err}
	}
	return nil
}

// ImageExists reports whether the named image tag exists locally.
//
//   - (true, nil)  — image found
//   - (false, nil) — image absent (cerrdefs.IsNotFound classified)
//   - (false, err) — any other error (daemon error, permission, …); the caller
//     must NOT treat this as "image absent" — a dead daemon must surface as
//     a daemon error, not a misleading "run 'makeslop build'" hint.
func ImageExists(ctx context.Context, image string) (bool, error) {
	c, err := newClientFn()
	if err != nil {
		return false, err
	}
	defer c.Close() //nolint:errcheck

	_, err = c.ImageInspect(ctx, image)
	if err == nil {
		return true, nil
	}
	if cerrdefs.IsNotFound(err) {
		return false, nil
	}
	return false, err
}
