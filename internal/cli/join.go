package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Zwergpro/makeslop/internal/projectconfig"
)

// joinTarget is one validated --join value.
type joinTarget struct {
	Host     string // abs, EvalSymlinks'd
	Name     string // filepath.Base(Host); mounted at /workspace/<Name>
	ReadOnly bool
	Raw      string // as typed (for messages)
}

// resolveJoins validates and normalizes --join values. It checks paths only:
// the join's .makeslop.yaml must exist as a regular file but is not parsed
// here, so the daemon preflight still runs before any config error surfaces.
// mainRoot and baseDir are compared both lexically and by inode, so alias
// paths (case-insensitive filesystems, bind mounts) cannot slip past.
func resolveJoins(pwd, mainRoot, mainName, baseDir string, raw []string, outOfHome bool) ([]joinTarget, error) {
	joins := make([]joinTarget, 0, len(raw))
	for _, r := range raw {
		j, err := resolveJoin(pwd, r, outOfHome)
		if err != nil {
			return nil, err
		}

		switch {
		case sameDir(j.Host, mainRoot):
			return nil, fmt.Errorf("--join %q: is the current project", r)
		case containsDir(mainRoot, j.Host):
			return nil, fmt.Errorf("--join %q: is inside the current project", r)
		case containsDir(j.Host, mainRoot):
			return nil, fmt.Errorf("--join %q: contains the current project", r)
		case overlaps(j.Host, baseDir):
			return nil, fmt.Errorf("--join %q: overlaps the makeslop data dir %s", r, baseDir)
		}
		for _, prev := range joins {
			if overlaps(j.Host, prev.Host) {
				return nil, fmt.Errorf("--join %q: overlaps --join %q", r, prev.Raw)
			}
		}
		if j.Name == mainName {
			return nil, fmt.Errorf("--join %q: mount name %q collides with the current project", r, j.Name)
		}
		for _, prev := range joins {
			if j.Name == prev.Name {
				return nil, fmt.Errorf("--join %q: mount name %q collides with --join %q", r, j.Name, prev.Raw)
			}
		}
		joins = append(joins, j)
	}
	return joins, nil
}

// resolveJoin handles a single value: suffix, path resolution, filesystem
// checks, mount name and the home guard.
func resolveJoin(pwd, raw string, outOfHome bool) (joinTarget, error) {
	p, readOnly := parseJoinSuffix(raw)
	if !filepath.IsAbs(p) {
		p = filepath.Join(pwd, p)
	}
	host, err := filepath.EvalSymlinks(p)
	if err != nil {
		return joinTarget{}, fmt.Errorf("--join %q: %w", raw, err)
	}
	info, err := os.Stat(host)
	if err != nil {
		return joinTarget{}, fmt.Errorf("--join %q: %w", raw, err)
	}
	if !info.IsDir() {
		return joinTarget{}, fmt.Errorf("--join %q: not a directory", raw)
	}

	name := filepath.Base(host)
	switch name {
	case "", ".", "..", string(filepath.Separator):
		return joinTarget{}, fmt.Errorf("--join %q: cannot derive a mount name from %s", raw, host)
	}

	cfgInfo, err := os.Lstat(filepath.Join(host, projectconfig.Filename))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return joinTarget{}, fmt.Errorf("--join %q: not a makeslop project (no %s)", raw, projectconfig.Filename)
	case err != nil:
		return joinTarget{}, fmt.Errorf("--join %q: %w", raw, err)
	case cfgInfo.Mode()&fs.ModeSymlink != 0:
		return joinTarget{}, fmt.Errorf("--join %q: %s is a symlink — the project config must be a regular file", raw, projectconfig.Filename)
	case !cfgInfo.Mode().IsRegular():
		return joinTarget{}, fmt.Errorf("--join %q: %s is not a regular file", raw, projectconfig.Filename)
	}

	if !outOfHome {
		ok, home, err := isWithinHome(host)
		if err != nil {
			return joinTarget{}, err
		}
		if !ok {
			return joinTarget{}, fmt.Errorf("--join %q: outside %s — pass --out-of-home to override", raw, home)
		}
	}

	return joinTarget{Host: host, Name: name, ReadOnly: readOnly, Raw: raw}, nil
}

// parseJoinSuffix strips a trailing ":ro" or ":rw". Any other value is taken
// whole as a path (rw), so "foo:bar" is a path and "foo:ro:rw" joins "foo:ro".
func parseJoinSuffix(raw string) (path string, readOnly bool) {
	i := strings.LastIndex(raw, ":")
	if i < 0 {
		return raw, false
	}
	switch raw[i+1:] {
	case "ro":
		return raw[:i], true
	case "rw":
		return raw[:i], false
	}
	return raw, false
}

// overlaps reports whether a and b are the same directory or one contains the other.
func overlaps(a, b string) bool {
	return containsDir(a, b) || containsDir(b, a)
}

// sameDir reports whether a and b name the same directory, lexically or by inode.
func sameDir(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ai, aErr := os.Stat(a)
	bi, bErr := os.Stat(b)
	return aErr == nil && bErr == nil && os.SameFile(ai, bi)
}

// containsDir reports whether child is parent or lies below it. The lexical
// check uses filepath.Rel + IsLocal (as guard.go does); the inode check walks
// child's ancestors and compares each with parent via os.SameFile.
func containsDir(parent, child string) bool {
	if rel, err := filepath.Rel(parent, child); err == nil && filepath.IsLocal(rel) {
		return true
	}
	return hasSameFileAncestor(parent, child)
}

// hasSameFileAncestor reports whether child or any of its ancestors is the
// same file as parent. Unstattable paths never match.
func hasSameFileAncestor(parent, child string) bool {
	pi, err := os.Stat(parent)
	if err != nil {
		return false
	}
	for d := filepath.Clean(child); ; d = filepath.Dir(d) {
		if di, err := os.Stat(d); err == nil && os.SameFile(pi, di) {
			return true
		}
		if filepath.Dir(d) == d {
			return false
		}
	}
}
