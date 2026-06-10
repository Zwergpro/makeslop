package docker

import (
	"context"
	"io"
	"net"

	moby "github.com/moby/moby/client"
)

// apiClient is the narrow subset of moby's client.APIClient this package uses.
// *moby.Client satisfies it (see assertion below); tests inject a fake via WithClient.
type apiClient interface {
	ContainerCreate(ctx context.Context, options moby.ContainerCreateOptions) (moby.ContainerCreateResult, error)
	ContainerAttach(ctx context.Context, container string, options moby.ContainerAttachOptions) (moby.ContainerAttachResult, error)
	ContainerStart(ctx context.Context, container string, options moby.ContainerStartOptions) (moby.ContainerStartResult, error)
	ContainerWait(ctx context.Context, container string, options moby.ContainerWaitOptions) moby.ContainerWaitResult
	ContainerResize(ctx context.Context, container string, options moby.ContainerResizeOptions) (moby.ContainerResizeResult, error)
	ContainerRemove(ctx context.Context, container string, options moby.ContainerRemoveOptions) (moby.ContainerRemoveResult, error)
	ImageBuild(ctx context.Context, buildContext io.Reader, options moby.ImageBuildOptions) (moby.ImageBuildResult, error)
	DialHijack(ctx context.Context, url, proto string, meta map[string][]string) (net.Conn, error)
	Ping(ctx context.Context, options moby.PingOptions) (moby.PingResult, error)
	ImageInspect(ctx context.Context, imageID string, opts ...moby.ImageInspectOption) (moby.ImageInspectResult, error)
	Close() error
}

// Guards against moby SDK signature drift.
var _ apiClient = (*moby.Client)(nil)

// newClient builds a Docker client from the environment (DOCKER_HOST etc.).
// The connection is lazy — it does not dial.
func newClient() (apiClient, error) {
	return moby.New(moby.FromEnv)
}
