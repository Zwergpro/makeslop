package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
	buildtypes "github.com/moby/moby/api/types/build"
	"github.com/moby/moby/api/types/jsonstream"
	moby "github.com/moby/moby/client"
	"google.golang.org/protobuf/proto"
)

// ─── parseBuildArgs unit tests ───────────────────────────────────────────────

func TestParseBuildArgs_Empty(t *testing.T) {
	if got := parseBuildArgs(nil); got != nil {
		t.Errorf("parseBuildArgs(nil) = %v, want nil", got)
	}
	if got := parseBuildArgs([]string{}); got != nil {
		t.Errorf("parseBuildArgs([]) = %v, want nil", got)
	}
}

func TestParseBuildArgs_KeyValue(t *testing.T) {
	out := parseBuildArgs([]string{"GO_VERSION=1.26.3", "FOO=bar"})
	if out == nil {
		t.Fatal("parseBuildArgs returned nil")
	}
	if val, ok := out["GO_VERSION"]; !ok || val == nil || *val != "1.26.3" {
		t.Errorf("GO_VERSION = %v, want 1.26.3", val)
	}
	if val, ok := out["FOO"]; !ok || val == nil || *val != "bar" {
		t.Errorf("FOO = %v, want bar", val)
	}
}

func TestParseBuildArgs_KeyOnly_NilValue(t *testing.T) {
	// KEY with no "=" should produce a nil pointer value (daemon inherits from env).
	out := parseBuildArgs([]string{"KEYONLY"})
	if out == nil {
		t.Fatal("parseBuildArgs returned nil")
	}
	val, ok := out["KEYONLY"]
	if !ok {
		t.Fatal("KEYONLY key missing")
	}
	if val != nil {
		t.Errorf("KEYONLY value = %v, want nil", *val)
	}
}

func TestParseBuildArgs_EqualSignInValue(t *testing.T) {
	// "KEY=a=b" → key="KEY", value="a=b" (split on first = only).
	out := parseBuildArgs([]string{"KEY=a=b"})
	if out == nil {
		t.Fatal("parseBuildArgs returned nil")
	}
	val, ok := out["KEY"]
	if !ok || val == nil || *val != "a=b" {
		t.Errorf("KEY = %v, want a=b", val)
	}
}

func TestParseBuildArgs_EmptyValue(t *testing.T) {
	// "KEY=" → value is empty string (not nil).
	out := parseBuildArgs([]string{"KEY="})
	if out == nil {
		t.Fatal("parseBuildArgs returned nil")
	}
	val, ok := out["KEY"]
	if !ok || val == nil {
		t.Errorf("KEY is nil or missing, want empty string")
	} else if *val != "" {
		t.Errorf("KEY = %q, want empty string", *val)
	}
}

// ─── buildImageOptions unit tests ────────────────────────────────────────────

func TestBuildImageOptions_Mapping(t *testing.T) {
	o := BuildOptions{
		Image:          "claudebox",
		DockerfilePath: "/home/user/.makeslop/Dockerfile",
		ContextDir:     "/tmp/ctx",
		NoCache:        true,
		BuildArgs:      []string{"GO_VERSION=1.26.3"},
	}
	sessionID := "sess-abc123"
	opts := buildImageOptions(o, sessionID)

	// Tags must contain the image name.
	if len(opts.Tags) != 1 || opts.Tags[0] != "claudebox" {
		t.Errorf("Tags = %v, want [claudebox]", opts.Tags)
	}

	// Dockerfile is the basename of DockerfilePath.
	if opts.Dockerfile != filepath.Base(o.DockerfilePath) {
		t.Errorf("Dockerfile = %q, want %q", opts.Dockerfile, filepath.Base(o.DockerfilePath))
	}

	// NoCache propagated.
	if !opts.NoCache {
		t.Error("NoCache = false, want true")
	}

	// Version must be BuilderBuildKit.
	if opts.Version != buildtypes.BuilderBuildKit {
		t.Errorf("Version = %q, want %q", opts.Version, buildtypes.BuilderBuildKit)
	}

	// SessionID propagated.
	if opts.SessionID != sessionID {
		t.Errorf("SessionID = %q, want %q", opts.SessionID, sessionID)
	}

	// RemoteContext must be "client-session".
	if opts.RemoteContext != "client-session" {
		t.Errorf("RemoteContext = %q, want %q", opts.RemoteContext, "client-session")
	}

	// BuildArgs mapped.
	val, ok := opts.BuildArgs["GO_VERSION"]
	if !ok || val == nil || *val != "1.26.3" {
		t.Errorf("BuildArgs[GO_VERSION] = %v, want 1.26.3", val)
	}
}

