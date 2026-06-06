package docker

// Sidecar manages the lifecycle of an alpine/socat container that bridges a
// remote HTTP proxy to a unix socket on a Docker volume inside the VM.
//
// Architecture:
//
//	remote HTTP proxy (ip:port)
//	       ▲  via TCP-CONNECT over bridge networking
//	       │
//	┌──────┴─────────────────┐   makeslop-sock-<name>  (Docker volume, in-VM)
//	│ alpine/socat sidecar    │   UNIX-LISTEN:/sockets/proxy.sock,fork,mode=0666
//	│ (bridge networking)     │◄─────────────────────── volume ──► app container
//	└─────────────────────────┘                                     (--network none)
//
// The sidecar creates the unix socket inside the VM filesystem, side-stepping the
// host file-sharing boundary that breaks bind-mounts on Docker Desktop / macOS.
// Because the sidecar uses bridge networking, it can reach any remote ip:port
// directly — no host.docker.internal, no Docker-Desktop-only limitation.
//
// Security note: the app container stays airtight (--network none; sole egress is
// the volume unix socket → socat → remote HTTP proxy). This works on any Docker
// daemon (Docker Desktop and native Linux).

import (
	"context"
	"fmt"
	"io"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	moby "github.com/moby/moby/client"
)

// SocatImage is the pinned alpine/socat digest used for the proxy sidecar.
// Using a digest pin avoids pulling a different tag across updates.
// Digest obtained from docker.io/alpine/socat (multi-arch manifest).
// Exported so that status.go can check for the image presence.
const SocatImage = "alpine/socat@sha256:beb4a68d9e4fe6b0f21ea774a0fde6c31f580dde6368939ed70100c5385b015e"

// proxySocketName is the unix socket filename created by socat inside the volume.
const proxySocketName = "proxy.sock"

// Sidecar manages the socat container that creates the proxy unix socket inside
// the Docker VM on a named volume. The app container mounts the same volume
// read-only and connects to the socket.
type Sidecar struct {
	// volumeName is the Docker volume name used for the proxy socket.
	// Set by Start; used by Close.
	volumeName string

	// containerID is the ID of the running socat container.
	// Set by Start; used by Close and readiness poll.
	containerID string

	// quiet suppresses the "pulling socat image" notice when true.
	quiet bool

	// stderr is the writer for user-facing notices (pull progress, etc.).
	// If nil, pull notices are silently suppressed (quiet regardless of quiet flag).
	// Callers should pass the same writer as runRun.
	stderr io.Writer
}

// NewSidecar creates a new Sidecar with the given options. When quiet is true
// the pull notice is suppressed. stderr receives user-facing notices; pass
// os.Stderr in production.
func NewSidecar(quiet bool, stderr io.Writer) *Sidecar {
	return &Sidecar{quiet: quiet, stderr: stderr}
}

// Start ensures the socat image is present, creates the proxy volume, and
// launches the socat container. It then polls for the unix socket to appear
// inside the volume (readiness), aborting loudly if the sidecar exits early.
//
// upstream must be a "host:port" string identifying the remote HTTP proxy that
// socat will forward traffic to (e.g. "10.0.0.5:3128").
//
// On success, the caller should mount volumeName read-only at proxySocketDir
// (/sockets) in the app container.
func (s *Sidecar) Start(ctx context.Context, upstream string, volumeName string) error {
	cli, err := newClientFn()
	if err != nil {
		return fmt.Errorf("sidecar: create docker client: %w", err)
	}
	defer cli.Close() //nolint:errcheck // Start-only client; Close opens its own

	// 1. Ensure SocatImage is present; pull on demand.
	if err := s.ensureImage(ctx, cli); err != nil {
		return err
	}

	// 2. Create the volume (per-run name passed in by caller).
	// NOTE: s.volumeName is set only after VolumeCreate succeeds so that Close()
	// does not attempt to remove a volume that was never created.
	if _, err := cli.VolumeCreate(ctx, moby.VolumeCreateOptions{
		Name:   volumeName,
		Labels: map[string]string{"managed-by": "makeslop"},
	}); err != nil {
		return fmt.Errorf("sidecar: volume create %q: %w", volumeName, err)
	}
	s.volumeName = volumeName

	// 3. Create the socat container.
	//    - bridge networking so socat can reach the remote upstream directly
	//    - volume mounted read-write at /sockets so socat can create the socket
	//    - detached (no stdin/stdout/tty)
	socatCmd := []string{
		fmt.Sprintf("UNIX-LISTEN:%s/%s,fork,mode=0666", proxySocketDir, proxySocketName),
		fmt.Sprintf("TCP-CONNECT:%s,reuseaddr", upstream),
	}
	createRes, err := cli.ContainerCreate(ctx, moby.ContainerCreateOptions{
		Config: &container.Config{
			Image: SocatImage,
			Cmd:   socatCmd,
		},
		HostConfig: &container.HostConfig{
			NetworkMode: container.NetworkMode("bridge"),
			AutoRemove:  true, // daemon removes the container on exit; Close()'s ContainerRemove is best-effort and ignores not-found
			Mounts: []mount.Mount{
				{
					Type:     mount.TypeVolume,
					Source:   volumeName,
					Target:   proxySocketDir,
					ReadOnly: false,
				},
			},
		},
		Name: volumeName, // stable name derived from volume name
	})
	if err != nil {
		// Best-effort volume cleanup on create failure.
		_, _ = cli.VolumeRemove(context.Background(), volumeName, moby.VolumeRemoveOptions{})
		return fmt.Errorf("sidecar: container create: %w", err)
	}
	s.containerID = createRes.ID

	// 4. Start the container.
	if _, err := cli.ContainerStart(ctx, s.containerID, moby.ContainerStartOptions{}); err != nil {
		// Best-effort cleanup.
		_, _ = cli.ContainerRemove(context.Background(), s.containerID, moby.ContainerRemoveOptions{Force: true})
		_, _ = cli.VolumeRemove(context.Background(), volumeName, moby.VolumeRemoveOptions{})
		return fmt.Errorf("sidecar: container start: %w", err)
	}

	// 5. Poll for readiness: the socket must appear at /sockets/proxy.sock.
	if err := s.waitReady(ctx, cli); err != nil {
		// Tear down: container remove then volume.
		_, _ = cli.ContainerRemove(context.Background(), s.containerID, moby.ContainerRemoveOptions{Force: true})
		_, _ = cli.VolumeRemove(context.Background(), volumeName, moby.VolumeRemoveOptions{})
		return err
	}

	return nil
}

