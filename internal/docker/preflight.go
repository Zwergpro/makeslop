package docker

import (
	"context"
	"fmt"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	moby "github.com/moby/moby/client"
)

// preflightTimeout bounds preflight ping/inspect calls so they never hang on a
// black-hole DOCKER_HOST. Not applied to long-lived Run/Build.
const preflightTimeout = 10 * time.Second

// WithPreflightTimeout wraps parent with a preflightTimeout deadline; callers
// must defer the returned cancel.
func WithPreflightTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, preflightTimeout)
}

// ErrDaemonUnreachable is returned by CheckDaemon when the daemon cannot be reached.
type ErrDaemonUnreachable struct {
	Cause error
}

func (e *ErrDaemonUnreachable) Error() string {
	return fmt.Sprintf("docker daemon unreachable: %v", e.Cause)
}

func (e *ErrDaemonUnreachable) Unwrap() error { return e.Cause }

// CheckDaemon pings the daemon, returning *ErrDaemonUnreachable on failure.
func (d *Docker) CheckDaemon(ctx context.Context) error {
	_, err := d.client.Ping(ctx, moby.PingOptions{})
	if err != nil {
		return &ErrDaemonUnreachable{Cause: err}
	}
	return nil
}

// ImageExists reports whether the named image tag exists locally. (false, nil)
// only for a classified not-found; other errors return (false, err).
func (d *Docker) ImageExists(ctx context.Context, image string) (bool, error) {
	_, err := d.client.ImageInspect(ctx, image)
	if err == nil {
		return true, nil
	}
	if cerrdefs.IsNotFound(err) {
		return false, nil
	}
	return false, err
}
