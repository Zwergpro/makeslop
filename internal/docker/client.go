package docker

import (
	"context"

	moby "github.com/moby/moby/client"
)

// apiClient is the narrow subset of moby's client.APIClient this package uses.
// *moby.Client satisfies it (see assertion below); tests inject a fake via WithClient.
type apiClient interface {
	ContainerList(ctx context.Context, options moby.ContainerListOptions) (moby.ContainerListResult, error)
	ContainerCreate(ctx context.Context, options moby.ContainerCreateOptions) (moby.ContainerCreateResult, error)
	ContainerAttach(ctx context.Context, container string, options moby.ContainerAttachOptions) (moby.ContainerAttachResult, error)
	ContainerStart(ctx context.Context, container string, options moby.ContainerStartOptions) (moby.ContainerStartResult, error)
	ContainerStop(
		ctx context.Context,
		container string,
		options moby.ContainerStopOptions,
	) (moby.ContainerStopResult, error)
	ExecCreate(ctx context.Context, execContainer string, options moby.ExecCreateOptions) (moby.ExecCreateResult, error)
	ExecStart(ctx context.Context, execID string, options moby.ExecStartOptions) (moby.ExecStartResult, error)
	ExecAttach(ctx context.Context, execID string, options moby.ExecAttachOptions) (moby.ExecAttachResult, error)
	ExecInspect(ctx context.Context, execID string, options moby.ExecInspectOptions) (moby.ExecInspectResult, error)
	ExecResize(ctx context.Context, execID string, options moby.ExecResizeOptions) (moby.ExecResizeResult, error)
	ContainerWait(ctx context.Context, container string, options moby.ContainerWaitOptions) moby.ContainerWaitResult
	ContainerResize(ctx context.Context, container string, options moby.ContainerResizeOptions) (moby.ContainerResizeResult, error)
	ContainerRemove(ctx context.Context, container string, options moby.ContainerRemoveOptions) (moby.ContainerRemoveResult, error)
	Ping(ctx context.Context, options moby.PingOptions) (moby.PingResult, error)
	ImageInspect(ctx context.Context, imageID string, opts ...moby.ImageInspectOption) (moby.ImageInspectResult, error)
	ContainerInspect(ctx context.Context, container string, options moby.ContainerInspectOptions) (moby.ContainerInspectResult, error)
	NetworkInspect(ctx context.Context, network string, options moby.NetworkInspectOptions) (moby.NetworkInspectResult, error)
	Close() error
}

// Guards against moby SDK signature drift.
var _ apiClient = (*moby.Client)(nil)

// newClient builds a Docker client from the environment (DOCKER_HOST etc.).
// The connection is lazy — it does not dial.
func newClient() (apiClient, error) {
	return moby.New(moby.FromEnv)
}
