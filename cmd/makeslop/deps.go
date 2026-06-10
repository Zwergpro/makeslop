package main

import (
	"context"
	"io"

	"github.com/Zwergpro/makeslop/internal/docker"
)

// The four consumer-side docker interfaces. *docker.Docker satisfies all four in
// production; tests inject a fake via newRootCmdWithDeps.
type containerRunner interface {
	Run(ctx context.Context, s docker.Spec) error
}

type imageBuilder interface {
	Build(ctx context.Context, o docker.BuildOptions, out, errw io.Writer) error
}

type daemonChecker interface {
	CheckDaemon(ctx context.Context) error
}

type imageChecker interface {
	ImageExists(ctx context.Context, image string) (bool, error)
}

type dockerDeps struct {
	runner  containerRunner
	builder imageBuilder
	daemon  daemonChecker
	image   imageChecker
}

// checkDaemonPreflight calls CheckDaemon with a preflight timeout. The caller
// must not reuse ctx after this returns (it is cancelled internally).
func (d dockerDeps) checkDaemonPreflight(ctx context.Context) error {
	pfCtx, pfCancel := docker.WithPreflightTimeout(ctx)
	defer pfCancel()
	return d.daemon.CheckDaemon(pfCtx)
}

// imageExistsPreflight calls ImageExists with a preflight timeout. The caller
// must not reuse ctx after this returns (it is cancelled internally).
func (d dockerDeps) imageExistsPreflight(ctx context.Context, image string) (bool, error) {
	pfCtx, pfCancel := docker.WithPreflightTimeout(ctx)
	defer pfCancel()
	return d.image.ImageExists(pfCtx, image)
}

// dockerNewErrStub returns the docker.New() construction error from every
// operation, so docker-touching commands fail clearly instead of panicking.
type dockerNewErrStub struct{ err error }

func (s dockerNewErrStub) Run(_ context.Context, _ docker.Spec) error { return s.err }
func (s dockerNewErrStub) Build(_ context.Context, _ docker.BuildOptions, _, _ io.Writer) error {
	return s.err
}
func (s dockerNewErrStub) CheckDaemon(_ context.Context) error { return s.err }
func (s dockerNewErrStub) ImageExists(_ context.Context, _ string) (bool, error) {
	return false, s.err
}
