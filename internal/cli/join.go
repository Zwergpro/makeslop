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

// errNotProject reports a --join directory without a .makeslop.yaml.
var errNotProject = errors.New("not a makeslop project (no " + projectconfig.Filename + ")")

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
// mainRoot, baseDir and earlier joins are compared both lexically and by
// inode (each root's ancestors vs the other root), which catches an alias of
// a root or of one of its ancestors (case-insensitive filesystems, bind
// mounts). An alias of a subdirectory reached from outside is not detected.
func resolveJoins(pwd, mainRoot, mainName, baseDir string, raw []string, outOfHome bool) ([]joinTarget, error) {
	joins := make([]joinTarget, 0, len(raw))
	for _, r := range raw {
		j, err := resolveJoin(pwd, r, outOfHome)
		if err != nil {
			return nil, err
		}

		in, out := containsDir(mainRoot, j.Host), containsDir(j.Host, mainRoot)
		switch {
		case in && out:
			return nil, fmt.Errorf("--join %q: is the current project", r)
		case in:
			return nil, fmt.Errorf("--join %q: is inside the current project", r)
		case out:
			return nil, fmt.Errorf("--join %q: contains the current project", r)
		case overlaps(j.Host, baseDir):
			return nil, fmt.Errorf("--join %q: overlaps the makeslop data dir %s", r, baseDir)
		}
		// One pass over earlier joins; any overlap wins over a name collision.
		var clash *joinTarget
		for i, prev := range joins {
			if overlaps(j.Host, prev.Host) {
				return nil, fmt.Errorf("--join %q: overlaps --join %q", r, prev.Raw)
			}
			if clash == nil && j.Name == prev.Name {
				clash = &joins[i]
			}
		}
		switch {
		case j.Name == mainName:
			return nil, fmt.Errorf("--join %q: mount name %q collides with the current project", r, j.Name)
		case clash != nil:
			return nil, fmt.Errorf("--join %q: mount name %q collides with --join %q", r, j.Name, clash.Raw)
		}
		joins = append(joins, j)
	}
	return joins, nil
}

// resolveJoin handles a single value: suffix, path resolution, filesystem
// checks, mount name and the home guard. A relative path resolves against pwd,
// the physical (symlink-resolved) working directory, not the shell's $PWD.
// The .makeslop.yaml checks here only fail fast before the daemon preflight;
// loadProject re-checks existence when it parses the config.
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
		return joinTarget{}, fmt.Errorf("--join %q: %w", raw, errNotProject)
	case err != nil:
		return joinTarget{}, fmt.Errorf("--join %q: %w", raw, err)
	case cfgInfo.Mode()&fs.ModeSymlink != 0:
		return joinTarget{}, fmt.Errorf("--join %q: %w", raw, projectconfig.ErrConfigSymlink)
	case !cfgInfo.Mode().IsRegular():
		return joinTarget{}, fmt.Errorf("--join %q: %s is not a regular file", raw, projectconfig.Filename)
	}

	if !outOfHome {
		ok, home, err := isWithinHome(host)
		if err != nil {
			return joinTarget{}, fmt.Errorf("--join %q: %w", raw, err)
		}
		if !ok {
			return joinTarget{}, fmt.Errorf("--join %q: outside %s — pass --out-of-home to override", raw, home)
		}
	}

	return joinTarget{Host: host, Name: name, ReadOnly: readOnly, Raw: raw}, nil
}

// label is the dry-run section separator text for j.
func (j joinTarget) label() string {
	mode := "rw"
	if j.ReadOnly {
		mode = "ro"
	}
	return "join: " + j.Host + " (" + mode + ")"
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
