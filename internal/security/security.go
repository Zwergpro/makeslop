// Package security scans for secret-bearing files under a project root so they
// can be overlaid with /dev/null mounts, preventing container access to host
// secrets. Scanning is config-driven: the caller supplies basename glob patterns
// and directory names to prune.
//
// Walk errors are propagated immediately ("fail-loud"): if we cannot prove a
// directory is secret-free, we must not proceed (no-.env-leak invariant).
package security

import (
	"context"
	"io/fs"
	"path/filepath"
	"sort"
)

// Scan returns the absolute, sorted paths of every file under root whose
// basename matches a glob pattern; directories named in skipDirs (bare name)
// are pruned. Empty patterns returns nil without walking.
//
// Precondition: root absolute and EvalSymlinks-evaluated; patterns valid
// filepath.Match patterns (validated by projectconfig.Load).
func Scan(ctx context.Context, root string, patterns, skipDirs []string) ([]string, error) {
	if len(patterns) == 0 {
		return nil, nil
	}

	skip := make(map[string]struct{}, len(skipDirs))
	for _, d := range skipDirs {
		skip[d] = struct{}{}
	}

	var paths []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}

		if d.IsDir() {
			if path != root {
				if _, pruned := skip[d.Name()]; pruned {
					return filepath.SkipDir
				}
			}
			return nil
		}

		// Drop symlinks; WalkDir does not follow them.
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}

		// Skip sockets, pipes, device nodes, etc.
		if !d.Type().IsRegular() {
			return nil
		}

		name := d.Name()
		for _, pat := range patterns {
			matched, matchErr := filepath.Match(pat, name)
			if matchErr != nil {
				continue
			}
			if matched {
				paths = append(paths, path)
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Strings(paths)
	return paths, nil
}
