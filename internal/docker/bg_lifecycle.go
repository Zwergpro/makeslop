package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	moby "github.com/moby/moby/client"
)

const bgSession = "agent"

// These values pin zmx to the same private socket and name for every client.
var bgZMXEnv = []string{
	"ZMX_DIR=/tmp/makeslop-zmx", "ZMX_DIR_MODE=0700", "ZMX_SESSION=", "ZMX_SESSION_PREFIX=",
}

// RunBG starts a new background container and reports success only after zmx is ready.
func (d *Docker) RunBG(ctx context.Context, s BackgroundSpec) (err error) {
	uuid := s.uuid
	if !validBGUUID(uuid) {
		return fmt.Errorf("expected a full background UUID")
	}

	created, err := d.client.ContainerCreate(ctx, s.create.createOptions())
	if err != nil {
		return bgDaemonError("create", uuid, err)
	}
	defer func() {
		if err == nil {
			return
		}

		cleanup, cancel := WithPreflightTimeout(context.WithoutCancel(ctx))
		defer cancel()

		_, rmErr := d.client.ContainerRemove(cleanup, created.ID, moby.ContainerRemoveOptions{Force: true})
		if rmErr != nil && !errdefs.IsNotFound(rmErr) {
			err = errors.Join(err, bgDaemonError("clean up", uuid, rmErr))
		}
	}()

	return d.startBG(ctx, BGContainer{UUID: uuid, ID: created.ID})
}

// StartBG starts a stopped container or confirms an existing session is ready.
func (d *Docker) StartBG(ctx context.Context, uuid string) error {
	c, err := d.lookupBG(ctx, uuid)
	if err != nil {
		return err
	}

	if c.stopped {
		return d.startBG(ctx, c)
	}

	if !c.running {
		return bgStateError(c)
	}

	probeCtx, cancel := WithPreflightTimeout(ctx)
	defer cancel()

	ready, err := d.bgSessionReady(probeCtx, c.ID)
	if err != nil {
		return bgDaemonError("check session in", uuid, err)
	}

	if ready {
		return nil
	}

	return fmt.Errorf("session unavailable; use 'makeslop bg restart %s'", uuid)
}

// RestartBG replaces any live session without replacing the container.
func (d *Docker) RestartBG(ctx context.Context, uuid string) error {
	c, err := d.lookupBG(ctx, uuid)
	if err != nil {
		return err
	}

	if c.running {
		if err := d.stopBG(ctx, c); err != nil {
			return err
		}
	} else if !c.stopped {
		return bgStateError(c)
	}

	return d.startBG(ctx, c)
}

// StopBG stops a background container but keeps it and its writable layer.
func (d *Docker) StopBG(ctx context.Context, uuid string) error {
	c, err := d.lookupBG(ctx, uuid)
	if err != nil {
		return err
	}

	if c.stopped {
		return nil
	}

	if !c.running {
		return bgStateError(c)
	}

	return d.stopBG(ctx, c)
}

func (d *Docker) stopBG(ctx context.Context, c BGContainer) error {
	if _, err := d.client.ContainerStop(ctx, c.ID, moby.ContainerStopOptions{}); err != nil {
		return bgDaemonError("stop", c.UUID, err)
	}

	return nil
}

// RemoveBG removes only a stopped background container.
func (d *Docker) RemoveBG(ctx context.Context, uuid string) error {
	c, err := d.lookupBG(ctx, uuid)
	if err != nil {
		return err
	}

	if c.running {
		return fmt.Errorf("container running; use 'makeslop bg stop %s' before removing it", uuid)
	}

	if !c.stopped {
		return bgStateError(c)
	}

	if _, err := d.client.ContainerRemove(ctx, c.ID, moby.ContainerRemoveOptions{}); err != nil {
		return bgDaemonError("remove", uuid, err)
	}

	return nil
}

func bgStateError(c BGContainer) error {
	return fmt.Errorf("background container %s is not ready for this operation (state %q)", c.UUID, c.State)
}

func (d *Docker) startBG(ctx context.Context, c BGContainer) error {
	startCtx, cancelStart := WithPreflightTimeout(ctx)
	_, err := d.client.ContainerStart(startCtx, c.ID, moby.ContainerStartOptions{})
	cancelStart()

	if err != nil {
		startErr := bgDaemonError("start", c.UUID, err)

		if strings.Contains(err.Error(), "zmx") {
			return fmt.Errorf("%w — background images must have zmx on PATH", startErr)
		}

		return startErr
	}

	readyCtx, cancel := WithPreflightTimeout(ctx)
	defer cancel()

	for {
		res, err := d.client.ContainerInspect(readyCtx, c.ID, moby.ContainerInspectOptions{})
		if err != nil {
			return bgDaemonError("check startup of", c.UUID, err)
		}

		st := res.Container.State
		if st == nil || !st.Running || st.Paused || st.Restarting {
			return fmt.Errorf(
				"background container %s stopped before its session was ready — "+
					"check that zmx is on PATH and the image entrypoint runs the supplied command",
				c.UUID,
			)
		}

		ready, err := d.bgSessionReady(readyCtx, c.ID)
		if err == nil && ready {
			return readyCtx.Err()
		}

		// A startup race can close a probe just as zmx comes up or Docker stops.
		select {
		case <-readyCtx.Done():
			return fmt.Errorf("background container %s: waiting for zmx session: %w", c.UUID, readyCtx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (d *Docker) bgSessionReady(ctx context.Context, id string) (bool, error) {
	p, err := d.client.ExecCreate(ctx, id, moby.ExecCreateOptions{
		Cmd: []string{"zmx", "get", bgSession},
		Env: bgZMXEnv,
	})
	if err != nil {
		return false, err
	}

	// get queries an existing session without creating it. Only its status is
	// needed, so use cancellable HTTP calls rather than upgrading a stream.
	if _, err := d.client.ExecStart(ctx, p.ID, moby.ExecStartOptions{Detach: true}); err != nil {
		return false, err
	}

	for {
		status, err := d.client.ExecInspect(ctx, p.ID, moby.ExecInspectOptions{})
		if err != nil {
			return false, err
		}

		if !status.Running {
			switch status.ExitCode {
			case 0:
				return true, nil
			case 1:
				return false, nil
			default:
				return false, fmt.Errorf("zmx readiness probe exited with status %d", status.ExitCode)
			}
		}

		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}
