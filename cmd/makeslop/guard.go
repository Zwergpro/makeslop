package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func resolvePwd() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", fmt.Errorf("evaluate symlinks for %s: %w", cwd, err)
	}
	return resolved, nil
}

// ensureWithinHome returns errSilent when pwd is outside the user's home and
// outOfHome is false. pwd must be EvalSymlinks-resolved; $HOME is resolved here
// for a symlink-symmetric comparison.
func ensureWithinHome(stderr io.Writer, pwd string, outOfHome bool) error {
	if outOfHome {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	resolvedHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return fmt.Errorf("evaluate symlinks for %s: %w", home, err)
	}
	rel, err := filepath.Rel(resolvedHome, pwd)
	if err != nil {
		return fmt.Errorf("compute relative path from %s to %s: %w", resolvedHome, pwd, err)
	}
	if !filepath.IsLocal(rel) {
		fmt.Fprintf(stderr,
			"makeslop: refusing to run from %s (outside %s) — pass --out-of-home to override\n",
			pwd, resolvedHome)
		return errSilent
	}
	return nil
}

// quietWriter discards writes when quiet is true; used to gate stderr chrome
// (notices, nudges, progress) while real errors go to the underlying writer.
type quietWriter struct {
	w     io.Writer
	quiet bool
}

func (q *quietWriter) Write(p []byte) (int, error) {
	if q.quiet {
		return len(p), nil
	}
	return q.w.Write(p)
}

// errSilent signals that a RunE already wrote a tailored message to stderr;
// main() should exit non-zero without reprinting.
var errSilent = errors.New("makeslop: silent error already reported")
