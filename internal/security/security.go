// Package security scans for secret-bearing files under a project root so they
// can be overlaid with /dev/null mounts, preventing container access to host
// secrets. Scanning is config-driven: the caller supplies basename glob patterns
// (matched with filepath.Match) and directory names to prune during the walk.
// No in-code denylist or default patterns exist — if the caller passes empty
// patterns, nothing is scanned and the function returns nil immediately.
//
// Symlinks encountered during the walk are silently dropped; WalkDir does not
// follow them, so the walk stays within the tree rooted at root.
//
// Walk errors (e.g. unreadable subdirectory) are propagated immediately. This
// "fail-loud" behavior matches the no-.env-leak invariant: if we cannot prove a
// directory is secret-free, we must not proceed.
package security

import (
	"context"
	"io/fs"
	"path/filepath"
	"sort"
)

// Scan returns the absolute, sorted paths of every file under root whose
// basename matches at least one of the given glob patterns. Directories named
// in skipDirs are pruned from the walk (matched by bare name, not full path).
//
// When patterns is empty Scan returns nil immediately without walking root.
//
// Precondition: root must be absolute and filepath.EvalSymlinks-evaluated.
// Patterns must be valid filepath.Match patterns (validated by projectconfig.Load).
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

		// Match regular files by basename.
		name := d.Name()
		for _, pat := range patterns {
			matched, err := filepath.Match(pat, name)
			if err != nil {
				// Pattern was validated at Load time; this should not occur.
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
