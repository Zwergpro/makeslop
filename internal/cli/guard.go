package cli

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

// isWithinHome reports whether path is $HOME or below it. $HOME is
// EvalSymlinks-resolved; callers pass an already-resolved path so the
// comparison is symmetric. The resolved home is returned for messages.
func isWithinHome(path string) (ok bool, home string, err error) {
	rawHome, err := os.UserHomeDir()
	if err != nil {
		return false, "", fmt.Errorf("resolve home directory: %w", err)
	}
	home, err = filepath.EvalSymlinks(rawHome)
	if err != nil {
		return false, "", fmt.Errorf("evaluate symlinks for %s: %w", rawHome, err)
	}
	rel, err := filepath.Rel(home, path)
	if err != nil {
		return false, "", fmt.Errorf("compute relative path from %s to %s: %w", home, path, err)
	}
	return filepath.IsLocal(rel), home, nil
}

// ensureWithinHome returns errSilent when pwd is outside home and outOfHome is
// false. Both pwd and $HOME are EvalSymlinks-resolved for a symmetric comparison.
func ensureWithinHome(stderr io.Writer, pwd string, outOfHome bool) error {
	if outOfHome {
		return nil
	}
	ok, home, err := isWithinHome(pwd)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintf(stderr,
			"makeslop: refusing to run from %s (outside %s) — pass --out-of-home to override\n",
			pwd, home)
		return errSilent
	}
	return nil
}

// Errors bypass quietWriter so --quiet cannot hide failures.
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

// errSilent signals that RunE already printed a tailored message; exit non-zero
// without reprinting.
var errSilent = errors.New("makeslop: silent error already reported")
