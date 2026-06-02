package docker

// Sidecar manages the lifecycle of an alpine/socat container that bridges the
// host TCP proxy port to a unix socket on a Docker volume inside the VM.
//
// Architecture:
//
//	host Gateway (TCP 127.0.0.1:<port>)
//	       ▲  via host.docker.internal:<port>
//	       │
//	┌──────┴─────────────────┐   makeslop-sock-<name>  (Docker volume, in-VM)
//	│ alpine/socat sidecar    │   UNIX-LISTEN:/sockets/proxy.sock,fork,mode=0666
//	│ (bridge networking)     │◄─────────────────────── volume ──► app container
//	└─────────────────────────┘                                     (--network none)
//
// The sidecar creates the unix socket inside the VM filesystem, side-stepping the
// host file-sharing boundary that breaks bind-mounts on Docker Desktop / macOS.
//
// Security note: the app container stays airtight (--network none; sole egress is
// the volume unix socket). The host proxy listens on 127.0.0.1 at an ephemeral
// TCP port reachable via host.docker.internal. In gateway mode this grants nothing
// new (local processes already have direct internet); in upstream mode other local
// processes could borrow the upstream — acceptable for a single-user dev tool.
//
// Known limitation: host.docker.internal is provided by Docker Desktop; native-Linux
// daemons do not supply it by default. This path is intentionally Docker-Desktop-only.

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
const SocatImage = "alpine/socat@sha256:8d83acbdc16f926f3d7ffc9a6d50a6a63e51b2b8c88bba6c4b68bc07028c0bb7"

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
// On success, VolumeName() returns the volume name the app container must mount
// read-only at proxySocketDir (/sockets).
func (s *Sidecar) Start(ctx context.Context, port int, volumeName string) error {
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
	//    - bridge networking so host.docker.internal resolves to the host
	//    - volume mounted read-write at /sockets so socat can create the socket
	//    - detached (no stdin/stdout/tty)
	socatCmd := []string{
		fmt.Sprintf("UNIX-LISTEN:%s/%s,fork,mode=0666", proxySocketDir, proxySocketName),
		fmt.Sprintf("TCP-CONNECT:host.docker.internal:%d,reuseaddr", port),
	}
	createRes, err := cli.ContainerCreate(ctx, moby.ContainerCreateOptions{
		Config: &container.Config{
			Image: SocatImage,
			Cmd:   socatCmd,
		},
		HostConfig: &container.HostConfig{
			NetworkMode: container.NetworkMode("bridge"),
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

// VolumeName returns the Docker volume name that holds the proxy unix socket.
// Only valid after a successful Start.
func (s *Sidecar) VolumeName() string {
	return s.volumeName
}

// Close removes the socat container (force) and then the volume. Both
// operations are best-effort and idempotent: not-found errors are ignored.
// Close is safe to call multiple times.
func (s *Sidecar) Close() error {
	if s.containerID == "" && s.volumeName == "" {
		return nil // nothing started
	}

	// Capture and clear the IDs before any I/O so that concurrent or
	// repeated Close() calls are truly idempotent and do not re-attempt
	// removal of resources that have already been cleaned up.
	cid := s.containerID
	vol := s.volumeName
	s.containerID = ""
	s.volumeName = ""

	cli, err := newClientFn()
	if err != nil {
		// Can't clean up without a client — best effort.
		return nil
	}
	defer cli.Close() //nolint:errcheck

	if cid != "" {
		_, _ = cli.ContainerRemove(context.Background(), cid, moby.ContainerRemoveOptions{Force: true})
	}
	if vol != "" {
		_, _ = cli.VolumeRemove(context.Background(), vol, moby.VolumeRemoveOptions{})
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
		if insp.Container.State == nil {
			return fmt.Errorf("sidecar: container inspect returned nil State (unknown container status)")
		}
		if !insp.Container.State.Running {
			return fmt.Errorf("sidecar: socat container exited early (exit code %d)", insp.Container.State.ExitCode)
		}

		// Run `test -S <sockPath>` to check socket existence.
		execRes, err := cli.ExecCreate(ctx, s.containerID, moby.ExecCreateOptions{
			Cmd: []string{"test", "-S", sockPath},
		})
		if err != nil {
			return fmt.Errorf("sidecar: exec create: %w", err)
		}

		// ExecAttach (hijacked connection) properly blocks until the exec exits:
		// the server closes the connection only after the process terminates,
		// so reading the response body to EOF guarantees ExecInspect sees the
		// final exit code.
		//
		// ExecStart with Detach:false does NOT block until exec exits — the moby
		// client calls post() whose defer ensureReaderClosed drains only 512 bytes
		// and returns. An immediate ExecInspect after ExecStart can therefore read
		// the pre-run default state (Running=false, ExitCode=0), producing a
		// false-positive "socket ready" result.
		attachRes, err := cli.ExecAttach(ctx, execRes.ID, moby.ExecAttachOptions{})
		if err != nil {
			return fmt.Errorf("sidecar: exec attach: %w", err)
		}
		// Drain the hijacked connection to EOF; this blocks until the exec process
		// exits. The exec produces no output (no TTY, no attach flags) so the read
		// returns almost immediately after the process terminates.
		_, _ = io.Copy(io.Discard, attachRes.Reader)
		attachRes.Close()

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
