// Package projectconfig owns the project-local .makeslop.yaml file: its
// scaffold (init) and its parse (go). All returned paths are absolute and
// guaranteed to be under the project root (the package is the trust boundary
// for the user-supplied YAML, mirroring security.Scan's role for fd output).
//
// # Precondition
//
// Every function that accepts a root parameter requires that root be an
// absolute, filepath.EvalSymlinks-evaluated path — the same precondition as
// workspace.Lookup, security.Scan, and docker.BuildSpec. The cobra layer
// enforces this via resolvePwd; any direct caller must do the same.
//
// # Under-root guarantee
//
// Load validates every entry and only returns absolute paths that are under
// root. Consumers (e.g. docker.BuildSpec) may rely on this guarantee.
//
// # Symlink behaviour
//
// Load uses os.Lstat (not os.Stat) when checking whether an entry exists on
// disk. Symlink masking has ambiguous semantics (mask the symlink itself or
// its target?), and the rest of the codebase resolves symlinks at the boundary
// (resolvePwd, EvalSymlinks in main.go) — but here the user-supplied path is
// the input, not the output. Entries whose Lstat result is a symlink are
// silently dropped, matching the drop-if-not-regular/dir policy and avoiding a
// class of symlink-points-outside-root surprises that would otherwise require
// an extra under-root re-check after EvalSymlinks.
//
// # Reserved agent paths
//
// The relative paths .claude, .codex, docs, and CLAUDE.md are already mounted
// by docker.BuildSpec for agent state. Listing any of these in the YAML is a
// hard error (projectconfig: path %q collides with a reserved agent path)
// because mount order means a user overlay would shadow the agent mount,
// silently disabling agent features.
//
// # Network configuration
//
// Load also parses an optional network.proxy.address field. When present, the
// address is validated as a syntactic "host:port" pair via net.SplitHostPort
// (no network I/O is performed). The validated address is returned in the
// Network return value; an empty ProxyAddress means no proxy was configured.
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

	"gopkg.in/yaml.v3"
)

// Filename is the project-local config file name, relative to the project root.
const Filename = ".makeslop.yaml"

// Stub is the byte-stable content written by Scaffold. It is exactly 30 bytes
// including the trailing newline. Exported so tests can compare against it
// without hardcoding the bytes in multiple places.
var Stub = []byte("exclude:\n  dirs: []\n  files: []\n")

// reservedPaths is the set of relative paths already mounted by
// docker.BuildSpec for agent state. Listing any of these in the YAML is a
// hard error (would silently shadow the agent mount and disable agent
// features).
var reservedPaths = map[string]struct{}{
	".claude":   {},
	".codex":    {},
	"docs":      {},
	"CLAUDE.md": {},
}

// Excludes is the parsed, validated, host-resolved result of Load.
// Paths are absolute, under root, deduplicated within each list, and sorted.
type Excludes struct {
	Files []string // absolute host paths; /dev/null overlay targets
	Dirs  []string // absolute host paths; tmpfs overlay targets
}

// Network is the parsed, validated network configuration from .makeslop.yaml.
// ProxyAddress is the upstream forward proxy address in "host:port" form.
// An empty ProxyAddress means no proxy was configured (the network: section
// is absent or network.proxy.address is unset).
//
// Load performs only syntactic validation (net.SplitHostPort) — no network
// I/O, no reachability check.
type Network struct {
	ProxyAddress string // "host:port"; "" when unconfigured
}

// yamlSchema is the strict decode target. Using KnownFields(true) means any
// top-level key other than "exclude" / "network" (and their sub-keys) is
// rejected immediately with a meaningful decoder error.
type yamlSchema struct {
	Exclude struct {
		Dirs  []string `yaml:"dirs"`
		Files []string `yaml:"files"`
	} `yaml:"exclude"`
	Network struct {
		Proxy struct {
			Address string `yaml:"address"`
		} `yaml:"proxy"`
	} `yaml:"network"`
}