func TestBuildImageOptions_NoCache_False(t *testing.T) {
	o := BuildOptions{
		Image:          "claudebox",
		DockerfilePath: "/etc/Dockerfile",
		NoCache:        false,
	}
	opts := buildImageOptions(o, "s")
	if opts.NoCache {
		t.Error("NoCache = true, want false")
	}
}

func TestBuildImageOptions_EmptyBuildArgs(t *testing.T) {
	o := BuildOptions{
		Image:          "claudebox",
		DockerfilePath: "/etc/Dockerfile",
	}
	opts := buildImageOptions(o, "s")
	if opts.BuildArgs != nil {
		t.Errorf("BuildArgs = %v, want nil", opts.BuildArgs)
	}
}

// ─── build() options assertions ──────────────────────────────────────────────

// recordingBuildClient wraps fakeClient and records ImageBuildOptions.
type recordingBuildClient struct {
	fakeClient
	lastOpts    *moby.ImageBuildOptions
	buildCalled bool
}

func (r *recordingBuildClient) ImageBuild(_ context.Context, _ io.Reader, opts moby.ImageBuildOptions) (moby.ImageBuildResult, error) {
	r.lastOpts = &opts
	r.buildCalled = true
	return moby.ImageBuildResult{Body: io.NopCloser(bytes.NewReader(nil))}, nil
}

// TestBuild_FakeSelf_ImageBuildOptions verifies that build passes the correct
// image tag, Dockerfile basename, BuildKit version, and RemoteContext to
// ImageBuild. The session may fail to dial (fakeClient.DialHijack returns an
// error), but we verify ImageBuild was called with the expected options.
func TestBuild_FakeSelf_ImageBuildOptions(t *testing.T) {
	// Create a temporary Dockerfile so fsutil.NewFS(dockerfileDir) succeeds on
	// any system, not just machines that have /home/user/.makeslop/.
	dockerfileDir := t.TempDir()
	dockerfilePath := filepath.Join(dockerfileDir, "Dockerfile")
	if err := os.WriteFile(dockerfilePath, []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write temp Dockerfile: %v", err)
	}

	rc := &recordingBuildClient{}
	o := BuildOptions{
		Image:          "claudebox",
		DockerfilePath: dockerfilePath,
		ContextDir:     t.TempDir(),
	}
	// build may return an error (session dial fails for fakeClient), but
	// ImageBuild should still be called before or during session teardown.
	_ = build(context.Background(), rc, o, io.Discard, io.Discard)

	if !rc.buildCalled {
		t.Fatal("ImageBuild was not called (session may have failed before reaching ImageBuild; integration-test covers full flow)")
	}
	opts := rc.lastOpts
	if len(opts.Tags) == 0 || opts.Tags[0] != "claudebox" {
		t.Errorf("Tags = %v, want [claudebox]", opts.Tags)
	}
	if opts.Dockerfile != "Dockerfile" {
		t.Errorf("Dockerfile = %q, want %q", opts.Dockerfile, "Dockerfile")
	}
	if string(opts.Version) != "2" {
		t.Errorf("Version = %q, want %q", opts.Version, "2")
	}
	if opts.RemoteContext != "client-session" {
		t.Errorf("RemoteContext = %q, want %q", opts.RemoteContext, "client-session")
	}
}

// ─── renderBuildOutput unit tests ────────────────────────────────────────────

// encodeMessages encodes a slice of jsonstream.Message into a JSON stream
// (one JSON object per line) as the daemon would produce it.
func encodeMessages(t *testing.T, msgs []jsonstream.Message) io.ReadCloser {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, m := range msgs {
		if err := enc.Encode(m); err != nil {
			t.Fatalf("encode message: %v", err)
		}
	}
	return io.NopCloser(&buf)
}

