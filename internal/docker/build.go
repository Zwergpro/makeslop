// Build orchestration via the moby/moby SDK + a BuildKit session:
//  1. Create a session, register filesync (context + dockerfile dir) and an authprovider.
//  2. Run the session in a goroutine, wired to the daemon via DialHijack.
//  3. Call ImageBuild with Version=BuilderBuildKit and the session ID.
//  4. Render the daemon's build-trace stream via progressui, plain-text fallback otherwise.
//  5. Close the session (the client is owned by the *Docker struct).
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

// parseBuildArgs converts the "KEY=VALUE"/"KEY" slice into the
// map[string]*string that ImageBuildOptions.BuildArgs expects.
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
			// Bare KEY: nil pointer ⇒ daemon uses the build-arg from its own env.
			out[a] = nil
		}
	}
	return out
}

// buildImageOptions constructs an ImageBuildOptions from o and sessionID.
// Dockerfile is the basename — the daemon resolves it inside the session sync dir.
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

// dialerFor adapts cli.DialHijack (which takes a leading url) to the
// session.Dialer signature func(ctx, proto, meta).
func dialerFor(cli apiClient) session.Dialer {
	return func(ctx context.Context, proto string, meta map[string][]string) (net.Conn, error) {
		return cli.DialHijack(ctx, "/session", proto, meta)
	}
}

// stageDockerfile copies the Dockerfile into a fresh temp dir containing ONLY
// the Dockerfile, so the BuildKit dockerfile filesync never exposes sibling
// files (credentials, workspace cache) to the daemon. The caller must call
// cleanup() once (a no-op on error).
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
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		_ = os.RemoveAll(tmp)
		return "", func() {}, fmt.Errorf("write staged Dockerfile: %w", err)
	}
	return tmp, func() { _ = os.RemoveAll(tmp) }, nil
}

// buildImage implements (*Docker).Build with an injected apiClient. The caller
// owns the client lifetime; buildImage does NOT close it.
func buildImage(ctx context.Context, cli apiClient, o BuildOptions, stdout, stderr io.Writer) error {
	if o.ContextDir == "" {
		dir, err := os.MkdirTemp("", "makeslop-build-*")
		if err != nil {
			return fmt.Errorf("create build context dir: %w", err)
		}
		defer os.RemoveAll(dir) //nolint:errcheck
		o.ContextDir = dir
	}

	stagedDir, cleanupStaged, err := stageDockerfile(o.DockerfilePath)
	if err != nil {
		return fmt.Errorf("stage Dockerfile: %w", err)
	}
	defer cleanupStaged()

	ctxFS, err := fsutil.NewFS(o.ContextDir)
	if err != nil {
		return fmt.Errorf("create context dir FS: %w", err)
	}
	dfFS, err := fsutil.NewFS(stagedDir)
	if err != nil {
		return fmt.Errorf("create dockerfile dir FS: %w", err)
	}
	// Keys must be the logical names the BuildKit dockerfile frontend requests
	// ("context"/"dockerfile"), NOT on-disk paths — the daemon looks up by name.
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

	// drainSession closes the session and waits for its goroutine. Context
	// cancellation/deadline are filtered out because sess.Close cancels the
	// session's internal context.
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

	if err := renderBuildOutput(ctx, resp.Body, stdout, o.Quiet); err != nil {
		_ = drainSession() //nolint:errcheck // render error takes precedence
		return fmt.Errorf("render build output: %w", err)
	}
	return drainSession()
}

// renderBuildOutput decodes the daemon's JSON build stream. BuildKit trace aux
// payloads (moby.buildkit.trace) drive progressui; other frames fall back to
// plain stream/status text. The display is created lazily on the first trace
// frame so plain and empty/error streams never spin one up.
func renderBuildOutput(ctx context.Context, body io.ReadCloser, stdout io.Writer, quiet bool) error {
	dec := json.NewDecoder(body)

	// nil until the first BuildKit trace frame.
	var statusCh chan *bkclient.SolveStatus
	var dispErrCh chan error
	var decodeErr error

	// ensureDisplay lazily creates the display and the goroutine draining
	// statusCh; idempotent. AutoMode falls back to PlainMode off-TTY internally,
	// so NewDisplay(_, AutoMode) never errors in practice.
	ensureDisplay := func() error {
		if statusCh != nil {
			return nil
		}
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
			// Plain fallback. Once the display goroutine is active we discard
			// plain messages to avoid concurrent writes to stdout.
			if !quiet {
				if msg.Stream != "" {
					_, _ = fmt.Fprint(stdout, msg.Stream)
				} else if msg.Status != "" {
					_, _ = fmt.Fprintln(stdout, msg.Status)
				}
			}
		}
	}

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
// frame, or nil for non-trace frames / decode failures. Daemon wire format:
// msg.ID == "moby.buildkit.trace", msg.Aux a JSON string whose base64 contents
// are a proto-marshalled controlapi.StatusResponse.
func decodeBuildKitAux(msg jsonstream.Message) *bkclient.SolveStatus {
	if msg.ID != "moby.buildkit.trace" || msg.Aux == nil {
		return nil
	}
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
