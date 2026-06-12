package projectconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// renderStub returns the .makeslop.yaml stub bytes for the given Cache defaults.
func renderStub(c Cache) []byte {
	return []byte(fmt.Sprintf(`exclude:
  scan:
    patterns:
      - "*.env"
      - ".env.*"
      - "*.pem"
      - "*.key"
      - "*.p12"
      - "*.pfx"
      - "*.tfstate"
      - "id_rsa*"
      - "id_ed25519*"
      - ".npmrc"
      - ".netrc"
      - ".git-credentials"
      - ".pypirc"
      - ".htpasswd"
      - "service-account*.json"
      - "kubeconfig"
      - "*.kubeconfig"
    skip-dirs:
      - .git
      - node_modules
      - vendor
      - .venv
  files: []
  dirs: []
cache:
  content: %t
  agent: %t
`, c.Content, c.Agent))
}

// Filename is the project-local config file name, relative to the project root.
const Filename = ".makeslop.yaml"

// Stub is the content Scaffold writes for the default Cache{true,true}. It seeds
// the default scan filters as active values so new projects get secret masking
// out of the box.
var Stub = renderStub(Cache{Content: true, Agent: true})

// reservedPaths lists paths docker.BuildSpec may mount over the project root;
// user overlays in exclude.dirs/exclude.files would silently shadow them. The
// check fires regardless of cache config, so disabling a cache group and still
// listing such a path errors even when the mount is inactive (conservative).
var reservedPaths = map[string]struct{}{
	".claude":        {},
	".codex":         {},
	"docs":           {},
	"CLAUDE.md":      {},
	".makeslop.yaml": {},
}

// Excludes is the parsed, validated, host-resolved result of Load.
// Paths are absolute, under root, deduplicated within each list, and sorted.
type Excludes struct {
	Files    []string // absolute host paths; /dev/null overlay targets
	Dirs     []string // absolute host paths; tmpfs overlay targets
	Patterns []string // basename globs for the WalkDir secret scan; deduped and sorted
	SkipDirs []string // bare directory names pruned during the WalkDir scan; deduped and sorted
	Warnings []string // human-readable notices about dropped entries (e.g. symlinks); callers should surface these
}

// Cache is the parsed cache-overlay configuration. Both fields default to true
// when the cache: block is absent (backward-compatible).
type Cache struct {
	Content bool // mount per-workspace cache docs/ + CLAUDE.md (default true)
	Agent   bool // mount per-workspace cache .claude/ + .codex/ (default true)
}

// yamlSchema is the strict decode target. KnownFields(true) rejects any unknown
// key — including a stale "network:" block from a prior makeslop version, the
// intended loud break.
type yamlSchema struct {
	Exclude struct {
		Scan struct {
			Patterns []string `yaml:"patterns"`
			SkipDirs []string `yaml:"skip-dirs"`
		} `yaml:"scan"`
		Dirs  []string `yaml:"dirs"`
		Files []string `yaml:"files"`
	} `yaml:"exclude"`
	Cache struct {
		Content *bool `yaml:"content"`
		Agent   *bool `yaml:"agent"`
	} `yaml:"cache"`
	// Decoded as yaml.Node for lenient scalar coercion (numbers/booleans
	// become their string representations).
	Environments map[string]yaml.Node `yaml:"environments"`
}

// Scaffold creates <root>/.makeslop.yaml with the stub for the given Cache
// defaults. Idempotent: EEXIST on a regular file is success and user edits are
// never clobbered (c is a no-op on an existing file). A symlink at the path —
// dangling or live — is rejected with a hard error: the project config must be
// a regular file so ProtectProjectConfig and Load behave predictably. root must
// be absolute and EvalSymlinks-evaluated.
func Scaffold(root string, c Cache) error {
	path := filepath.Join(root, Filename)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			// EEXIST could mean a regular file (idempotent success) or a symlink
			// (which O_EXCL treats as existing). Lstat to distinguish.
			info, lstErr := os.Lstat(path)
			if lstErr != nil {
				return fmt.Errorf("scaffold %s: %w", Filename, lstErr)
			}
			if info.Mode()&fs.ModeSymlink != 0 {
				return fmt.Errorf("projectconfig: %s is a symlink — the project config must be a regular file", Filename)
			}
			return nil
		}
		return fmt.Errorf("scaffold %s: %w", Filename, err)
	}
	defer f.Close()
	if _, err := f.Write(renderStub(c)); err != nil {
		// Remove the partial file so a retry isn't blocked by O_EXCL seeing
		// ErrExist and returning nil, leaving the file corrupt forever.
		writeErr := fmt.Errorf("scaffold %s: %w", Filename, err)
		if removeErr := os.Remove(path); removeErr != nil {
			return fmt.Errorf("%w; also failed to remove partial file (manual cleanup required): %v", writeErr, removeErr)
		}
		return writeErr
	}
	return nil
}

