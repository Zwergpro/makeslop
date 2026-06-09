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

// checkDaemon is the internal implementation of CheckDaemon using a provided
// apiClient. Used by (*Docker).CheckDaemon; the caller owns the client lifetime.
func checkDaemon(ctx context.Context, c apiClient) error {
	_, err := c.Ping(ctx, moby.PingOptions{})
	if err != nil {
		// Attempt to extract DOCKER_HOST from environment for richer messages.
		// The real moby.Client exposes DaemonHost() but our narrow interface
		// does not. Use the error text only.
		return &ErrDaemonUnreachable{Cause: err}
	}
	return nil
}

// imageExists is the internal implementation of ImageExists using a provided
// apiClient. Used by (*Docker).ImageExists; the caller owns the client lifetime.
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

// CheckDaemon pings the Docker daemon. It returns *ErrDaemonUnreachable if the
// daemon cannot be reached, and nil on success.
//
// Deprecated: use (*Docker).CheckDaemon instead. This package-level shim is
// kept for backward compatibility during the struct-DI migration and will be
// removed in Task 5.
func CheckDaemon(ctx context.Context) error {
	c, err := newClientFn()
	if err != nil {
		return &ErrDaemonUnreachable{Cause: err}
	}
	defer c.Close() //nolint:errcheck // shim owns its client
	return checkDaemon(ctx, c)
}

// ImageExists reports whether the named image tag exists locally.
//
//   - (true, nil)  — image found
//   - (false, nil) — image absent (cerrdefs.IsNotFound classified)
//   - (false, err) — any other error (daemon error, permission, …); the caller
//     must NOT treat this as "image absent" — a dead daemon must surface as
//     a daemon error, not a misleading "run 'makeslop build'" hint.
//
// Deprecated: use (*Docker).ImageExists instead. This package-level shim is
// kept for backward compatibility during the struct-DI migration and will be
// removed in Task 5.
func ImageExists(ctx context.Context, image string) (bool, error) {
	c, err := newClientFn()
	if err != nil {
		return false, err
	}
	defer c.Close() //nolint:errcheck // shim owns its client
	return imageExists(ctx, c, image)
}
