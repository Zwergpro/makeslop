// Package cache manages the on-disk makeslop project cache stored under
// ~/.makeslop. It exposes lookup and registration operations keyed by the
// absolute, symlink-evaluated path of a project root.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// settingsFile is the on-disk filename for cache settings, located directly
// inside the cache base directory.
const settingsFile = "settings.json"

// projectsDir is the subdirectory of baseDir that holds per-project cache dirs.
const projectsDir = "projects"

// currentVersion is the schema version written for new settings files.
const currentVersion = 1

// Project is a single registered project entry in the cache settings.
type Project struct {
	// Name is the on-disk directory name under <baseDir>/projects.
	Name string `json:"name"`
	// CreatedAt is the registration timestamp (UTC, RFC3339).
	CreatedAt time.Time `json:"created_at"`
}

// Settings is the persisted shape of <baseDir>/settings.json.
type Settings struct {
	// Version is the schema version. Currently always 1.
	Version int `json:"version"`
	// Projects maps absolute, symlink-evaluated project root paths to entries.
	Projects map[string]Project `json:"projects"`
}

// Cache provides access to the on-disk makeslop cache rooted at baseDir.
type Cache struct {
	baseDir string
}

// ErrNotRegistered is returned by Lookup when no ancestor of pwd is a
// registered project.
var ErrNotRegistered = errors.New("no project registered for path")

// New constructs a Cache bound to baseDir. It does not touch the filesystem.
func New(baseDir string) *Cache {
	return &Cache{baseDir: baseDir}
}

// DefaultBaseDir returns the conventional cache base directory ~/.makeslop.
// It resolves the user's home directory via os.UserHomeDir.
func DefaultBaseDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".makeslop"), nil
}

// loadSettings reads <baseDir>/settings.json. When the file is missing it
// returns an empty Settings with the current version and an initialized
// projects map. Malformed JSON yields a wrapped error.
func (c *Cache) loadSettings() (*Settings, error) {
	path := filepath.Join(c.baseDir, settingsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Settings{Version: currentVersion, Projects: map[string]Project{}}, nil
		}
		return nil, fmt.Errorf("read settings %s: %w", path, err)
	}
	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse settings %s: %w", path, err)
	}
	if s.Projects == nil {
		s.Projects = map[string]Project{}
	}
	return &s, nil
}

// saveSettings atomically writes settings to <baseDir>/settings.json. It
// creates baseDir if absent, writes to a sibling temp file (so rename is
// intra-device), and renames into place. JSON is indented with two spaces.
func (c *Cache) saveSettings(s *Settings) error {
	if err := os.MkdirAll(c.baseDir, 0o755); err != nil {
		return fmt.Errorf("create base dir %s: %w", c.baseDir, err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	// Append a trailing newline so the file is POSIX-friendly.
	data = append(data, '\n')

	tmp, err := os.CreateTemp(c.baseDir, settingsFile+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp settings file: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if anything goes wrong before the rename.
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp settings file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp settings file: %w", err)
	}

	final := filepath.Join(c.baseDir, settingsFile)
	if err := os.Rename(tmpName, final); err != nil {
		return fmt.Errorf("rename settings file into place: %w", err)
	}
	cleanup = false
	return nil
}

// findAncestor walks pwd and its parents via filepath.Dir, stopping when the
// parent equals the current element (filesystem root reached). It returns the
// first ancestor present as a key in s.Projects. pwd MUST be an absolute,
// symlink-evaluated path; callers are responsible.
func (c *Cache) findAncestor(s *Settings, pwd string) (matchedPath string, project Project, ok bool) {
	for p := pwd; ; {
		if proj, found := s.Projects[p]; found {
			return p, proj, true
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", Project{}, false
		}
		p = parent
	}
}

// Lookup walks pwd's ancestors and returns the cache directory of the first
// registered project found. Returns ErrNotRegistered if none match. Never
// mutates state on disk. pwd MUST be an absolute, EvalSymlinks-evaluated path;
// callers are responsible.
func (c *Cache) Lookup(pwd string) (string, error) {
	s, err := c.loadSettings()
	if err != nil {
		return "", err
	}
	_, proj, ok := c.findAncestor(s, pwd)
	if !ok {
		return "", ErrNotRegistered
	}
	return filepath.Join(c.baseDir, projectsDir, proj.Name), nil
}

// Init returns the cache directory for pwd: if an ancestor (or pwd itself) is
// already registered, the existing project's cache directory is returned
// without mutating settings (idempotent). Otherwise pwd is registered as a new
// project, its cache directory is created, settings are persisted, and the new
// cache directory is returned. pwd MUST be an absolute, EvalSymlinks-evaluated
// path; callers are responsible.
func (c *Cache) Init(pwd string) (string, error) {
	s, err := c.loadSettings()
	if err != nil {
		return "", err
	}
	if _, proj, ok := c.findAncestor(s, pwd); ok {
		return filepath.Join(c.baseDir, projectsDir, proj.Name), nil
	}
	proj := Project{Name: projectName(pwd), CreatedAt: time.Now().UTC()}
	// loadSettings always returns a non-nil Projects map (empty defaults,
	// JSON null normalisation, and any successful unmarshal of an object),
	// so no nil-guard is needed here.
	s.Projects[pwd] = proj
	projectDir := filepath.Join(c.baseDir, projectsDir, proj.Name)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		return "", fmt.Errorf("create project dir %s: %w", projectDir, err)
	}
	// Note: if saveSettings fails after MkdirAll succeeds, the freshly created
	// project directory is left orphaned (not referenced from settings.json).
	// A subsequent Init for the same pwd is idempotent at the directory level
	// (MkdirAll is a no-op) and will re-attempt the save, so the orphan is
	// reclaimed on the next successful run. No explicit cleanup is attempted
	// because doing so safely would require distinguishing "we just created
	// this" from "this already existed", which is not worth the complexity.
	if err := c.saveSettings(s); err != nil {
		return "", err
	}
	return projectDir, nil
}

// projectName derives the on-disk project directory name from an absolute
// path: "<basename>-<sha256(absPath)[:6]>". When absPath is the filesystem
// root the basename "root" is used to keep the result non-empty. Callers must
// pass a non-empty absolute path.
func projectName(absPath string) string {
	base := filepath.Base(absPath)
	if base == string(filepath.Separator) {
		base = "root"
	}
	sum := sha256.Sum256([]byte(absPath))
	return base + "-" + hex.EncodeToString(sum[:])[:6]
}
