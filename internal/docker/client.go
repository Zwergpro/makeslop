package docker

import (
	"context"
	"io"
	"net"

	moby "github.com/moby/moby/client"
)

// apiClient is the narrow subset of the moby/moby/client.APIClient interface
// that this package actually uses. *client.Client satisfies it (see the
// compile-time assertion below). Tests inject a fake via newClientFn.
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

// compile-time assertion: *moby.Client must satisfy apiClient.
var _ apiClient = (*moby.Client)(nil)

// newClient constructs a Docker client from the environment
// (DOCKER_HOST, DOCKER_TLS_VERIFY, DOCKER_CERT_PATH, DOCKER_API_VERSION).
// It does not dial — the connection is lazy.
func newClient() (apiClient, error) {
	return moby.NewClientWithOpts(moby.FromEnv, moby.WithAPIVersionNegotiation())
}

// newClientFn is the swap point used by tests to inject a fake apiClient.
// Production code always uses the default newClient.
var newClientFn = newClient
