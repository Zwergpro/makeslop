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

// checkDaemon implements CheckDaemon with an injected apiClient.
func checkDaemon(ctx context.Context, c apiClient) error {
	_, err := c.Ping(ctx, moby.PingOptions{})
	if err != nil {
		// The narrow interface lacks DaemonHost(), so Endpoint is left empty.
		return &ErrDaemonUnreachable{Cause: err}
	}
	return nil
}

// imageExists implements ImageExists with an injected apiClient.
func imageExists(ctx context.Context, c apiClient, image string) (bool, error) {
	_, err := c.ImageInspect(ctx, image)
	if err == nil {
		return true, nil
	}
	if cerrdefs.IsNotFound(err) {
		return false, nil
	}
	return false, err
}
