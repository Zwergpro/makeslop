package docker

// Test-only helpers for the docker package.
//
// These functions are intentionally compiled into the production binary.
// The idiomatic Go approach (export_test.go) cannot be used here because
// cmd/makeslop/main_test.go is in package main (not package docker_test), so
// it can only access exported symbols from the docker package — not
// _test.go-only exports. This is a known, deliberate trade-off: the test
// surface (SetDockerBinaryForTest, SetTTYCheckForTest, WriteShim, SkipNonPOSIX)
// is small, pure-Go, and carries no runtime cost when unused.

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
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

// SkipNonPOSIX skips the calling test on non-POSIX hosts per the CLAUDE.md
// invariant. why becomes the skip reason so failure logs explain the gate.
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
