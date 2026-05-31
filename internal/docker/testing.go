package docker

// Test-only helpers, compiled into the production binary so package-main tests
// can access them as exported symbols (export_test.go cannot satisfy this because
// main_test.go is in package main, not package docker_test).

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/moby/moby/api/types/container"
	moby "github.com/moby/moby/client"
	"golang.org/x/term"
)

// SetDockerBinaryForTest swaps the path Run will exec, returning a restore
// function that callers MUST register with t.Cleanup. Concurrent tests that
// touch this swap point must serialize themselves (the package state is
// process-global).
func SetDockerBinaryForTest(path string) (restore func()) {
	prev := dockerBinary
	dockerBinary = path
	return func() { dockerBinary = prev }
}

// SetTTYCheckForTest swaps the TTY-detection predicate Run consults. Same
// caveats as SetDockerBinaryForTest apply.
func SetTTYCheckForTest(fn func() bool) (restore func()) {
	prev := ttyCheck
	ttyCheck = fn
	return func() { ttyCheck = prev }
}

// SetClientForTest swaps the client factory that Run uses, replacing it with
// one that always returns c. Returns a restore function that callers MUST
// register with t.Cleanup. Concurrent tests that touch this swap point must
// serialize themselves (the package state is process-global).
func SetClientForTest(c apiClient) (restore func()) {
	prev := newClientFn
	newClientFn = func() (apiClient, error) { return c, nil }
	return func() { newClientFn = prev }
}

// SetTermMakeRawForTest swaps the terminal raw-mode function that run() calls.
// Use a no-op stub in tests that have no real PTY:
//
//	t.Cleanup(docker.SetTermMakeRawForTest(func(fd int) (*term.State, error) {
//	    return nil, nil
//	}))
//
// IMPORTANT: when the stub returns nil state, defer term.Restore(fd, nil) is
// called — that is a no-op, so restoring is safe.
func SetTermMakeRawForTest(fn func(fd int) (*term.State, error)) (restore func()) {
	prev := termMakeRaw
	termMakeRaw = fn
	return func() { termMakeRaw = prev }
}

// SkipNonPOSIX skips on non-POSIX hosts per the CLAUDE.md invariant.
func SkipNonPOSIX(t *testing.T, why string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip(why)
	}
}

// WriteShim drops a POSIX shell script at <dir>/shim that records its argv
// (one arg per line) to a sibling argv.txt and exits with exitCode. Returns
// the shim path and the argv record path.
func WriteShim(t *testing.T, dir string, exitCode int) (shimPath, recordPath string) {
	t.Helper()
	shimPath = filepath.Join(dir, "shim")
	recordPath = filepath.Join(dir, "argv.txt")
	script := "#!/bin/sh\n" +
		"for arg in \"$@\"; do printf '%s\\n' \"$arg\" >> \"" + recordPath + "\"; done\n" +
		"exit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(shimPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	return shimPath, recordPath
}

// FakeRunClient is an exported fake apiClient for use in cmd/makeslop tests.
// It instruments Run calls with a scripted exit code and records whether the
// container was started. TTY check must still be stubbed by the caller via
// SetTTYCheckForTest.
//
// Callers register the fake with:
//
//	t.Cleanup(docker.SetClientForTest(docker.NewFakeRunClient(exitCode)))
type FakeRunClient struct {
	ExitCode int
	Started  bool
}

// NewFakeRunClient creates a FakeRunClient that will return the given exit code
// from ContainerWait.
func NewFakeRunClient(exitCode int) *FakeRunClient {
	return &FakeRunClient{ExitCode: exitCode}
}

func (f *FakeRunClient) ContainerCreate(_ context.Context, _ moby.ContainerCreateOptions) (moby.ContainerCreateResult, error) {
	return moby.ContainerCreateResult{ID: "fake-id"}, nil
}

func (f *FakeRunClient) ContainerAttach(_ context.Context, _ string, _ moby.ContainerAttachOptions) (moby.ContainerAttachResult, error) {
	pr, pw := net.Pipe()
	go func() { _ = pw.Close() }()
	hr := moby.NewHijackedResponse(pr, "")
	return moby.ContainerAttachResult{HijackedResponse: hr}, nil
}

func (f *FakeRunClient) ContainerStart(_ context.Context, _ string, _ moby.ContainerStartOptions) (moby.ContainerStartResult, error) {
	f.Started = true
	return moby.ContainerStartResult{}, nil
}

func (f *FakeRunClient) ContainerWait(_ context.Context, _ string, _ moby.ContainerWaitOptions) moby.ContainerWaitResult {
	resultC := make(chan container.WaitResponse, 1)
	errC := make(chan error, 1)
	resultC <- container.WaitResponse{StatusCode: int64(f.ExitCode)}
	return moby.ContainerWaitResult{Result: resultC, Error: errC}
}

func (f *FakeRunClient) ContainerResize(_ context.Context, _ string, _ moby.ContainerResizeOptions) (moby.ContainerResizeResult, error) {
	return moby.ContainerResizeResult{}, nil
}

func (f *FakeRunClient) ContainerRemove(_ context.Context, _ string, _ moby.ContainerRemoveOptions) (moby.ContainerRemoveResult, error) {
	return moby.ContainerRemoveResult{}, nil
}

func (f *FakeRunClient) ImageBuild(_ context.Context, _ io.Reader, _ moby.ImageBuildOptions) (moby.ImageBuildResult, error) {
	return moby.ImageBuildResult{Body: io.NopCloser(bytes.NewReader(nil))}, nil
}

func (f *FakeRunClient) DialHijack(_ context.Context, _, _ string, _ map[string][]string) (net.Conn, error) {
	return nil, nil
}

func (f *FakeRunClient) Close() error { return nil }

// WriteBuildShim drops a POSIX shell script at <dir>/shim that records its
// argv (one arg per line) to a sibling argv.txt AND records the value of
// DOCKER_BUILDKIT to a sibling env.txt, then exits with exitCode.
// Returns the shim path, argv record path, and env record path.
func WriteBuildShim(t *testing.T, dir string, exitCode int) (shimPath, recordPath, envPath string) {
	t.Helper()
	shimPath = filepath.Join(dir, "shim")
	recordPath = filepath.Join(dir, "argv.txt")
	envPath = filepath.Join(dir, "env.txt")
	script := "#!/bin/sh\n" +
		"for arg in \"$@\"; do printf '%s\\n' \"$arg\" >> \"" + recordPath + "\"; done\n" +
		"printf '%s\\n' \"$DOCKER_BUILDKIT\" >> \"" + envPath + "\"\n" +
		"exit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(shimPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write build shim: %v", err)
	}
	return shimPath, recordPath, envPath
}
