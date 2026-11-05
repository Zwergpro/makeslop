package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type cacheDir struct {
	Base  string
	Path  string
	Entry string
}

// checkCacheDir permits missing directories but never follows managed symlinks.
func checkCacheDir(dir cacheDir) error {
	_, err := cacheDirComponents(dir)
	return err
}

func cacheDirComponents(dir cacheDir) ([]string, error) {
	rel, err := checkedCacheDirRelativePath(dir)
	if err != nil {
		return nil, err
	}

	components := []string{}
	current := dir.Base
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		components = append(components, current)

		// Stat would follow links, including links escaping the managed base.
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf(
				"%s: check %q: %w",
				dir.Entry,
				current,
				err,
			)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%s: %q must be a real directory, not a file or symlink", dir.Entry, current)
		}
	}

	return components, nil
}

func checkedCacheDirRelativePath(dir cacheDir) (string, error) {
	rel, err := filepath.Rel(dir.Base, dir.Path)
	absolutePaths := filepath.IsAbs(dir.Base) && filepath.IsAbs(dir.Path)
	localChild := err == nil && rel != "." && filepath.IsLocal(rel)
	if !absolutePaths || !localChild {
		return "", fmt.Errorf(
			"%s: managed path %q must be beneath %q",
			dir.Entry,
			dir.Path,
			dir.Base,
		)
	}

	info, err := os.Lstat(dir.Base)
	if err != nil {
		return "", fmt.Errorf(
			"%s: check base %q: %w",
			dir.Entry,
			dir.Base,
			err,
		)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s: base %q must be a real directory", dir.Entry, dir.Base)
	}

	return rel, nil
}

// Preparation is idempotent; an operational failure may leave earlier empty dirs.
func prepareCacheDirs(ctx context.Context, dirs []cacheDir) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// Validate every declaration first so a later invalid source causes no writes.
	for _, dir := range dirs {
		if err := checkCacheDir(dir); err != nil {
			return err
		}
	}
	for _, dir := range dirs {
		if err := prepareCacheDir(ctx, dir); err != nil {
			return err
		}
	}

	return nil
}

func prepareCacheDir(ctx context.Context, dir cacheDir) error {
	components, err := cacheDirComponents(dir)
	if err != nil {
		return err
	}

	for _, component := range components {
		if err := ctx.Err(); err != nil {
			return err
		}

		// Recheck ancestors before and after creation; EEXIST may also be a symlink.
		current := cacheDir{Base: dir.Base, Path: component, Entry: dir.Entry}
		if err := checkCacheDir(current); err != nil {
			return err
		}
		if err := os.Mkdir(component, 0o755); err != nil && !os.IsExist(err) {
			return fmt.Errorf(
				"%s: create %q: %w",
				dir.Entry,
				component,
				err,
			)
		}
		if err := checkCacheDir(current); err != nil {
			return err
		}
	}

	return nil
}
