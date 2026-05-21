// Package security provides secret-file scanning for the makeslop container
// launcher. It locates .env files under a project root so the docker layer can
// overlay them with /dev/null mounts, preventing the container from reading
// host secrets.
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

// fdBinary is the package-level swap point for tests (see testing.go). It is
// resolved via exec.LookPath at call time, so tests may point it at a shim
// shell script or a nonexistent path to exercise error paths.
//
// When empty (the default), Scan discovers "fd" or "fdfind" from PATH.
// When set by a test shim, Scan uses this path directly.
var fdBinary = ""

// ErrFdMissing is returned when neither "fd" nor "fdfind" is on PATH. The
// cobra layer translates this sentinel into a user-facing install hint and
// returns errSilent so main() skips the reprint.
var ErrFdMissing = errors.New("security: fd CLI not found on PATH")

// Scan returns the absolute, lexicographically sorted paths of every regular
// file whose basename ends in ".env" found under root.
//
// Precondition: root must be an absolute path that has been resolved through
// filepath.EvalSymlinks (the same precondition as workspace.Workspaces.Lookup
// and docker.BuildSpec). The cobra layer enforces this via resolvePwd; any
// direct caller must do the same.
//
// The returned paths are guaranteed to be under root (Scan is the trust
// boundary for the external process; any path outside root is silently
// dropped). Consumers (e.g. docker.BuildSpec) may rely on this guarantee and
// do not need to re-validate.
//
// If neither "fd" nor "fdfind" is on PATH, Scan returns ErrFdMissing.
// Other exec errors are wrapped with a "security: run fd: " prefix.
func Scan(ctx context.Context, root string) ([]string, error) {
	// Resolve the binary to use. When fdBinary is set (e.g. by a test shim),
	// use it directly. Otherwise try "fd" then "fdfind".
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

	// Split on NUL, drop the trailing empty token that follows the last NUL.
	raw := strings.Split(string(out), "\x00")
	if len(raw) > 0 && raw[len(raw)-1] == "" {
		raw = raw[:len(raw)-1]
	}

	var paths []string
	for _, p := range raw {
		if p == "" {
			continue
		}
		// Trust boundary: verify every path returned by the external process is
		// under root. filepath.Rel returns an error only when the two paths are
		// on different Windows volumes; on POSIX (this project's only target)
		// the call always succeeds. Use filepath.IsLocal to detect ".."-escapes.
		rel, err := filepath.Rel(root, p)
		if err != nil || !filepath.IsLocal(rel) {
			continue
		}
		paths = append(paths, p)
	}

	sort.Strings(paths)
	return paths, nil
}
