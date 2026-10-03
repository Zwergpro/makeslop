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

// WithPreflightTimeout prevents an unreachable daemon from hanging a CLI check.
func WithPreflightTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, preflightTimeout)
}

type ErrDaemonUnreachable struct {
	Cause error
}

func (e *ErrDaemonUnreachable) Error() string {
	return fmt.Sprintf("docker daemon unreachable: %v", e.Cause)
}

func (e *ErrDaemonUnreachable) Unwrap() error { return e.Cause }

func (d *Docker) CheckDaemon(ctx context.Context) error {
	_, err := d.client.Ping(ctx, moby.PingOptions{})
	if err != nil {
		return &ErrDaemonUnreachable{Cause: err}
	}
	return nil
}

// ImageExists distinguishes a missing image from a failed daemon request.
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

// ContainerRunning rejects paused containers, which would stall shared-network
// traffic, and restarting containers, whose namespace Docker refuses to join.
// Only a classified not-found returns no error.
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

// NetworkExists preserves daemon errors instead of misreporting them as absence.
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
