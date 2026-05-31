// Package projectconfig parses and scaffolds the project-local .makeslop.yaml.
// All returned paths are absolute and guaranteed to be under the project root
// (trust boundary for user-supplied YAML).
//
// root parameters must be absolute and EvalSymlinks-evaluated; any direct
// caller must enforce this.
//
// The config has two distinct exclude mechanisms:
//   - exclude.scan: drives the WalkDir secret scan (config-driven glob walk, no
//     in-code denylist). Patterns are basename globs; skip-dirs are bare directory
//     names pruned during the walk. These are names, not paths — no IsLocal/path
//     checks apply.
//   - exclude.files / exclude.dirs: explicit host paths for /dev/null and tmpfs
//     overlays. Load returns only paths that are under root. Symlinks are silently
//     dropped (masking a symlink has ambiguous semantics: the symlink itself or its
//     target?). Reserved agent paths (.claude, .codex, docs, CLAUDE.md) are a hard
//     error because a user overlay would shadow the agent mount.
package projectconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Filename is the project-local config file name, relative to the project root.
const Filename = ".makeslop.yaml"

// Stub is the content Scaffold writes. Exported so tests can compare without hardcoding.
// It seeds the default scan filters (patterns + skip-dirs) as active values so that
// new projects get secret masking out of the box without requiring manual configuration.
var Stub = []byte(`exclude:
  scan:
    patterns:
      - "*.env"
      - ".env.*"
      - "*.pem"
      - "*.key"
      - "id_rsa*"
      - "id_ed25519*"
      - ".npmrc"
      - ".netrc"
      - ".git-credentials"
    skip-dirs:
      - .git
      - node_modules
      - vendor
      - .venv
  files: []
  dirs: []
network:
  proxy:
    address: ""
`)

// Already mounted by docker.BuildSpec; user overlays would silently shadow them.
var reservedPaths = map[string]struct{}{
	".claude":   {},
	".codex":    {},
	"docs":      {},
	"CLAUDE.md": {},
}

// Excludes is the parsed, validated, host-resolved result of Load.
// Paths are absolute, under root, deduplicated within each list, and sorted.
type Excludes struct {
	Files    []string // absolute host paths; /dev/null overlay targets
	Dirs     []string // absolute host paths; tmpfs overlay targets
	Patterns []string // basename globs for the WalkDir secret scan; deduped and sorted
	SkipDirs []string // bare directory names pruned during the WalkDir scan; deduped and sorted
}

// Network is the parsed network configuration. Empty ProxyAddress means no
// proxy configured. Load validates only syntax (net.SplitHostPort) — no I/O.
type Network struct {
	ProxyAddress string // "host:port"; "" when unconfigured
}

// yamlSchema is the strict decode target. Using KnownFields(true) means any
// top-level key other than "exclude" / "network" (and their sub-keys) is
// rejected immediately with a meaningful decoder error.
type yamlSchema struct {
	Exclude struct {
		Scan struct {
			Patterns []string `yaml:"patterns"`
			SkipDirs []string `yaml:"skip-dirs"`
		} `yaml:"scan"`
		Dirs  []string `yaml:"dirs"`
		Files []string `yaml:"files"`
	} `yaml:"exclude"`
	Network struct {
		Proxy struct {
			Address string `yaml:"address"`
		} `yaml:"proxy"`
	} `yaml:"network"`
}

// Scaffold creates <root>/.makeslop.yaml with an empty stub. Idempotent:
// EEXIST is success, user edits are never clobbered. root must be absolute
// and EvalSymlinks-evaluated.
func Scaffold(root string) error {
	path := filepath.Join(root, Filename)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		return fmt.Errorf("scaffold %s: %w", Filename, err)
	}
	defer f.Close()
	if _, err := f.Write(Stub); err != nil {
		// Remove the empty/partial file so a subsequent Scaffold call can retry
		// (otherwise O_EXCL would see ErrExist and return nil, leaving the file
		// corrupt forever).
		writeErr := fmt.Errorf("scaffold %s: %w", Filename, err)
		if removeErr := os.Remove(path); removeErr != nil {
			return fmt.Errorf("%w; also failed to remove partial file (manual cleanup required): %v", writeErr, removeErr)
		}
		return writeErr
	}
	return nil
}

