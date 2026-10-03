package cli

import (
	"context"
	"fmt"
	"strings"

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

// allDocker is everything dockerDeps needs from a single implementation.
type allDocker interface {
	containerRunner
	daemonChecker
	imageChecker
	networkChecker
}

type dockerDeps struct {
	runner  containerRunner
	daemon  daemonChecker
	image   imageChecker
	network networkChecker
}

// newDockerDeps fills every dockerDeps field from one implementation, so a
// newly added field cannot be forgotten at a construction site (a nil field
// would compile and only panic in production).
func newDockerDeps(x allDocker) dockerDeps {
	return dockerDeps{runner: x, daemon: x, image: x, network: x}
}

func (d dockerDeps) checkDaemonPreflight(ctx context.Context) error {
	pfCtx, pfCancel := docker.WithPreflightTimeout(ctx)
	defer pfCancel()
	return d.daemon.CheckDaemon(pfCtx)
}

func (d dockerDeps) imageExistsPreflight(ctx context.Context, image string) (bool, error) {
	pfCtx, pfCancel := docker.WithPreflightTimeout(ctx)
	defer pfCancel()
	return d.image.ImageExists(pfCtx, image)
}

// builtinNetworkModes are daemon built-ins with nothing to inspect; "default"
// is a bridge alias with no network object.
var builtinNetworkModes = map[string]bool{
	"bridge":  true,
	"host":    true,
	"none":    true,
	"default": true,
}

const containerModePrefix = "container:"

// networkNeedsInspect reports whether n references a container or network the
// daemon must be asked about (false for unset and built-in modes).
func networkNeedsInspect(n projectconfig.Network) bool {
	if len(n.Networks) > 0 {
		return true
	}
	return n.Mode != "" && !builtinNetworkModes[n.Mode]
}

// networkPreflight checks that the container: target is running and every
// named network exists, bounded by preflightTimeout. The returned error is
// the user-facing hint (callers prefix "makeslop: "). No-op for the zero
// Network and built-in modes.
func (d dockerDeps) networkPreflight(ctx context.Context, n projectconfig.Network) error {
	if !networkNeedsInspect(n) {
		return nil
	}
	pfCtx, pfCancel := docker.WithPreflightTimeout(ctx)
	defer pfCancel()

	if target, ok := strings.CutPrefix(n.Mode, containerModePrefix); ok {
		exists, running, err := d.network.ContainerRunning(pfCtx, target)
		if err != nil {
			return fmt.Errorf("network_mode: check container %q: %v — is docker running?", target, err)
		}
		if !exists {
			return fmt.Errorf("network_mode: container %q not found — start it first; "+
				"compose names containers <project>-<service>-1 unless container_name is set (check 'docker ps')", target)
		}
		if !running {
			return fmt.Errorf("network_mode: container %q is not running — start it first", target)
		}
		return nil
	}

	names := n.Networks
	if n.Mode != "" {
		names = []string{n.Mode}
	}
	for _, name := range names {
		found, err := d.network.NetworkExists(pfCtx, name)
		if err != nil {
			return fmt.Errorf("check network %q: %v — is docker running?", name, err)
		}
		if !found {
			return fmt.Errorf("network %[1]q not found — create it with 'docker network create %[1]s'; "+
				"compose prefixes networks with <project>_ (check 'docker network ls')", name)
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
