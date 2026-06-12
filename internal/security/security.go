package security

import (
	"context"
	"io/fs"
	"path/filepath"
	"sort"
)

// Scan returns two sorted slices and an error:
//   - paths: absolute paths of regular files whose basename matches a pattern.
//   - symlinkMatches: absolute paths of symlinks whose basename matches a pattern
//     (these are NOT masked — WalkDir does not follow symlinks — callers should
//     warn the user that protection is incomplete).
//
// Directories named in skipDirs (bare name) are pruned. Empty patterns returns
// (nil, nil, nil) without walking.
//
// Precondition: root absolute and EvalSymlinks-evaluated; patterns valid
// filepath.Match patterns (validated by projectconfig.Load).
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

		// Skip sockets, pipes, device nodes, etc. (but not symlinks — we want to
		// check their names against patterns before dropping them).
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
