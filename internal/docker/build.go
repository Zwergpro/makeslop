// Build orchestration via BuildKit session.
//
// This file implements makeslop build via the moby/moby/client SDK instead of
// shelling out to the docker CLI. The flow is:
//
//  1. Create a BuildKit session and register filesync (context + dockerfile dir)
//     and a docker-config authprovider.
//  2. Run the session in a goroutine, wiring it to the daemon via DialHijack.
//  3. Call ImageBuild with Version=BuilderBuildKit, SessionID, and
//     RemoteContext="client-session".
//  4. Render the daemon's build-trace stream via progressui (BuildKit) or plain
//     text fallback for non-BuildKit streams.
//  5. Close session + client.
package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	controlapi "github.com/moby/buildkit/api/services/control"
	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/frontend/dockerui"
	"github.com/moby/buildkit/session/auth/authprovider"
	"github.com/moby/buildkit/session/filesync"
	"github.com/moby/buildkit/util/progress/progressui"
	buildtypes "github.com/moby/moby/api/types/build"
	"github.com/moby/moby/api/types/jsonstream"
	moby "github.com/moby/moby/client"
	"github.com/tonistiigi/fsutil"
	"google.golang.org/protobuf/proto"
)

// parseBuildArgs converts the CLI "KEY=VALUE" or "KEY" slice into the
// map[string]*string form that ImageBuildOptions.BuildArgs expects.
// Pure: same input → same output.
func parseBuildArgs(args []string) map[string]*string {
	if len(args) == 0 {
		return nil
	}
	out := make(map[string]*string, len(args))
	for _, a := range args {
		if idx := strings.Index(a, "="); idx >= 0 {
			key := a[:idx]
			val := a[idx+1:]
			out[key] = &val
		} else {
			// KEY with no value: pass a nil pointer so the daemon uses the
			// build-arg from its own environment (standard docker behaviour).
			out[a] = nil
		}
	}
	return out
}

// buildImageOptions constructs an ImageBuildOptions from o and the sessionID.
// Pure: never touches the filesystem. Dockerfile is the basename of
// opts.DockerfilePath (the daemon resolves it inside the session sync dir).
func buildImageOptions(o BuildOptions, sessionID string) moby.ImageBuildOptions {
	return moby.ImageBuildOptions{
		Tags:          []string{o.Image},
		Dockerfile:    filepath.Base(o.DockerfilePath),
		NoCache:       o.NoCache,
		BuildArgs:     parseBuildArgs(o.BuildArgs),
		Version:       buildtypes.BuilderBuildKit,
		SessionID:     sessionID,
		RemoteContext: "client-session",
	}
}

// dialerFor wraps cli.DialHijack so it matches the session.Dialer signature.
// session.Dialer is func(ctx, proto, meta) (net.Conn, error) while DialHijack
// takes a leading url argument.
func dialerFor(cli apiClient) session.Dialer {
	return func(ctx context.Context, proto string, meta map[string][]string) (net.Conn, error) {
		return cli.DialHijack(ctx, "/session", proto, meta)
	}
}

// stageDockerfile copies the Dockerfile at path into a fresh temporary
// directory, returning the directory path and a cleanup function that removes
// it. The staged directory contains ONLY the Dockerfile, so the BuildKit
// filesync sync for the dockerfile key never exposes sibling files (e.g.
// credentials or workspace cache) to the build daemon.
//
// The caller must invoke cleanup() exactly once, typically via defer.
// If stageDockerfile returns an error, cleanup is a no-op (the temp dir, if
// any, has already been removed before returning).
//
// Note: .dockerignore siblings are intentionally not staged — none are
// expected in ~/.makeslop and the build context sent to the daemon is empty.
func stageDockerfile(path string) (dir string, cleanup func(), err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", func() {}, fmt.Errorf("read Dockerfile %q: %w", path, err)
	}
	tmp, err := os.MkdirTemp("", "makeslop-dockerfile-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("create dockerfile stage dir: %w", err)
	}
	dest := filepath.Join(tmp, "Dockerfile")
	if err := os.WriteFile(dest, data, 0o600); err != nil {
		_ = os.RemoveAll(tmp)
		return "", func() {}, fmt.Errorf("write staged Dockerfile: %w", err)
	}
	return tmp, func() { _ = os.RemoveAll(tmp) }, nil
}