// Load parses <root>/.makeslop.yaml and returns validated Excludes, Network,
// and any error. Missing file yields zero values with no error. Malformed YAML,
// unknown fields, cross-list duplicates, reserved-path collisions, and invalid
// paths/addresses are errors wrapped with "projectconfig: ". Symlinks and
// missing entries are silently dropped. root must be absolute and
// EvalSymlinks-evaluated.
func Load(root string) (Excludes, Network, error) {
	path := filepath.Join(root, Filename)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Excludes{}, Network{}, nil
		}
		return Excludes{}, Network{}, fmt.Errorf("projectconfig: read %s: %w", Filename, err)
	}

	// Decode with strict mode: unknown top-level fields cause an error, surfacing
	// typos (e.g. "excludes:" vs "exclude:") immediately.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var schema yamlSchema
	if err := dec.Decode(&schema); err != nil {
		if errors.Is(err, io.EOF) {
			// Empty file, whitespace-only, or comment-only YAML: treat as zero config.
			return Excludes{}, Network{}, nil
		}
		return Excludes{}, Network{}, fmt.Errorf("projectconfig: parse %s: %w", Filename, err)
	}

	patterns, err := validatePatterns(schema.Exclude.Scan.Patterns)
	if err != nil {
		return Excludes{}, Network{}, err
	}
	skipDirs, err := validateSkipDirs(schema.Exclude.Scan.SkipDirs)
	if err != nil {
		return Excludes{}, Network{}, err
	}

	cleanedFiles, err := validateEntries(schema.Exclude.Files, "exclude.files")
	if err != nil {
		return Excludes{}, Network{}, err
	}
	cleanedDirs, err := validateEntries(schema.Exclude.Dirs, "exclude.dirs")
	if err != nil {
		return Excludes{}, Network{}, err
	}

	// Cross-list duplicate check: a path in both lists is an error. Done before
	// stat-drop so the error is deterministic regardless of on-disk state.
	seen := make(map[string]struct{}, len(cleanedFiles))
	for _, rel := range cleanedFiles {
		seen[rel] = struct{}{}
	}
	for _, rel := range cleanedDirs {
		if _, ok := seen[rel]; ok {
			return Excludes{}, Network{}, fmt.Errorf("projectconfig: path %q listed in both exclude.files and exclude.dirs", rel)
		}
	}

	files, err := statFilter(root, cleanedFiles, func(info os.FileInfo) bool { return info.Mode().IsRegular() })
	if err != nil {
		return Excludes{}, Network{}, err
	}
	dirs, err := statFilter(root, cleanedDirs, func(info os.FileInfo) bool { return info.IsDir() })
	if err != nil {
		return Excludes{}, Network{}, err
	}

	files = dedupSorted(files)
	dirs = dedupSorted(dirs)

	var netCfg Network
	if addr := schema.Network.Proxy.Address; addr != "" {
		host, port, splitErr := net.SplitHostPort(addr)
		if splitErr != nil || host == "" || port == "" {
			return Excludes{}, Network{}, fmt.Errorf("projectconfig: invalid network.proxy.address %q: must be host:port", addr)
		}
		netCfg.ProxyAddress = addr
	}

	return Excludes{Files: files, Dirs: dirs, Patterns: patterns, SkipDirs: skipDirs}, netCfg, nil
}

