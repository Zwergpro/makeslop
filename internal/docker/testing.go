package docker

// Test-only helpers, compiled into the production binary so package-main tests
// can access them as exported symbols (export_test.go cannot satisfy this because
// main_test.go is in package main, not package docker_test).

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
