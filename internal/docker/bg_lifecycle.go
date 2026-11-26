package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	moby "github.com/moby/moby/client"
)

const bgSession = "agent"

// These values pin zmx to the same private socket and name for every client.
var bgZMXEnv = []string{
	"ZMX_DIR=/tmp/makeslop-zmx", "ZMX_DIR_MODE=0700", "ZMX_SESSION=", "ZMX_SESSION_PREFIX=",
}

// ForBackground returns the detached projection of an existing project spec.
func (s Spec) ForBackground(uuid, project string) Spec {
	s.bgUUID, s.bgProject = uuid, project
	s.ZMXArgv = []string{"zmx", "attach", bgSession}
	env := make([]string, 0, len(s.Env)+len(bgZMXEnv))
	for _, entry := range s.Env {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "ZMX_DIR", "ZMX_DIR_MODE", "ZMX_SESSION", "ZMX_SESSION_PREFIX":
		default:
			env = append(env, entry)
		}
	}
	s.Env = append(env, bgZMXEnv...)
	return s
}

// RunBG starts a new background container and reports success only after zmx is ready.
func (d *Docker) RunBG(ctx context.Context, s Spec) (err error) {
	uuid := s.bgUUID
	if !bgUUIDPattern.MatchString(uuid) {
		return fmt.Errorf("expected a full background UUID")
	}
	created, err := d.client.ContainerCreate(ctx, moby.ContainerCreateOptions{
		Name: bgPrefix + uuid, Config: s.ContainerConfig(), HostConfig: s.HostConfig(), NetworkingConfig: s.NetworkingConfig(),
	})
	if err != nil {
		return bgDaemonError("create", uuid, err)
	}
	defer func() {
		if err == nil {
			return
		}
		cleanup, cancel := WithPreflightTimeout(context.WithoutCancel(ctx))
		defer cancel()
		if _, rmErr := d.client.ContainerRemove(cleanup, created.ID, moby.ContainerRemoveOptions{Force: true}); rmErr != nil && !errdefs.IsNotFound(rmErr) {
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
	if ready, err := d.bgSessionReady(probeCtx, c.ID); ready && err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
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
			return fmt.Errorf("background container %s stopped before its session was ready — check that zmx is on PATH and the image entrypoint runs the supplied command", c.UUID)
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
		Cmd: []string{"zmx", "list", "--short"}, Env: bgZMXEnv, AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		return false, err
	}
	attached, err := d.client.ExecAttach(ctx, p.ID, moby.ExecAttachOptions{})
	if err != nil {
		return false, err
	}
	defer attached.Close()
	cancelStream := context.AfterFunc(ctx, attached.Close)
	defer cancelStream()
	var stdout bytes.Buffer
	if _, err = stdcopy.StdCopy(&stdout, io.Discard, io.LimitReader(attached.Reader, 64<<10)); err != nil {
		return false, err
	}
	status, err := d.client.ExecInspect(ctx, p.ID, moby.ExecInspectOptions{})
	if err != nil || status.Running || status.ExitCode != 0 {
		return false, err
	}
	for line := range strings.Lines(stdout.String()) {
		if strings.TrimSpace(line) == bgSession {
			return true, nil
		}
	}
	return false, nil
}
