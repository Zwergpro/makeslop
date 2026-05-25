package security

// Test-only helpers, compiled into the production binary so package-main tests
// can access them as exported symbols.

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

// WriteFdShim writes a POSIX shim at <dir>/fd-shim that emits paths
// null-separated and records its argv to argv.txt. Returns shim and record paths.
func WriteFdShim(t *testing.T, dir string, paths []string) (shimPath, recordPath string) {
	t.Helper()
	shimPath = filepath.Join(dir, "fd-shim")
	recordPath = filepath.Join(dir, "fd-argv.txt")

	var printStmts strings.Builder
	for _, p := range paths {
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