// Close removes the socat container (force) and then the volume. Both
// operations are best-effort and idempotent: not-found errors are ignored.
// Close is safe to call multiple times.
func (s *Sidecar) Close() error {
	if s.containerID == "" && s.volumeName == "" {
		return nil // nothing started
	}

	cli, err := newClientFn()
	if err != nil {
		// Can't clean up without a client — best effort.
		return nil
	}
	defer cli.Close() //nolint:errcheck

	if s.containerID != "" {
		_, _ = cli.ContainerRemove(context.Background(), s.containerID, moby.ContainerRemoveOptions{Force: true})
	}
	if s.volumeName != "" {
		_, _ = cli.VolumeRemove(context.Background(), s.volumeName, moby.VolumeRemoveOptions{})
	}
	return nil
}

// ensureImage checks whether SocatImage is locally present; if not, it pulls
// it and prints a one-line notice to s.stderr (suppressed when s.quiet is true).
// Any pull error is returned as a fatal, actionable error.
func (s *Sidecar) ensureImage(ctx context.Context, cli apiClient) error {
	_, err := cli.ImageInspect(ctx, SocatImage)
	if err == nil {
		return nil // already present
	}
	if !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("sidecar: inspect socat image: %w", err)
	}

	// Image absent — pull it.
	if !s.quiet && s.stderr != nil {
		fmt.Fprintf(s.stderr, "pulling socat image %s\n", SocatImage)
	}
	resp, pullErr := cli.ImagePull(ctx, SocatImage, moby.ImagePullOptions{})
	if pullErr != nil {
		return fmt.Errorf("sidecar: pull socat image %s: %w (hint: ensure Docker can reach registry.hub.docker.com)", SocatImage, pullErr)
	}
	// Drain and close the pull response to avoid leaking the connection.
	if resp != nil {
		_, _ = io.Copy(io.Discard, resp)
		_ = resp.Close()
	}
	return nil
}

// waitReady polls for the proxy.sock unix socket inside the sidecar volume,
// using the create→start→inspect exec handshake (test -S /sockets/proxy.sock).
// It is bounded to ~5 seconds with ~100ms intervals. On each poll iteration it
// also checks whether the sidecar exited early and aborts loudly if so.
func (s *Sidecar) waitReady(ctx context.Context, cli apiClient) error {
	const (
		maxWait  = 5 * time.Second
		interval = 100 * time.Millisecond
	)
	deadline := time.Now().Add(maxWait)
	sockPath := proxySocketDir + "/" + proxySocketName

	for {
		// Check sidecar liveness first.
		insp, err := cli.ContainerInspect(ctx, s.containerID, moby.ContainerInspectOptions{})
		if err != nil {
			return fmt.Errorf("sidecar: inspect container: %w", err)
		}
		if insp.Container.State != nil && !insp.Container.State.Running {
			return fmt.Errorf("sidecar: socat container exited early (exit code %d)", insp.Container.State.ExitCode)
		}

		// Run `test -S <sockPath>` to check socket existence.
		execRes, err := cli.ExecCreate(ctx, s.containerID, moby.ExecCreateOptions{
			Cmd: []string{"test", "-S", sockPath},
		})
		if err != nil {
			return fmt.Errorf("sidecar: exec create: %w", err)
		}

		// Detach: false makes ExecStart block until the exec completes, so
		// ExecInspect always sees a deterministic result (never the default
		// Running=false, ExitCode=0 of an unstarted exec).
		if _, err := cli.ExecStart(ctx, execRes.ID, moby.ExecStartOptions{Detach: false}); err != nil {
			return fmt.Errorf("sidecar: exec start: %w", err)
		}

		inspRes, err := cli.ExecInspect(ctx, execRes.ID, moby.ExecInspectOptions{})
		if err != nil {
			return fmt.Errorf("sidecar: exec inspect: %w", err)
		}

		if !inspRes.Running && inspRes.ExitCode == 0 {
			return nil // socket is ready
		}

		// Not ready yet — check deadline and back off.
		if time.Now().After(deadline) {
			return fmt.Errorf("sidecar: timed out waiting for proxy socket %s after %s", sockPath, maxWait)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}