// Load parses <root>/.makeslop.yaml. The four-value return is:
//   - Excludes: file/dir masks and scan patterns. Excludes.Warnings carries
//     human-readable notices for symlinked entries (dropped with a warning);
//     missing entries and non-symlink wrong-type drops stay silent.
//   - Cache: per-workspace overlay settings; defaults to {true,true} when the
//     cache: block (or the whole file) is absent.
//   - []string: sorted "KEY=VALUE" env pairs from environments:; nil when the
//     block is absent (nil ≡ no env injection, backward-compatible).
//   - error: any parse, validation, or filesystem error, wrapped "projectconfig: ".
//
// The file at root/.makeslop.yaml must be a regular file. A symlink — dangling
// or live — is rejected with a hard error: masking and sandbox-policy behaviour
// depend on the file being a real file on disk.
//
// root must be absolute and EvalSymlinks-evaluated.
func Load(root string) (Excludes, Cache, []string, error) {
	path := filepath.Join(root, Filename)

	// Lstat before ReadFile to detect symlinks. ReadFile follows symlinks, which
	// would silently succeed for a live symlink or give ENOENT for a dangling one
	// (treating it as "no config" — the silent data-loss case this check closes).
	linfo, lstErr := os.Lstat(path)
	if lstErr != nil {
		if errors.Is(lstErr, fs.ErrNotExist) {
			return Excludes{}, Cache{Content: true, Agent: true}, nil, nil
		}
		return Excludes{}, Cache{}, nil, fmt.Errorf("projectconfig: read %s: %w", Filename, lstErr)
	}
	if linfo.Mode()&fs.ModeSymlink != 0 {
		return Excludes{}, Cache{}, nil, fmt.Errorf("projectconfig: %s is a symlink — the project config must be a regular file", Filename)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return Excludes{}, Cache{}, nil, fmt.Errorf("projectconfig: read %s: %w", Filename, err)
	}

	// Strict mode: unknown fields error out, surfacing typos and stale
	// "network:" blocks from prior makeslop versions.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var schema yamlSchema
	if err := dec.Decode(&schema); err != nil {
		if errors.Is(err, io.EOF) {
			// Empty, whitespace-only, or comment-only YAML: zero config.
			return Excludes{}, Cache{Content: true, Agent: true}, nil, nil
		}
		return Excludes{}, Cache{}, nil, fmt.Errorf("projectconfig: parse %s: %w", Filename, err)
	}

	patterns, err := validatePatterns(schema.Exclude.Scan.Patterns)
	if err != nil {
		return Excludes{}, Cache{}, nil, err
	}
	skipDirs, err := validateSkipDirs(schema.Exclude.Scan.SkipDirs)
	if err != nil {
		return Excludes{}, Cache{}, nil, err
	}

	cleanedFiles, err := validateEntries(schema.Exclude.Files, "exclude.files")
	if err != nil {
		return Excludes{}, Cache{}, nil, err
	}
	cleanedDirs, err := validateEntries(schema.Exclude.Dirs, "exclude.dirs")
	if err != nil {
		return Excludes{}, Cache{}, nil, err
	}

	// A path in both lists is an error. Checked before stat-drop so the error is
	// deterministic regardless of on-disk state.
	seen := make(map[string]struct{}, len(cleanedFiles))
	for _, rel := range cleanedFiles {
		seen[rel] = struct{}{}
	}
	for _, rel := range cleanedDirs {
		if _, ok := seen[rel]; ok {
			return Excludes{}, Cache{}, nil, fmt.Errorf("projectconfig: path %q listed in both exclude.files and exclude.dirs", rel)
		}
	}

	files, fileWarnings, err := statFilter(root, cleanedFiles, func(info os.FileInfo) bool { return info.Mode().IsRegular() })
	if err != nil {
		return Excludes{}, Cache{}, nil, err
	}
	dirs, dirWarnings, err := statFilter(root, cleanedDirs, func(info os.FileInfo) bool { return info.IsDir() })
	if err != nil {
		return Excludes{}, Cache{}, nil, err
	}

	files = dedupSorted(files)
	dirs = dedupSorted(dirs)

	// Merge warnings from both lists; deduplicate and sort.
	warnings := dedupSorted(append(fileWarnings, dirWarnings...))

	// Absent pointer (nil) means the field was unset in YAML, defaulting to true
	// (backward-compatible: absent block = both mounted).
	cacheCfg := Cache{
		Content: schema.Cache.Content == nil || *schema.Cache.Content,
		Agent:   schema.Cache.Agent == nil || *schema.Cache.Agent,
	}

	envVars, err := validateEnvironments(schema.Environments)
	if err != nil {
		return Excludes{}, Cache{}, nil, err
	}

	return Excludes{Files: files, Dirs: dirs, Patterns: patterns, SkipDirs: skipDirs, Warnings: warnings}, cacheCfg, envVars, nil
}

// validateEntries cleans and validates relative paths, erroring on the first
// invalid entry.
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

