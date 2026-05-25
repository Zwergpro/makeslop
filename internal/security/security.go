// Package security scans for .env files under a project root so they can be
// overlaid with /dev/null mounts, preventing container access to host secrets.
package security

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// fdBinary is a test-only swap point for the fd binary; empty means discover from PATH.
var fdBinary = ""

// ErrFdMissing is returned when neither fd nor fdfind is on PATH.
var ErrFdMissing = errors.New("security: fd CLI not found on PATH")

// Scan returns the absolute, sorted paths of every .env file under root.
// Precondition: root must be absolute and EvalSymlinks-evaluated; any direct
// caller must enforce this.
// Returned paths are guaranteed to be under root (trust boundary for the fd
// process; paths outside root are silently dropped).
// Returns ErrFdMissing when fd/fdfind is not on PATH; other exec errors are
// wrapped with "security: run fd: ".
func Scan(ctx context.Context, root string) ([]string, error) {
	var bin string
	if fdBinary != "" {
		resolved, err := exec.LookPath(fdBinary)
		if err != nil {
			return nil, ErrFdMissing
		}
		bin = resolved
	} else {
		for _, name := range []string{"fd", "fdfind"} {
			if resolved, err := exec.LookPath(name); err == nil {
				bin = resolved
				break
			}
		}
		if bin == "" {
			return nil, ErrFdMissing
		}
	}

	argv := []string{
		"--hidden",
		"--no-ignore",
		"--type", "f",
		"--exclude", ".git",
		"--exclude", "node_modules",
		"--exclude", "vendor",
		"--exclude", ".venv",
		"--print0",
		"--regex", `\.env$`,
		"--",
		root,
	}

	cmd := exec.CommandContext(ctx, bin, argv...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("security: run fd: %w", err)
	}

	if len(out) == 0 {
		return nil, nil
	}

	// fd's NUL-separated output ends with a trailing NUL; drop the empty token it produces.
	raw := strings.Split(string(out), "\x00")
	if len(raw) > 0 && raw[len(raw)-1] == "" {
		raw = raw[:len(raw)-1]
	}

	var paths []string
	for _, p := range raw {
		if p == "" {
			continue
		}
		// Trust boundary: drop paths outside root. filepath.Rel never errors on
		// POSIX; IsLocal catches ..-escapes.
		rel, err := filepath.Rel(root, p)
		if err != nil || !filepath.IsLocal(rel) {
			continue
		}
		paths = append(paths, p)
	}

	sort.Strings(paths)
	return paths, nil
}
