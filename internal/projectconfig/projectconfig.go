// Package projectconfig parses and scaffolds the project-local .makeslop.yaml.
// All returned paths are absolute and guaranteed to be under the project root
// (trust boundary for user-supplied YAML).
//
// root parameters must be absolute and EvalSymlinks-evaluated; any direct
// caller must enforce this.
//
// Load returns only paths that are under root. Symlinks are silently dropped
// (masking a symlink has ambiguous semantics: the symlink itself or its target?).
// Reserved agent paths (.claude, .codex, docs, CLAUDE.md) are a hard error
// because a user overlay would shadow the agent mount.
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

// Stub is the content Scaffold writes. Exported so tests can compare without hardcoding.
var Stub = []byte("exclude:\n  dirs: []\n  files: []\n")

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
	Files []string // absolute host paths; /dev/null overlay targets
	Dirs  []string // absolute host paths; tmpfs overlay targets
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

	return Excludes{Files: files, Dirs: dirs}, netCfg, nil
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