// Build builds the docker image described by o, writing build output to stdout
// and stderr. When o.ContextDir is empty, Build creates a temporary empty
// directory as the build context (the Dockerfile never COPYs from context) and
// removes it on return.
//
// Build never checks for a TTY and is CI/pipe-safe.
func Build(ctx context.Context, o BuildOptions, stdout, stderr io.Writer) error {
	cli, err := newClientFn()
	if err != nil {
		return fmt.Errorf("create docker client: %w", err)
	}
	return build(ctx, cli, o, stdout, stderr)
}

// build is the internal implementation of Build with an injected apiClient.
func build(ctx context.Context, cli apiClient, o BuildOptions, stdout, stderr io.Writer) error {
	if o.ContextDir == "" {
		dir, err := os.MkdirTemp("", "makeslop-build-*")
		if err != nil {
			return fmt.Errorf("create build context dir: %w", err)
		}
		defer os.RemoveAll(dir) //nolint:errcheck
		o.ContextDir = dir
	}
	defer cli.Close() //nolint:errcheck

	// Stage the Dockerfile in an isolated temp dir so that only the Dockerfile
	// is synced to the daemon — not the entire ~/.makeslop/ directory (which
	// contains credentials and workspace cache trees).
	stagedDir, cleanupStaged, err := stageDockerfile(o.DockerfilePath)
	if err != nil {
		return fmt.Errorf("stage Dockerfile: %w", err)
	}
	defer cleanupStaged()

	// Build the filesync DirSource: both the (empty) build-context dir and the
	// staged dockerfile dir must be synced so the daemon can read them.
	ctxFS, err := fsutil.NewFS(o.ContextDir)
	if err != nil {
		return fmt.Errorf("create context dir FS: %w", err)
	}
	dfFS, err := fsutil.NewFS(stagedDir)
	if err != nil {
		return fmt.Errorf("create dockerfile dir FS: %w", err)
	}
	// Keys must be the logical names the BuildKit dockerfile frontend requests
	// over the filesync protocol ("context" / "dockerfile"), NOT the on-disk
	// paths — the daemon looks dirs up by name, not by path.
	dirSource := filesync.StaticDirSource{
		dockerui.DefaultLocalNameContext:    ctxFS,
		dockerui.DefaultLocalNameDockerfile: dfFS,
	}

	sess, err := session.NewSession(ctx, "makeslop")
	if err != nil {
		return fmt.Errorf("create build session: %w", err)
	}
	sess.Allow(filesync.NewFSSyncProvider(dirSource))
	sess.Allow(authprovider.NewDockerAuthProvider(authprovider.DockerAuthProviderConfig{}))

	dialer := dialerFor(cli)
	sessionErrCh := make(chan error, 1)
	go func() {
		sessionErrCh <- sess.Run(ctx, dialer)
	}()

	// drainSession closes the session and waits for the session goroutine to
	// exit. It returns a non-nil error only when the goroutine reports an
	// unexpected failure (context cancellation and deadline are filtered out
	// because sess.Close cancels the session's internal context).
	drainSession := func() error {
		_ = sess.Close()       //nolint:errcheck // best-effort; goroutine exit is what matters
		serr := <-sessionErrCh // always wait — prevents goroutine leak
		if serr != nil && !errors.Is(serr, context.Canceled) && !errors.Is(serr, context.DeadlineExceeded) {
			return fmt.Errorf("build session: %w", serr)
		}
		return nil
	}

	opts := buildImageOptions(o, sess.ID())
	resp, err := cli.ImageBuild(ctx, nil, opts)
	if err != nil {
		_ = drainSession() //nolint:errcheck // ImageBuild error takes precedence
		return fmt.Errorf("image build: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	// Render build progress. We attempt the progressui / build-trace decoder
	// first; if the decode channel receives nothing within the first message,
	// we fall back to the plain jsonmessage stream renderer.
	if err := renderBuildOutput(ctx, resp.Body, stdout, o.Quiet); err != nil {
		_ = drainSession() //nolint:errcheck // render error takes precedence
		return fmt.Errorf("render build output: %w", err)
	}

	// Drain the session after the body is fully consumed.
	return drainSession()
}

// renderBuildOutput renders the daemon's build output stream. It decodes the
// JSON message stream and:
//   - If the stream contains BuildKit trace aux payloads (moby.buildkit.trace),
//     routes them to progressui for the "[+] Building" UI (display created lazily
//     on the first trace frame so concurrent streaming starts immediately).
//   - Otherwise falls back to plain text rendering of stream/status messages.
//
// The display is created lazily: it is not allocated until the first BuildKit
// trace frame arrives, so pure-plain and empty/error streams never spin up a
// progressui display. This preserves existing test semantics while enabling
// live streaming for BuildKit builds.
func renderBuildOutput(ctx context.Context, body io.ReadCloser, stdout io.Writer, quiet bool) error {
	dec := json.NewDecoder(body)

	// statusCh and dispErrCh are nil until the first BuildKit trace frame.
	var statusCh chan *bkclient.SolveStatus
	var dispErrCh chan error
	var decodeErr error

	// ensureDisplay lazily creates the progressui display and starts the
	// goroutine that drains statusCh. It is idempotent after first call.
	// AutoMode falls back internally to PlainMode when stdout is not a TTY
	// (e.g. under go test or when piped), so a separate PlainMode fallback
	// is unnecessary — NewDisplay(_, AutoMode) never errors in practice.
	ensureDisplay := func() error {
		if statusCh != nil {
			return nil
		}
		// QuietMode discards all output; AutoMode renders the live UI (falling
		// back to PlainMode when stdout is not a TTY).
		mode := progressui.AutoMode
		if quiet {
			mode = progressui.QuietMode
		}
		d, err := progressui.NewDisplay(stdout, mode)
		if err != nil {
			return fmt.Errorf("create progress display: %w", err)
		}
		statusCh = make(chan *bkclient.SolveStatus)
		dispErrCh = make(chan error, 1)
		go func() { _, derr := d.UpdateFrom(ctx, statusCh); dispErrCh <- derr }()
		return nil
	}

	for {
		var msg jsonstream.Message
		if err := dec.Decode(&msg); err != nil {
			if !errors.Is(err, io.EOF) {
				decodeErr = fmt.Errorf("decode build stream: %w", err)
			}
			break
		}
		if msg.Error != nil {
			decodeErr = fmt.Errorf("build error: %s", msg.Error.Message)
			break
		}
		if ss := decodeBuildKitAux(msg); ss != nil {
			// First BuildKit trace frame: spin up the display.
			if err := ensureDisplay(); err != nil {
				decodeErr = err
				break
			}
			select {
			case statusCh <- ss:
			case <-ctx.Done():
				decodeErr = ctx.Err()
			}
			if decodeErr != nil {
				break
			}
		} else if statusCh == nil {
			// Plain fallback: write stream/status lines to stdout immediately.
			// Once the display goroutine is active (statusCh != nil) we discard
			// plain messages to avoid a concurrent write to stdout from both the
			// decode loop and the progressui goroutine. Suppressed entirely
			// under --quiet.
			if !quiet {
				if msg.Stream != "" {
					_, _ = fmt.Fprint(stdout, msg.Stream)
				} else if msg.Status != "" {
					_, _ = fmt.Fprintln(stdout, msg.Status)
				}
			}
		}
	}

	// Always close the display channel and drain the goroutine before returning.
	var dispErr error
	if statusCh != nil {
		close(statusCh)
		dispErr = <-dispErrCh
	}
	if decodeErr != nil {
		return decodeErr
	}
	if dispErr != nil && !errors.Is(dispErr, context.Canceled) && !errors.Is(dispErr, context.DeadlineExceeded) {
		return fmt.Errorf("render progress: %w", dispErr)
	}
	return nil
}

// decodeBuildKitAux extracts a *bkclient.SolveStatus from a BuildKit trace
// jsonstream.Message, or returns nil if the message is not a trace frame or
// decoding fails.
//
// The real daemon wire format is an id-keyed message:
//
//	{ "id": "moby.buildkit.trace", "aux": "<base64-of-proto-StatusResponse>" }
//
// i.e. msg.ID == "moby.buildkit.trace" and msg.Aux is a JSON string whose
// base64 contents are the proto-marshalled controlapi.StatusResponse. Other aux
// frames (e.g. "moby.image.id") and plain stream frames must be ignored.
func decodeBuildKitAux(msg jsonstream.Message) *bkclient.SolveStatus {
	if msg.ID != "moby.buildkit.trace" || msg.Aux == nil {
		return nil
	}
	// msg.Aux is a JSON string containing base64-encoded proto bytes.
	var dt []byte
	if err := json.Unmarshal(*msg.Aux, &dt); err != nil {
		return nil
	}
	var sr controlapi.StatusResponse
	if err := proto.Unmarshal(dt, &sr); err != nil {
		return nil
	}
	return bkclient.NewSolveStatus(&sr)
}
