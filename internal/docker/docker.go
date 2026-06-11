package docker

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
)

// Docker holds the dependencies shared by Run, Build, CheckDaemon, and
// ImageExists. Construct with New; call Close when done.
type Docker struct {
	client              apiClient
	isTTYFn             func() bool
	makeRaw             func(fd int) (*term.State, error)
	stdin               io.Reader
	stdout              io.Writer
	resizeGoroutineHook func() // called at end of resize goroutine body; nil in production
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

// WithStreams overrides the stdin reader and stdout writer used by Run for
// container I/O. Fd-based terminal operations (raw mode, GetSize, resize) always
// use the real os.Stdin; only the data copies are redirected.
func WithStreams(in io.Reader, out io.Writer) Option {
	return func(d *Docker) {
		d.stdin = in
		d.stdout = out
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
		stdin:   os.Stdin,
		stdout:  os.Stdout,
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
