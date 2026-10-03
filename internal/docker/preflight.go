package docker

import (
	"context"
	"fmt"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	moby "github.com/moby/moby/client"
)

// Bound daemon probes so an unreachable DOCKER_HOST cannot hang the CLI.
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

// ContainerRunning reports whether the named container exists and is running.
// A paused container counts as not running (it would stall traffic through a
// shared network namespace), and so does a restarting (crash-looping) one,
// whose namespace the daemon refuses to join although it reports Running. (false, false, nil) only for a classified
// not-found; other errors return (false, false, err).
func (d *Docker) ContainerRunning(ctx context.Context, name string) (exists, running bool, err error) {
	res, err := d.client.ContainerInspect(ctx, name, moby.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return false, false, nil
		}
		return false, false, err
	}
	st := res.Container.State
	return true, st != nil && st.Running && !st.Paused && !st.Restarting, nil
}

// NetworkExists reports whether the named network exists. (false, nil) only
// for a classified not-found; other errors return (false, err).
func (d *Docker) NetworkExists(ctx context.Context, name string) (bool, error) {
	_, err := d.client.NetworkInspect(ctx, name, moby.NetworkInspectOptions{})
	if err == nil {
		return true, nil
	}
	if cerrdefs.IsNotFound(err) {
		return false, nil
	}
	return false, err
}