// TestRenderBuildOutput_PlainFallback: stream and status lines written to stdout.
func TestRenderBuildOutput_PlainFallback(t *testing.T) {
	msgs := []jsonstream.Message{
		{Stream: "Step 1/3 : FROM scratch\n"},
		{Status: "Pulling from library/scratch"},
		{Stream: "Successfully built abc123\n"},
	}
	body := encodeMessages(t, msgs)

	var out bytes.Buffer
	if err := renderBuildOutput(context.Background(), body, &out, io.Discard); err != nil {
		t.Fatalf("renderBuildOutput returned error: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Step 1/3") {
		t.Errorf("output missing 'Step 1/3': %q", got)
	}
	if !strings.Contains(got, "Successfully built") {
		t.Errorf("output missing 'Successfully built': %q", got)
	}
	if !strings.Contains(got, "Pulling from") {
		t.Errorf("output missing 'Pulling from': %q", got)
	}
}

// TestRenderBuildOutput_ErrorMessage: a message with an Error field returns an error.
func TestRenderBuildOutput_ErrorMessage(t *testing.T) {
	msgs := []jsonstream.Message{
		{Error: &jsonstream.Error{Message: "build failed: context expired"}},
	}
	body := encodeMessages(t, msgs)

	if err := renderBuildOutput(context.Background(), body, io.Discard, io.Discard); err == nil {
		t.Fatal("expected error from build error message, got nil")
	} else if !strings.Contains(err.Error(), "build failed") {
		t.Errorf("error should contain 'build failed', got %v", err)
	}
}

// TestRenderBuildOutput_EmptyBody: empty body returns nil (no messages = no error).
func TestRenderBuildOutput_EmptyBody(t *testing.T) {
	body := io.NopCloser(bytes.NewReader(nil))
	if err := renderBuildOutput(context.Background(), body, io.Discard, io.Discard); err != nil {
		t.Errorf("empty body: expected nil error, got %v", err)
	}
}

// ─── decodeBuildKitAux unit tests ─────────────────────────────────────────────

// TestDecodeBuildKitAux_NilAux: nil Aux returns nil.
func TestDecodeBuildKitAux_NilAux(t *testing.T) {
	msg := jsonstream.Message{Aux: nil}
	if got := decodeBuildKitAux(msg); got != nil {
		t.Errorf("nil Aux: expected nil, got %v", got)
	}
}

// TestDecodeBuildKitAux_NoTracKey: Aux without "moby.buildkit.trace" returns nil.
func TestDecodeBuildKitAux_NoTraceKey(t *testing.T) {
	raw := json.RawMessage(`{"other.key":"value"}`)
	msg := jsonstream.Message{Aux: &raw}
	if got := decodeBuildKitAux(msg); got != nil {
		t.Errorf("no trace key: expected nil, got %v", got)
	}
}

// TestDecodeBuildKitAux_InvalidProto: malformed proto bytes return nil (no panic).
func TestDecodeBuildKitAux_InvalidProto(t *testing.T) {
	// Encode junk bytes as the trace value (valid JSON base64, but bad proto).
	junk := []byte("this is not a valid proto message")
	junkJSON, err := json.Marshal(junk)
	if err != nil {
		t.Fatalf("marshal junk: %v", err)
	}
	auxJSON := fmt.Sprintf(`{"moby.buildkit.trace":%s}`, string(junkJSON))
	raw := json.RawMessage(auxJSON)
	msg := jsonstream.Message{Aux: &raw}
	// Should return nil (graceful degradation), not panic.
	if got := decodeBuildKitAux(msg); got != nil {
		t.Errorf("invalid proto: expected nil, got %v", got)
	}
}

// ─── streaming renderBuildOutput tests ───────────────────────────────────────

// makeAuxMessage builds a jsonstream.Message with a valid "moby.buildkit.trace"
// aux payload from an empty StatusResponse (so the message decodes cleanly).
func makeAuxMessage(t *testing.T) jsonstream.Message {
	t.Helper()
	sr := &controlapi.StatusResponse{}
	b, err := proto.Marshal(sr)
	if err != nil {
		t.Fatalf("proto.Marshal StatusResponse: %v", err)
	}
	bJSON, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("json.Marshal bytes: %v", err)
	}
	auxStr := fmt.Sprintf(`{"moby.buildkit.trace":%s}`, string(bJSON))
	auxJSON := json.RawMessage(auxStr)
	return jsonstream.Message{Aux: &auxJSON}
}