// validateEntries cleans and validates user-supplied relative paths for the
// given list; returns an error on the first invalid entry.
func validateEntries(entries []string, listName string) ([]string, error) {
	cleaned := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry == "" {
			return nil, fmt.Errorf("projectconfig: empty path in %s", listName)
		}
		if filepath.IsAbs(entry) {
			return nil, fmt.Errorf("projectconfig: absolute path %q in %s; must be relative to project root", entry, listName)
		}
		c := filepath.Clean(entry)
		if !filepath.IsLocal(c) {
			return nil, fmt.Errorf("projectconfig: path %q escapes project root", entry)
		}
		// "foo/.." cleans to "." — a tmpfs or /dev/null overlay on "." would shadow the workspace root.
		if c == "." {
			return nil, fmt.Errorf("projectconfig: path %q refers to project root; entries must be strictly under root", entry)
		}
		if _, reserved := reservedPaths[c]; reserved {
			return nil, fmt.Errorf("projectconfig: path %q collides with a reserved agent path", c)
		}
		cleaned = append(cleaned, c)
	}
	return cleaned, nil
}

// validatePatterns cleans and validates user-supplied basename glob patterns for
// exclude.scan.patterns. Rejects empty entries and syntactically invalid globs
// (filepath.ErrBadPattern). Returns deduplicated, sorted patterns.
func validatePatterns(entries []string) ([]string, error) {
	for _, p := range entries {
		if p == "" {
			return nil, fmt.Errorf("projectconfig: empty pattern in exclude.scan.patterns")
		}
		// filepath.Match with "" as name validates syntax without matching anything.
		if _, err := filepath.Match(p, ""); err != nil {
			return nil, fmt.Errorf("projectconfig: invalid scan pattern %q: %w", p, err)
		}
	}
	// Patterns are basename globs, not paths — filepath.Clean is intentionally
	// not applied. "*.env" and ".env.*" are not path components, so cleaning
	// them would be wrong (e.g. it would strip leading dots or reduce "a/../b"
	// in a glob into something unintended). The original entries are passed
	// directly to dedupSorted.
	return dedupSorted(entries), nil
}

// validateSkipDirs cleans and validates user-supplied bare directory names for
// exclude.scan.skip-dirs. Rejects empty entries, entries containing a path
// separator, and the special names "." and "..". Returns deduplicated, sorted
// names.
func validateSkipDirs(entries []string) ([]string, error) {
	for _, d := range entries {
		if d == "" {
			return nil, fmt.Errorf("projectconfig: empty entry in exclude.scan.skip-dirs")
		}
		if d == "." || d == ".." {
			return nil, fmt.Errorf("projectconfig: skip-dir %q must be a bare directory name", d)
		}
		// Reject entries that contain a path separator.
		if strings.Contains(d, "/") {
			return nil, fmt.Errorf("projectconfig: skip-dir %q must be a bare directory name", d)
		}
	}
	return dedupSorted(entries), nil
}

// statFilter Lstats each relative path under root; silently drops missing and
// wrong-type entries (symlinks included). keep decides the acceptable type.
func statFilter(root string, cleaned []string, keep func(os.FileInfo) bool) ([]string, error) {
	var result []string
	for _, rel := range cleaned {
		abs := filepath.Join(root, rel)
		info, err := os.Lstat(abs)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("projectconfig: stat %s: %w", rel, err)
		}
		if !keep(info) {
			continue // includes symlinks
		}
		result = append(result, abs)
	}
	return result, nil
}

// dedupSorted returns a deduplicated, sorted copy; input is not modified.
func dedupSorted(paths []string) []string {
	if len(paths) == 0 {
		return paths
	}
	cp := make([]string, len(paths))
	copy(cp, paths)
	sort.Strings(cp)
	// compact: keep only the first of each run of equal strings.
	// Aliasing cp[:1] as the output slice is safe because deduplication only
	// shrinks the slice — out never overtakes the read head, so writes never
	// clobber elements that have not yet been processed.
	out := cp[:1]
	for _, p := range cp[1:] {
		if p != out[len(out)-1] {
			out = append(out, p)
		}
	}
	return out
}
