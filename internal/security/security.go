package security

import (
	"context"
	"io/fs"
	"path/filepath"
	"sort"
)

// Scan returns sorted regular-file matches and symlink matches separately.
// WalkDir does not follow symlinks, so callers must warn that they remain
// unmasked. Empty patterns skip the walk. root must be absolute and resolved;
// patterns must be valid filepath.Match globs.
func Scan(ctx context.Context, root string, patterns, skipDirs []string) (paths, symlinkMatches []string, err error) {
	if len(patterns) == 0 {
		return nil, nil, nil
	}

	skip := make(map[string]struct{}, len(skipDirs))
	for _, d := range skipDirs {
		skip[d] = struct{}{}
	}

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, wErr error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if wErr != nil {
			return wErr
		}

		if d.IsDir() {
			if path != root {
				if _, pruned := skip[d.Name()]; pruned {
					return filepath.SkipDir
				}
			}
			return nil
		}

		isSymlink := d.Type()&fs.ModeSymlink != 0

		// Symlinks still need a warning when their names match.
		if !isSymlink && !d.Type().IsRegular() {
			return nil
		}

		name := d.Name()
		for _, pat := range patterns {
			matched, matchErr := filepath.Match(pat, name)
			if matchErr != nil {
				continue
			}
			if matched {
				if isSymlink {
					symlinkMatches = append(symlinkMatches, path)
				} else {
					paths = append(paths, path)
				}
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	sort.Strings(paths)
	sort.Strings(symlinkMatches)
	return paths, symlinkMatches, nil
}