// validatePatterns validates exclude.scan.patterns basename globs, rejecting
// empty entries, path separators, and invalid glob syntax. Returns deduplicated,
// sorted patterns. Patterns are matched against basenames only (security.Scan
// calls filepath.Match(p, d.Name())), so path-style patterns like
// "secrets/*.pem" or "**/*.env" can never match and are rejected fail-loud.
func validatePatterns(entries []string) ([]string, error) {
	for _, p := range entries {
		if p == "" {
			return nil, fmt.Errorf("projectconfig: empty pattern in exclude.scan.patterns")
		}
		// Patterns are basename globs: a '/' can never match any basename, so
		// a path-style pattern silently masks nothing. Reject early so the user
		// gets a clear error instead of silent data loss.
		if strings.Contains(p, "/") {
			return nil, fmt.Errorf("projectconfig: scan pattern %q contains a path separator — patterns match basenames only", p)
		}
		// Matching against "" validates glob syntax without matching anything.
		if _, err := filepath.Match(p, ""); err != nil {
			return nil, fmt.Errorf("projectconfig: invalid scan pattern %q: %w", p, err)
		}
	}
	// No filepath.Clean: these are basename globs, not paths — cleaning would
	// mangle entries like ".env.*".
	return dedupSorted(entries), nil
}

// validateSkipDirs validates exclude.scan.skip-dirs bare directory names,
// rejecting empty entries, path separators, and "."/"..". Returns deduplicated,
// sorted names.
func validateSkipDirs(entries []string) ([]string, error) {
	for _, d := range entries {
		if d == "" {
			return nil, fmt.Errorf("projectconfig: empty entry in exclude.scan.skip-dirs")
		}
		if d == "." || d == ".." {
			return nil, fmt.Errorf("projectconfig: skip-dir %q must be a bare directory name", d)
		}
		if strings.Contains(d, "/") {
			return nil, fmt.Errorf("projectconfig: skip-dir %q must be a bare directory name", d)
		}
	}
	return dedupSorted(entries), nil
}

// validateEnvironments validates an environments: map into a sorted []string of
// "KEY=VALUE" pairs. Rules:
//   - Empty keys rejected ("-e =value" is broken docker syntax).
//   - Keys must not contain '=' (breaks KEY=VALUE encoding) or newline/tab.
//   - Values must be scalar nodes; non-scalars (lists/maps) rejected fail-loud.
//   - Null scalars (bare KEY: or KEY: null) rejected — almost always a mistake.
//   - Explicit empty string (KEY: "") accepted → "KEY=".
//
// yaml.v3 rejects duplicate map keys at decode time, so no dup-key handling here.
func validateEnvironments(env map[string]yaml.Node) ([]string, error) {
	if len(env) == 0 {
		return nil, nil
	}
	result := make([]string, 0, len(env))
	for k, v := range env {
		if k == "" {
			return nil, fmt.Errorf("projectconfig: empty key in environments")
		}
		if strings.Contains(k, "=") {
			return nil, fmt.Errorf("projectconfig: environment key %q must not contain '='", k)
		}
		if strings.ContainsAny(k, "\n\r\t") {
			return nil, fmt.Errorf("projectconfig: environment key %q must not contain newline or tab characters", k)
		}
		if v.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("projectconfig: environment key %q must be a scalar value", k)
		}
		if v.Tag == "!!null" {
			return nil, fmt.Errorf("projectconfig: environment key %q has no value", k)
		}
		if strings.ContainsAny(v.Value, "\n\r\t") {
			return nil, fmt.Errorf("projectconfig: environment key %q value must not contain newline or tab characters", k)
		}
		result = append(result, k+"="+v.Value)
	}
	sort.Strings(result)
	return result, nil
}

// statFilter Lstats each path under root. Symlinks are dropped with a warning
// message in warnings (degraded protection is not silent). Missing entries and
// non-symlink wrong-type drops are silently discarded. keep decides the
// acceptable type (checked only after the symlink guard).
func statFilter(root string, cleaned []string, keep func(os.FileInfo) bool) (result, warnings []string, err error) {
	for _, rel := range cleaned {
		abs := filepath.Join(root, rel)
		info, statErr := os.Lstat(abs)
		if statErr != nil {
			if errors.Is(statErr, fs.ErrNotExist) {
				continue
			}
			return nil, nil, fmt.Errorf("projectconfig: stat %s: %w", rel, statErr)
		}
		// Symlinks are dropped with a visible warning: masking a symlink is
		// ambiguous (the link or its target?), but silently skipping it could
		// leave secrets unmasked. Callers must surface these warnings.
		if info.Mode()&os.ModeSymlink != 0 {
			warnings = append(warnings, fmt.Sprintf("path %q is a symlink and is NOT masked", rel))
			continue
		}
		if !keep(info) {
			continue // non-symlink wrong-type drop (e.g. dir listed in files): stay silent
		}
		result = append(result, abs)
	}
	return result, warnings, nil
}

// dedupSorted returns a deduplicated, sorted copy; input is not modified.
func dedupSorted(paths []string) []string {
	if len(paths) == 0 {
		return paths
	}
	cp := make([]string, len(paths))
	copy(cp, paths)
	sort.Strings(cp)
	// Aliasing cp[:1] as output is safe: dedup only shrinks, so out never
	// overtakes the read head and never clobbers unprocessed elements.
	out := cp[:1]
	for _, p := range cp[1:] {
		if p != out[len(out)-1] {
			out = append(out, p)
		}
	}
	return out
}
