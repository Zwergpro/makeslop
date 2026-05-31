package security

// Test-only helpers, compiled into the production binary so package-main tests
// can access them as exported symbols.
//
// Deprecated: fd is no longer used. SetFdBinaryForTest and WriteFdShim are
// retained only for the transition period while tests are updated (Task 4).
// They will be deleted in Task 4.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// SetFdBinaryForTest is a no-op stub kept for compile compatibility while
// security_test.go and main_test.go are migrated in Task 4.
// Deprecated: will be removed in Task 4.
func SetFdBinaryForTest(_ string) func() {
	return func() {}
}

// WriteFdShim writes a POSIX shim at <dir>/fd-shim that emits paths
// null-separated and records its argv to argv.txt. Returns shim and record paths.
// Deprecated: fd is no longer used; this helper is retained only for the
// transition period while security_test.go is updated (Task 4).
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
