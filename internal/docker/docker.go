package docker

import (
	"context"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
)

// Docker holds the dependencies shared by Run, Build, CheckDaemon, and
// ImageExists. Construct with New; call Close when done.
type Docker struct {
	client  apiClient
	isTTYFn func() bool
	makeRaw func(fd int) (*term.State, error)
}

// Option is a functional option for New.
type Option func(*Docker)

// WithClient injects a pre-built apiClient. Same-package _test.go only
// (apiClient is unexported).
func WithClient(c apiClient) Option {
	return func(d *Docker) {
		d.client = c
	}
}

// WithTTYCheck overrides the TTY-detection predicate used by Run.
func WithTTYCheck(fn func() bool) Option {
	return func(d *Docker) {
		d.isTTYFn = fn
	}
}

// WithRawMode overrides the terminal raw-mode function used by Run.
func WithRawMode(fn func(int) (*term.State, error)) Option {
	return func(d *Docker) {
		d.makeRaw = fn
	}
}

// New constructs a Docker with real defaults: an environment-built moby client,
// stdin+stdout TTY detection, and term.MakeRaw. Options apply before client
// construction so WithClient can suppress the real newClient() call (avoiding an
// orphaned transport when a test injects a fake).
func New(opts ...Option) (*Docker, error) {
	d := &Docker{
		isTTYFn: func() bool { return isTTY(os.Stdin) && isTTY(os.Stdout) },
		makeRaw: func(fd int) (*term.State, error) { return term.MakeRaw(fd) },
	}
	for _, o := range opts {
		o(d)
	}
	if d.client == nil {
		cli, err := newClient()
		if err != nil {
			return nil, fmt.Errorf("create docker client: %w", err)
		}
		d.client = cli
	}
	return d, nil
}

// Close releases the underlying Docker client connection.
func (d *Docker) Close() error {
	return d.client.Close()
}

// Run launches s interactively. Returns ErrNoTTY unless both stdin and stdout
// are TTYs, and *ExitError on non-zero container exit.
func (d *Docker) Run(ctx context.Context, s Spec) error {
	return runContainer(ctx, d.client, d.isTTYFn, d.makeRaw, s)
}

// Build builds the image described by o, writing output to out and errw.
// CI/pipe-safe; never checks for a TTY.
func (d *Docker) Build(ctx context.Context, o BuildOptions, out, errw io.Writer) error {
	return buildImage(ctx, d.client, o, out, errw)
}

// CheckDaemon pings the daemon, returning *ErrDaemonUnreachable on failure.
func (d *Docker) CheckDaemon(ctx context.Context) error {
	return checkDaemon(ctx, d.client)
}

// ImageExists reports whether the named image tag exists locally. (false, nil)
// only for a classified not-found; other errors return (false, err).
func (d *Docker) ImageExists(ctx context.Context, image string) (bool, error) {
	return imageExists(ctx, d.client, image)
}
