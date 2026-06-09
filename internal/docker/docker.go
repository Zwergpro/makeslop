package docker

import (
	"context"
	"fmt"
	"io"

	"golang.org/x/term"
)

// Docker holds the dependencies needed by Run, Build, CheckDaemon, and
// ImageExists. Construct one with New; all methods share the same client
// for the lifetime of the struct. Call Close when done.
type Docker struct {
	client   apiClient
	isTTY    func() bool
	makeRaw  func(fd int) (*term.State, error)
}

// Option is a functional option for New.
type Option func(*Docker)

// WithClient injects a pre-built apiClient. Intended for use by same-package
// _test.go files only (apiClient is unexported).
func WithClient(c apiClient) Option {
	return func(d *Docker) {
		d.client = c
	}
}

// WithTTYCheck overrides the TTY-detection predicate used by Run.
func WithTTYCheck(fn func() bool) Option {
	return func(d *Docker) {
		d.isTTY = fn
	}
}

// WithRawMode overrides the terminal raw-mode function used by Run.
func WithRawMode(fn func(int) (*term.State, error)) Option {
	return func(d *Docker) {
		d.makeRaw = fn
	}
}

// New constructs a Docker with real defaults: a moby client built from the
// environment (DOCKER_HOST etc.), stdin+stdout TTY detection, and real
// term.MakeRaw. Options are applied after the defaults.
//
// The default isTTY and makeRaw closures delegate through the package-level
// ttyCheck/termMakeRaw globals so that SetTTYCheckForTest and
// SetTermMakeRawForTest still take effect during the struct-DI migration
// (Task 3/4 transition). Similarly, the client is obtained via newClientFn so
// SetClientForTest takes effect. In Task 5 those globals are deleted.
func New(opts ...Option) (*Docker, error) {
	cli, err := newClientFn()
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	d := &Docker{
		client:  cli,
		isTTY:   func() bool { return ttyCheck() },
		makeRaw: func(fd int) (*term.State, error) { return termMakeRaw(fd) },
	}
	for _, o := range opts {
		o(d)
	}
	return d, nil
}

// Close releases the underlying Docker client connection.
func (d *Docker) Close() error {
	return d.client.Close()
}

// Run launches the container described by s interactively. It refuses to start
// (returning ErrNoTTY) unless both stdin and stdout are TTYs.
// On non-zero container exit, Run returns *ExitError with the exit code.
func (d *Docker) Run(ctx context.Context, s Spec) error {
	return run(ctx, d.client, d.isTTY, d.makeRaw, s)
}

// Build builds the docker image described by o, writing build output to out
// and errw. It is CI/pipe-safe and never checks for a TTY.
func (d *Docker) Build(ctx context.Context, o BuildOptions, out, errw io.Writer) error {
	return build(ctx, d.client, o, out, errw)
}

// CheckDaemon pings the Docker daemon. It returns *ErrDaemonUnreachable if the
// daemon cannot be reached, and nil on success.
func (d *Docker) CheckDaemon(ctx context.Context) error {
	return checkDaemon(ctx, d.client)
}

// ImageExists reports whether the named image tag exists locally.
//
//   - (true, nil)  — image found
//   - (false, nil) — image absent (cerrdefs.IsNotFound classified)
//   - (false, err) — any other error (daemon error, permission, …)
func (d *Docker) ImageExists(ctx context.Context, image string) (bool, error) {
	return imageExists(ctx, d.client, image)
}
