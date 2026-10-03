package cli

import (
	"context"
	"fmt"

	"github.com/Zwergpro/makeslop/internal/docker"
	"github.com/Zwergpro/makeslop/internal/projectconfig"
)

type containerRunner interface {
	Run(ctx context.Context, s docker.Spec) error
}

type daemonChecker interface {
	CheckDaemon(ctx context.Context) error
}

type imageChecker interface {
	ImageExists(ctx context.Context, image string) (bool, error)
}

type networkChecker interface {
	ContainerRunning(ctx context.Context, name string) (exists, running bool, err error)
	NetworkExists(ctx context.Context, name string) (bool, error)
}

// dockerAPI is everything the commands need from docker.
type dockerAPI interface {
	containerRunner
	daemonChecker
	imageChecker
	networkChecker
}

// dockerDeps holds a single dockerAPI implementation, so no capability can be
// left nil at a construction site.
type dockerDeps struct {
	api dockerAPI
}

func (d dockerDeps) checkDaemonPreflight(ctx context.Context) error {
	pfCtx, pfCancel := docker.WithPreflightTimeout(ctx)
	defer pfCancel()
	return d.api.CheckDaemon(pfCtx)
}

func (d dockerDeps) imageExistsPreflight(ctx context.Context, image string) (bool, error) {
	pfCtx, pfCancel := docker.WithPreflightTimeout(ctx)
	defer pfCancel()
	return d.api.ImageExists(pfCtx, image)
}

// networkPreflight checks that the container: target is running and every
// named network exists, bounded by preflightTimeout. The returned error is
// the user-facing hint (callers prefix "makeslop: "). No-op for the zero
// Network and built-in modes.
func (d dockerDeps) networkPreflight(ctx context.Context, n projectconfig.Network) error {
	if !n.NeedsInspect() {
		return nil
	}
	pfCtx, pfCancel := docker.WithPreflightTimeout(ctx)
	defer pfCancel()

	if target, ok := n.ContainerTarget(); ok {
		exists, running, err := d.api.ContainerRunning(pfCtx, target)
		if err != nil {
			return fmt.Errorf("network_mode: check container %q: %w", target, err)
		}
		if !exists {
			return fmt.Errorf("network_mode: container %q not found — start it first; "+
				"compose names containers <project>-<service>-1 unless container_name is set (check 'docker ps')", target)
		}
		if !running {
			return fmt.Errorf("network_mode: container %q is not running (stopped, paused or restarting) — "+
				"start or unpause it (check 'docker ps -a')", target)
		}
		return nil
	}

	key, names := "networks", n.Networks
	if n.Mode != "" {
		key, names = "network_mode", []string{n.Mode}
	}
	for _, name := range names {
		found, err := d.api.NetworkExists(pfCtx, name)
		if err != nil {
			return fmt.Errorf("%s: check network %q: %w", key, name, err)
		}
		if !found {
			return fmt.Errorf("%[1]s: network %[2]q not found — create it with 'docker network create %[2]s'; "+
				"compose prefixes networks with <project>_ (check 'docker network ls')", key, name)
		}
	}
	return nil
}

// dockerNewErrStub surfaces the docker.New() error from every method so
// non-docker commands still work while docker commands fail clearly.
type dockerNewErrStub struct{ err error }

func (s dockerNewErrStub) Run(_ context.Context, _ docker.Spec) error { return s.err }
func (s dockerNewErrStub) CheckDaemon(_ context.Context) error        { return s.err }
func (s dockerNewErrStub) ImageExists(_ context.Context, _ string) (bool, error) {
	return false, s.err
}
func (s dockerNewErrStub) ContainerRunning(_ context.Context, _ string) (exists, running bool, err error) {
	return false, false, s.err
}
func (s dockerNewErrStub) NetworkExists(_ context.Context, _ string) (bool, error) {
	return false, s.err
}