// TestRenderBuildOutput_StreamingTrace: a valid aux frame feeds the display and
// renderBuildOutput returns nil without hanging (no goroutine leak). Running
// under go test (no TTY) exercises the AutoMode→PlainMode fallback path.
func TestRenderBuildOutput_StreamingTrace(t *testing.T) {
	msgs := []jsonstream.Message{makeAuxMessage(t)}
	body := encodeMessages(t, msgs)

	if err := renderBuildOutput(context.Background(), body, io.Discard, io.Discard); err != nil {
		t.Fatalf("renderBuildOutput with trace frame returned error: %v", err)
	}
}

// TestRenderBuildOutput_TraceFollowedByError: a trace frame followed by a build
// error message must return an error and not hang (channel is closed, goroutine joined).
func TestRenderBuildOutput_TraceFollowedByError(t *testing.T) {
	msgs := []jsonstream.Message{
		makeAuxMessage(t),
		{Error: &jsonstream.Error{Message: "layer pull failed"}},
	}
	body := encodeMessages(t, msgs)

	err := renderBuildOutput(context.Background(), body, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected error from error message after trace frame, got nil")
	}
	if !strings.Contains(err.Error(), "layer pull failed") {
		t.Errorf("error should contain 'layer pull failed', got %v", err)
	}
}

// TestRenderBuildOutput_TraceAndPlainMixed: a stream with a plain message before
// a trace frame delivers the pre-trace plain message to stdout; plain messages
// arriving after the first trace frame are silently discarded (not written to
// stdout) to avoid a concurrent-write race with the progressui goroutine.
func TestRenderBuildOutput_TraceAndPlainMixed(t *testing.T) {
	msgs := []jsonstream.Message{
		{Stream: "pre-trace line\n"},
		makeAuxMessage(t),
		{Stream: "post-trace line\n"},
	}
	body := encodeMessages(t, msgs)

	var out bytes.Buffer
	if err := renderBuildOutput(context.Background(), body, &out, io.Discard); err != nil {
		t.Fatalf("renderBuildOutput returned error: %v", err)
	}
	got := out.String()
	// Pre-trace plain lines must reach stdout (the display is not yet active).
	if !strings.Contains(got, "pre-trace line") {
		t.Errorf("output missing 'pre-trace line': %q", got)
	}
	// Post-trace plain lines must NOT appear on stdout: once the progressui
	// display is active, plain messages are discarded to prevent a concurrent
	// write from the decode loop and the display goroutine.
	if strings.Contains(got, "post-trace line") {
		t.Errorf("post-trace plain line must not appear on stdout (would race with progressui): %q", got)
	}
}

// TestRenderBuildOutput_ContextCanceled: with a pre-cancelled context the
// function must return promptly (no hang). The error value is intentionally
// not asserted here because the outcome is non-deterministic: Go's select
// chooses randomly when both `case statusCh <- ss:` (the decode loop) and
// `case <-ctx.Done():` are simultaneously ready. If the channel send wins,
// the decode loop advances, decodeErr stays nil, the display goroutine drains
// with context.Canceled (which renderBuildOutput filters), and the function
// legitimately returns nil. If ctx.Done() wins first, decodeErr = Canceled
// and the function returns non-nil. Both are correct — context cancellation is
// surfaced opportunistically. The only invariant guaranteed by the
// implementation is that it returns without blocking.
func TestRenderBuildOutput_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel so the select in the decode loop fires immediately

	msgs := []jsonstream.Message{makeAuxMessage(t)}
	body := encodeMessages(t, msgs)

	// The sole contract: must return (no hang). Error may be nil or non-nil.
	done := make(chan struct{})
	go func() {
		renderBuildOutput(ctx, body, io.Discard, io.Discard) //nolint:errcheck
		close(done)
	}()
	select {
	case <-done:
		// returned promptly — pass
	case <-time.After(5 * time.Second):
		t.Fatal("renderBuildOutput blocked for >5s with pre-cancelled context")
	}
}
