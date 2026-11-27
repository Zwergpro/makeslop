package cli

import (
	"context"
	"errors"
	"fmt"
	"testing"

	moby "github.com/moby/moby/client"

	"github.com/Zwergpro/makeslop/internal/docker"
)

func testBuildForeground(t *testing.T, in docker.BuildInputs) docker.ForegroundSpec {
	t.Helper()
	spec, err := docker.BuildForeground(in)
	if err != nil {
		t.Fatal(err)
	}

	return spec
}

// Stop at the SDK boundary without contacting a daemon or manipulating a TTY.
var errCapturedCreate = errors.New("test captured Docker create request")

type captureCreateClient struct {
	*moby.Client
	options moby.ContainerCreateOptions
}

func (c *captureCreateClient) ContainerCreate(
	_ context.Context,
	options moby.ContainerCreateOptions,
) (moby.ContainerCreateResult, error) {
	c.options = options

	return moby.ContainerCreateResult{}, errCapturedCreate
}

func captureCreate(run func(*docker.Docker) error) (moby.ContainerCreateOptions, error) {
	c := &captureCreateClient{}
	d, err := docker.New(docker.WithClient(c), docker.WithTTYCheck(func() bool { return true }))
	if err != nil {
		return moby.ContainerCreateOptions{}, err
	}

	err = run(d)
	if !errors.Is(err, errCapturedCreate) {
		return moby.ContainerCreateOptions{}, fmt.Errorf("expected capture at ContainerCreate, got %v", err)
	}

	return c.options, nil
}
