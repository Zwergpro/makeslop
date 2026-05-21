package security

// Test-only helpers for the security package.
//
// These functions are intentionally compiled into the production binary so
// that cmd/makeslop/main_test.go (package main) can access them as exported
// symbols. This matches the same pattern used in internal/docker/testing.go.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// SetFdBinaryForTest swaps the fd binary path that Scan will exec, returning a
// restore function that callers MUST register with t.Cleanup. Concurrent tests
// that touch this swap point must serialize themselves (the package state is
// process-global).
func SetFdBinaryForTest(path string) (restore func()) {
	prev := fdBinary
	fdBinary = path
	return func() { fdBinary = prev }
}

// WriteFdShim drops a POSIX shell script at <dir>/fd-shim that prints the
// given paths null-separated on stdout and records its argv (one arg per line)
// to a sibling argv.txt. It returns the shim path and the argv record path.
//
// paths is the list of absolute paths the shim will emit. The caller is
// responsible for ensuring the paths make sense for the test scenario.
func WriteFdShim(t *testing.T, dir string, paths []string) (shimPath, recordPath string) {
	t.Helper()
	shimPath = filepath.Join(dir, "fd-shim")
	recordPath = filepath.Join(dir, "fd-argv.txt")

	// Build the printf calls for each path, null-separated.
	var printStmts strings.Builder
	for _, p := range paths {
		// Use printf '%s\0' to emit each path followed by a NUL byte.
		printStmts.WriteString("printf '%s\\0' '")
		printStmts.WriteString(p) // paths are controlled by tests; no shell injection risk
		printStmts.WriteString("'\n")
	}

	script := "#!/bin/sh\n" +
		// Record argv.
		"for arg in \"$@\"; do printf '%s\\n' \"$arg\" >> \"" + recordPath + "\"; done\n" +
		// Emit the paths null-separated.
		printStmts.String() +
		"exit 0\n"

	if err := os.WriteFile(shimPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fd shim: %v", err)
	}
	return shimPath, recordPath
}