// Scaffold creates <root>/.makeslop.yaml with the empty-list stub if it does
// not already exist. EEXIST is treated as success (idempotent) — a Scaffold
// call on a project that already has a hand-edited .makeslop.yaml leaves the
// file untouched. root MUST be absolute and EvalSymlinks-evaluated (same
// precondition as workspace.Init).
func Scaffold(root string) error {
	path := filepath.Join(root, Filename)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil // idempotent: file already exists
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

// Load parses <root>/.makeslop.yaml and returns the validated Excludes,
// Network configuration, and any error.
//
// A missing file yields zero Excludes and zero Network with no error. Malformed
// YAML, unknown fields, paths listed in both lists, reserved-agent-path
// collisions, invalid paths (absolute, .. escapes, empty), and an invalid
// network.proxy.address are surfaced as errors wrapped with "projectconfig: ".
// Symlinks and entries that do not exist on disk are silently dropped (Lstat +
// IsRegular/IsDir).
//
// Network.ProxyAddress is set only when network.proxy.address is present in the
// YAML and passes syntactic validation (net.SplitHostPort — no network I/O, no
// reachability check). An empty ProxyAddress means no proxy was configured.
//
// root MUST be absolute and EvalSymlinks-evaluated.
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

	// Validate and clean all entries, building cleaned relative path sets.
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

	// Stat-and-drop: Lstat each entry, drop missing or wrong-type entries silently.
	files, err := statFilter(root, cleanedFiles, func(info os.FileInfo) bool { return info.Mode().IsRegular() })
	if err != nil {
		return Excludes{}, Network{}, err
	}
	dirs, err := statFilter(root, cleanedDirs, func(info os.FileInfo) bool { return info.IsDir() })
	if err != nil {
		return Excludes{}, Network{}, err
	}

	// Deduplicate within each list (user may list same path twice) and sort.
	files = dedupSorted(files)
	dirs = dedupSorted(dirs)

	// Validate network.proxy.address if present.
	var netCfg Network
	if addr := schema.Network.Proxy.Address; addr != "" {
		host, port, splitErr := net.SplitHostPort(addr)
		if splitErr != nil || host == "" || port == "" {
			return Excludes{}, Network{}, fmt.Errorf("projectconfig: invalid network.proxy.address %q: must be host:port", addr)
		}
		netCfg.ProxyAddress = addr
	}

	return Excludes{Files: files, Dirs: dirs}, netCfg, nil
}

// validateEntries validates and cleans a list of user-supplied relative path
// strings for the given list name (e.g. "exclude.files"). It returns the
// slice of cleaned relative paths, or an error for the first invalid entry.
func validateEntries(entries []string, listName string) ([]string, error) {
	cleaned := make([]string, 0, len(entries))
	for _, entry := range entries {
		// Rule 1: reject empty string.
		if entry == "" {
			return nil, fmt.Errorf("projectconfig: empty path in %s", listName)
		}
		// Rule 2: reject absolute paths.
		if filepath.IsAbs(entry) {
			return nil, fmt.Errorf("projectconfig: absolute path %q in %s; must be relative to project root", entry, listName)
		}
		// Rule 3: clean the path.
		c := filepath.Clean(entry)
		// Rule 4: reject ..‐escapes.
		if !filepath.IsLocal(c) {
			return nil, fmt.Errorf("projectconfig: path %q escapes project root", entry)
		}
		// Rule 5: reject "." (and paths like "foo/.." that clean to ".") — a
		// tmpfs or /dev/null overlay on "." would shadow the entire workspace root.
		if c == "." {
			return nil, fmt.Errorf("projectconfig: path %q refers to project root; entries must be strictly under root", entry)
		}
		// Rule 6: reject reserved agent paths.
		if _, reserved := reservedPaths[c]; reserved {
			return nil, fmt.Errorf("projectconfig: path %q collides with a reserved agent path", c)
		}
		cleaned = append(cleaned, c)
	}
	return cleaned, nil
}

// statFilter maps cleaned relative paths to absolute host paths under root,
// Lstats each, silently drops missing or wrong-type entries (symlinks included),
// and returns the surviving absolute paths. The keep function decides whether
// the os.FileInfo is the right type.
func statFilter(root string, cleaned []string, keep func(os.FileInfo) bool) ([]string, error) {
	var result []string
	for _, rel := range cleaned {
		abs := filepath.Join(root, rel)
		info, err := os.Lstat(abs)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // silently drop missing entries
			}
			return nil, fmt.Errorf("projectconfig: stat %s: %w", rel, err)
		}
		if !keep(info) {
			continue // silently drop wrong-type entries (includes symlinks)
		}
		result = append(result, abs)
	}
	return result, nil
}

// dedupSorted deduplicates a slice of strings (by identity) and returns a
// lexicographically sorted copy. The input slice is not modified.
func dedupSorted(paths []string) []string {
	if len(paths) == 0 {
		return paths
	}
	seen := make(map[string]struct{}, len(paths))
	var out []string
	for _, p := range paths {
		if _, ok := seen[p]; !ok {
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}
