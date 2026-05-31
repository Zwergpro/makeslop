package docker

import (
	"context"
	"errors"
	"fmt"

	cerrdefs "github.com/containerd/errdefs"
	moby "github.com/moby/moby/client"
)

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

// ErrImageNotBuilt is returned by ImageExists (indirectly via callers) when the
// requested image is absent locally. The Tag field carries the image name for
// use in remedy messages.
type ErrImageNotBuilt struct {
	Tag string
}

func (e *ErrImageNotBuilt) Error() string {
	return fmt.Sprintf("image %q not built locally", e.Tag)
}

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
		endpoint := ""
		// Attempt to extract DOCKER_HOST from environment for richer messages.
		// The real moby.Client exposes DaemonHost() but our narrow interface
		// does not. Use the error text only.
		return &ErrDaemonUnreachable{Endpoint: endpoint, Cause: err}
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
	if cerrdefs.IsNotFound(err) || isNotFoundWrapped(err) {
		return false, nil
	}
	return false, err
}

// isNotFoundWrapped handles the case where the fake wraps errdefs.ErrNotFound
// (using fmt.Errorf with %w) — cerrdefs.IsNotFound uses errors.Is internally,
// which unwraps the chain, so this helper is a belt-and-suspenders path to
// catch any additional wrapping patterns.
func isNotFoundWrapped(err error) bool {
	return errors.Is(err, cerrdefs.ErrNotFound)
}
