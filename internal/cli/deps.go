package cli

import (
	"context"

	"github.com/Zwergpro/makeslop/internal/docker"
)

// Consumer-side docker interfaces. *docker.Docker satisfies all three; tests
// inject fakes via newRootCmdWithDeps.
type containerRunner interface {
	Run(ctx context.Context, s docker.Spec) error
}

type daemonChecker interface {
	CheckDaemon(ctx context.Context) error
}

type imageChecker interface {
	ImageExists(ctx context.Context, image string) (bool, error)
}

type dockerDeps struct {
	runner containerRunner
	daemon daemonChecker
	image  imageChecker
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

// dockerNewErrStub surfaces the docker.New() error from every method so
// non-docker commands still work while docker commands fail clearly.
type dockerNewErrStub struct{ err error }

func (s dockerNewErrStub) Run(_ context.Context, _ docker.Spec) error { return s.err }
func (s dockerNewErrStub) CheckDaemon(_ context.Context) error        { return s.err }
func (s dockerNewErrStub) ImageExists(_ context.Context, _ string) (bool, error) {
	return false, s.err
}
