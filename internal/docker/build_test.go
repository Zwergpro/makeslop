package docker

import (
	"bytes"
	"context"
	"encoding/json"
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
	"google.golang.org/protobuf/types/known/timestamppb"
)

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
	// KEY with no "=" → nil pointer value (daemon inherits from env).
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

	if len(opts.Tags) != 1 || opts.Tags[0] != "claudebox" {
		t.Errorf("Tags = %v, want [claudebox]", opts.Tags)
	}
	if opts.Dockerfile != filepath.Base(o.DockerfilePath) {
		t.Errorf("Dockerfile = %q, want %q", opts.Dockerfile, filepath.Base(o.DockerfilePath))
	}
	if !opts.NoCache {
		t.Error("NoCache = false, want true")
	}
	if opts.Version != buildtypes.BuilderBuildKit {
		t.Errorf("Version = %q, want %q", opts.Version, buildtypes.BuilderBuildKit)
	}
	if opts.SessionID != sessionID {
		t.Errorf("SessionID = %q, want %q", opts.SessionID, sessionID)
	}
	if opts.RemoteContext != "client-session" {
		t.Errorf("RemoteContext = %q, want %q", opts.RemoteContext, "client-session")
	}
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

// Verifies Build passes the correct ImageBuild options. fakeClient.DialHijack
// fails the session, but ImageBuild is reached first; this test only checks
// option propagation (the integration test covers the full flow).
func TestBuild_FakeSelf_ImageBuildOptions(t *testing.T) {
	// Temp Dockerfile so fsutil.NewFS(dockerfileDir) succeeds on any system.
	dockerfileDir := t.TempDir()
	dockerfilePath := filepath.Join(dockerfileDir, "Dockerfile")
	if err := os.WriteFile(dockerfilePath, []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write temp Dockerfile: %v", err)
	}

	rc := &recordingBuildClient{}
	d := newDockerWithClient(t, rc)
	o := BuildOptions{
		Image:          "claudebox",
		DockerfilePath: dockerfilePath,
		ContextDir:     t.TempDir(),
	}
	// Error expected (DialHijack fails the session); only the recorded options matter.
	_ = d.Build(context.Background(), o, io.Discard, io.Discard)

	if !rc.buildCalled {
		t.Fatal("ImageBuild was not called; ensure DialHijack error does not race past ImageBuild")
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

// encodeMessages encodes messages as the daemon's newline-delimited JSON stream.
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

func TestRenderBuildOutput_PlainFallback(t *testing.T) {
	msgs := []jsonstream.Message{
		{Stream: "Step 1/3 : FROM scratch\n"},
		{Status: "Pulling from library/scratch"},
		{Stream: "Successfully built abc123\n"},
	}
	body := encodeMessages(t, msgs)

	var out bytes.Buffer
	if err := renderBuildOutput(context.Background(), body, &out, false); err != nil {
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

func TestRenderBuildOutput_QuietPlainSuppressed(t *testing.T) {
	msgs := []jsonstream.Message{
		{Stream: "Step 1/3 : FROM scratch\n"},
		{Status: "Pulling from library/scratch"},
		{Stream: "Successfully built abc123\n"},
	}
	body := encodeMessages(t, msgs)

	var out bytes.Buffer
	if err := renderBuildOutput(context.Background(), body, &out, true); err != nil {
		t.Fatalf("renderBuildOutput returned error: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("quiet plain output should be empty, got %q", out.String())
	}
}

func TestRenderBuildOutput_ErrorMessage(t *testing.T) {
	msgs := []jsonstream.Message{
		{Error: &jsonstream.Error{Message: "build failed: context expired"}},
	}
	body := encodeMessages(t, msgs)

	if err := renderBuildOutput(context.Background(), body, io.Discard, false); err == nil {
		t.Fatal("expected error from build error message, got nil")
	} else if !strings.Contains(err.Error(), "build failed") {
		t.Errorf("error should contain 'build failed', got %v", err)
	}
}

func TestRenderBuildOutput_EmptyBody(t *testing.T) {
	body := io.NopCloser(bytes.NewReader(nil))
	if err := renderBuildOutput(context.Background(), body, io.Discard, false); err != nil {
		t.Errorf("empty body: expected nil error, got %v", err)
	}
}

// Malformed JSON must yield a "decode build stream" error, not a panic or hang.
func TestRenderBuildOutput_MalformedJSON(t *testing.T) {
	body := io.NopCloser(strings.NewReader("this is not json\n"))
	err := renderBuildOutput(context.Background(), body, io.Discard, false)
	if err == nil {
		t.Fatal("expected error for malformed JSON body, got nil")
	}
	if !strings.Contains(err.Error(), "decode build stream") {
		t.Errorf("error should mention 'decode build stream', got %v", err)
	}
}

// Isolates the nil-Aux guard (correct ID, nil Aux → nil), separate from the ID-gate.
func TestDecodeBuildKitAux_NilAux(t *testing.T) {
	msg := jsonstream.Message{ID: "moby.buildkit.trace", Aux: nil}
	if got := decodeBuildKitAux(msg); got != nil {
		t.Errorf("nil Aux: expected nil, got %v", got)
	}
}

// A trace-ID frame with junk proto bytes in Aux must return nil, not panic.
func TestDecodeBuildKitAux_InvalidProto(t *testing.T) {
	// 0xff,0xff is an invalid protobuf tag/wire-type and reliably fails Unmarshal.
	junk := []byte{0xff, 0xff}
	auxBytes, err := json.Marshal(junk)
	if err != nil {
		t.Fatalf("json.Marshal junk: %v", err)
	}
	raw := json.RawMessage(auxBytes)
	msg := jsonstream.Message{ID: "moby.buildkit.trace", Aux: &raw}
	if got := decodeBuildKitAux(msg); got != nil {
		t.Errorf("invalid proto: expected nil, got %v", got)
	}
}

// realDaemonFrame builds a jsonstream.Message matching the daemon wire format
// for a BuildKit trace frame: {"id":"moby.buildkit.trace","aux":"<base64 proto>"}.
func realDaemonFrame(t *testing.T, sr *controlapi.StatusResponse) jsonstream.Message {
	t.Helper()
	dt, err := proto.Marshal(sr)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}
	auxBytes, err := json.Marshal(dt)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	raw := json.RawMessage(auxBytes)
	return jsonstream.Message{ID: "moby.buildkit.trace", Aux: &raw}
}

// Regression: a real daemon-shaped trace frame must decode to a non-nil
// SolveStatus. The original bug assumed a nested-map Aux shape and dropped every frame.
func TestDecodeBuildKitAux_RealFrame(t *testing.T) {
	digest := "sha256:abc"
	name := "[1/2] FROM alpine"
	now := timestamppb.Now()
	sr := &controlapi.StatusResponse{
		Vertexes: []*controlapi.Vertex{
			{Digest: digest, Name: name, Started: now},
		},
	}
	msg := realDaemonFrame(t, sr)
	ss := decodeBuildKitAux(msg)
	if ss == nil {
		t.Fatal("decodeBuildKitAux returned nil for a real daemon-shaped frame")
	}
	if len(ss.Vertexes) == 0 {
		t.Fatal("SolveStatus has no vertexes")
	}
	got := ss.Vertexes[0]
	if got.Name != name {
		t.Errorf("vertex Name = %q, want %q", got.Name, name)
	}
	if got.Digest.String() != digest {
		t.Errorf("vertex Digest = %q, want %q", got.Digest.String(), digest)
	}
}

// Frames with a wrong or absent ID must be ignored even with a parseable Aux
// payload (daemon also emits non-trace frames like "moby.image.id" and bare-Aux
// frames). Reuses realDaemonFrame's payload to isolate the ID-gate from decode failures.
func TestDecodeBuildKitAux_WrongID(t *testing.T) {
	frame := realDaemonFrame(t, &controlapi.StatusResponse{})
	raw := *frame.Aux

	cases := []struct {
		name string
		id   string
	}{
		{"no ID (bare-Aux frame)", ""},
		{"non-trace ID moby.image.id", "moby.image.id"},
		{"unrelated ID", "something.else"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := jsonstream.Message{ID: tc.id, Aux: &raw}
			if got := decodeBuildKitAux(msg); got != nil {
				t.Errorf("ID=%q: expected nil, got %v", tc.id, got)
			}
		})
	}
}

// A valid aux frame feeds the display and renderBuildOutput returns without
// hanging (no goroutine leak). stdout is captured so a wire-format regression
// (empty output) fails the test instead of passing silently.
func TestRenderBuildOutput_StreamingTrace(t *testing.T) {
	sr := &controlapi.StatusResponse{
		Vertexes: []*controlapi.Vertex{
			{Digest: "sha256:abc", Name: "[1/2] FROM alpine", Started: timestamppb.Now()},
		},
	}
	msgs := []jsonstream.Message{realDaemonFrame(t, sr)}
	body := encodeMessages(t, msgs)

	var out bytes.Buffer
	if err := renderBuildOutput(context.Background(), body, &out, false); err != nil {
		t.Fatalf("renderBuildOutput with trace frame returned error: %v", err)
	}
	if !strings.Contains(out.String(), "[1/2] FROM alpine") {
		t.Errorf("vertex '[1/2] FROM alpine' not found in display output: %q", out.String())
	}
}

// Under --quiet, a valid trace frame produces no stdout yet renderBuildOutput
// still returns and joins the display goroutine cleanly (no hang/leak).
func TestRenderBuildOutput_QuietSuppressesOutput(t *testing.T) {
	sr := &controlapi.StatusResponse{
		Vertexes: []*controlapi.Vertex{
			{Digest: "sha256:abc", Name: "[1/2] FROM alpine", Started: timestamppb.Now()},
		},
	}
	msgs := []jsonstream.Message{realDaemonFrame(t, sr)}
	body := encodeMessages(t, msgs)

	var out bytes.Buffer
	if err := renderBuildOutput(context.Background(), body, &out, true); err != nil {
		t.Fatalf("renderBuildOutput (quiet) returned error: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("quiet trace output should be empty, got %q", out.String())
	}
}

// A trace frame followed by a build error must return the error and not hang
// (channel closed, goroutine joined). stdout is captured to confirm the trace
// frame was processed before decoding stopped.
func TestRenderBuildOutput_TraceFollowedByError(t *testing.T) {
	sr := &controlapi.StatusResponse{
		Vertexes: []*controlapi.Vertex{
			{Digest: "sha256:def", Name: "[1/2] FROM scratch", Started: timestamppb.Now()},
		},
	}
	msgs := []jsonstream.Message{
		realDaemonFrame(t, sr),
		{Error: &jsonstream.Error{Message: "layer pull failed"}},
	}
	body := encodeMessages(t, msgs)

	var out bytes.Buffer
	err := renderBuildOutput(context.Background(), body, &out, false)
	if err == nil {
		t.Fatal("expected error from error message after trace frame, got nil")
	}
	if !strings.Contains(err.Error(), "layer pull failed") {
		t.Errorf("error should contain 'layer pull failed', got %v", err)
	}
	if !strings.Contains(out.String(), "[1/2] FROM scratch") {
		t.Errorf("vertex '[1/2] FROM scratch' not found in display output: %q — trace frame was not processed", out.String())
	}
}

// Plain messages before the first trace frame reach stdout; those after are
// discarded to avoid a concurrent-write race with the progressui goroutine.
func TestRenderBuildOutput_TraceAndPlainMixed(t *testing.T) {
	msgs := []jsonstream.Message{
		{Stream: "pre-trace line\n"},
		realDaemonFrame(t, &controlapi.StatusResponse{}),
		{Stream: "post-trace line\n"},
	}
	body := encodeMessages(t, msgs)

	var out bytes.Buffer
	if err := renderBuildOutput(context.Background(), body, &out, false); err != nil {
		t.Fatalf("renderBuildOutput returned error: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "pre-trace line") {
		t.Errorf("output missing 'pre-trace line': %q", got)
	}
	// Post-trace plain lines are dropped once the display is active (race guard).
	if strings.Contains(got, "post-trace line") {
		t.Errorf("post-trace plain line must not appear on stdout (would race with progressui): %q", got)
	}
}

// With a pre-cancelled context the function must return promptly. The error is
// intentionally not asserted: when both the decode-loop send and ctx.Done() are
// ready, Go's select picks at random, so the result is nil or non-nil depending
// on the winner. The only guaranteed invariant is that it returns without blocking.
func TestRenderBuildOutput_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel so the select in the decode loop fires immediately

	msgs := []jsonstream.Message{realDaemonFrame(t, &controlapi.StatusResponse{})}
	body := encodeMessages(t, msgs)

	done := make(chan struct{})
	go func() {
		renderBuildOutput(ctx, body, io.Discard, false) //nolint:errcheck
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("renderBuildOutput blocked for >5s with pre-cancelled context")
	}
}

// The staged dir must contain only "Dockerfile" — the sibling credential file
// must not leak into the build context synced to the daemon.
func TestStageDockerfile_OnlyDockerfile(t *testing.T) {
	srcDir := t.TempDir()
	dockerfileContent := []byte("FROM scratch\nRUN echo hello\n")
	srcPath := filepath.Join(srcDir, "Dockerfile")
	if err := os.WriteFile(srcPath, dockerfileContent, 0o644); err != nil {
		t.Fatalf("write source Dockerfile: %v", err)
	}
	// Sibling that must NOT appear in the staged dir.
	if err := os.WriteFile(filepath.Join(srcDir, ".claude.json"), []byte(`{"secret":"s3kr3t"}`), 0o600); err != nil {
		t.Fatalf("write sibling credential file: %v", err)
	}

	stagedDir, cleanup, err := stageDockerfile(srcPath)
	if err != nil {
		t.Fatalf("stageDockerfile returned error: %v", err)
	}
	defer cleanup()

	if _, statErr := os.Stat(stagedDir); statErr != nil {
		t.Fatalf("staged dir does not exist: %v", statErr)
	}

	entries, err := os.ReadDir(stagedDir)
	if err != nil {
		t.Fatalf("ReadDir staged dir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("staged dir has %d entries %v, want exactly 1 (Dockerfile)", len(entries), names)
	}
	if entries[0].Name() != "Dockerfile" {
		t.Errorf("staged entry name = %q, want \"Dockerfile\"", entries[0].Name())
	}

	got, err := os.ReadFile(filepath.Join(stagedDir, "Dockerfile"))
	if err != nil {
		t.Fatalf("read staged Dockerfile: %v", err)
	}
	if !bytes.Equal(got, dockerfileContent) {
		t.Errorf("staged Dockerfile content differs: got %q, want %q", got, dockerfileContent)
	}
}

func TestStageDockerfile_CleanupRemovesDir(t *testing.T) {
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "Dockerfile")
	if err := os.WriteFile(srcPath, []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write source Dockerfile: %v", err)
	}

	stagedDir, cleanup, err := stageDockerfile(srcPath)
	if err != nil {
		t.Fatalf("stageDockerfile error: %v", err)
	}

	if _, statErr := os.Stat(stagedDir); statErr != nil {
		t.Fatalf("staged dir missing before cleanup: %v", statErr)
	}

	cleanup()

	if _, statErr := os.Stat(stagedDir); !os.IsNotExist(statErr) {
		t.Errorf("staged dir still exists after cleanup (stat error: %v)", statErr)
	}
}

// An unreadable source returns an error and must not leak a temp directory.
func TestStageDockerfile_UnreadableSource(t *testing.T) {
	_, cleanup, err := stageDockerfile("/nonexistent/path/to/Dockerfile")
	if err == nil {
		cleanup()
		t.Fatal("expected error for unreadable source, got nil")
	}
	if !strings.Contains(err.Error(), "read Dockerfile") {
		t.Errorf("error message %q should contain 'read Dockerfile'", err.Error())
	}
	cleanup() // must be safe to call even on the error path (no dir created)
}

// Invariant: stageDockerfile is wired into build() so the filesync source is the
// isolated staged dir, not the source's parent (which holds a sibling file).
// Verified indirectly: Build still reaches ImageBuild with basename "Dockerfile"
// (same fake-session pattern as TestBuild_FakeSelf_ImageBuildOptions).
func TestBuild_StagedDockerfileIsolation(t *testing.T) {
	srcDir := t.TempDir()
	dockerfilePath := filepath.Join(srcDir, "Dockerfile")
	if err := os.WriteFile(dockerfilePath, []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, ".claude.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write sibling: %v", err)
	}

	rc := &recordingBuildClient{}
	d := newDockerWithClient(t, rc)
	o := BuildOptions{
		Image:          "claudebox",
		DockerfilePath: dockerfilePath,
		ContextDir:     t.TempDir(),
	}
	_ = d.Build(context.Background(), o, io.Discard, io.Discard)

	if !rc.buildCalled {
		t.Fatal("ImageBuild was not called; stageDockerfile may have failed or the session did not reach ImageBuild")
	}
	if rc.lastOpts.Dockerfile != "Dockerfile" {
		t.Errorf("ImageBuild Dockerfile = %q, want \"Dockerfile\"", rc.lastOpts.Dockerfile)
	}
}
