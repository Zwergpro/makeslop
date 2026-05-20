// Package workspace manages the on-disk makeslop workspace registry stored
// under ~/.makeslop. It exposes lookup and registration operations keyed by
// the absolute, symlink-evaluated path of a workspace root.
package workspace

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

const (
	settingsFile   = "settings.json"
	workspacesDir  = "workspaces"
	currentVersion = 1
)

// Workspace is a single registered workspace entry.
type Workspace struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// Settings is the persisted shape of <baseDir>/settings.json.
// Workspaces is keyed by absolute, symlink-evaluated workspace root paths.
type Settings struct {
	Version    int                  `json:"version"`
	Workspaces map[string]Workspace `json:"workspaces"`
}

// Workspaces provides access to the on-disk makeslop workspace registry
// rooted at baseDir.
type Workspaces struct {
	baseDir string
}

// ErrNotRegistered is returned by Lookup when no ancestor of pwd is a
// registered workspace.
var ErrNotRegistered = errors.New("no workspace registered for path")

// New constructs a Workspaces bound to baseDir.
func New(baseDir string) *Workspaces {
	return &Workspaces{baseDir: baseDir}
}

// DefaultBaseDir returns ~/.makeslop.
func DefaultBaseDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".makeslop"), nil
}

// loadSettings reads <baseDir>/settings.json. A missing file yields an empty
// Settings at the current version (not an error); malformed JSON is wrapped.
func (w *Workspaces) loadSettings() (*Settings, error) {
	path := filepath.Join(w.baseDir, settingsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Settings{Version: currentVersion, Workspaces: map[string]Workspace{}}, nil
		}
		return nil, fmt.Errorf("read settings %s: %w", path, err)
	}
	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse settings %s: %w", path, err)
	}
	if s.Workspaces == nil {
		s.Workspaces = map[string]Workspace{}
	}
	return &s, nil
}

// saveSettings atomically writes settings via temp-file + intra-dir rename so
// a crash mid-write cannot leave a half-written settings.json behind.
func (w *Workspaces) saveSettings(s *Settings) error {
	if err := os.MkdirAll(w.baseDir, 0o755); err != nil {
		return fmt.Errorf("create base dir %s: %w", w.baseDir, err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	data = append(data, '\n') // POSIX-friendly trailing newline

	tmp, err := os.CreateTemp(w.baseDir, settingsFile+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp settings file: %w", err)
	}
	tmpName := tmp.Name()
	// Cleared once the rename succeeds; otherwise defer removes the temp file.
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

	final := filepath.Join(w.baseDir, settingsFile)
	if err := os.Rename(tmpName, final); err != nil {
		return fmt.Errorf("rename settings file into place: %w", err)
	}
	cleanup = false
	return nil
}

// findAncestor walks pwd and its parents, returning the first ancestor that
// is a registered workspace. pwd MUST be an absolute, EvalSymlinks-evaluated
// path.
func (w *Workspaces) findAncestor(s *Settings, pwd string) (matchedPath string, ws Workspace, ok bool) {
	for p := pwd; ; {
		if entry, found := s.Workspaces[p]; found {
			return p, entry, true
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", Workspace{}, false
		}
		p = parent
	}
}

// Lookup returns the cache directory of the first registered ancestor of pwd,
// or ErrNotRegistered if none. Never mutates state on disk.
// pwd MUST be an absolute, EvalSymlinks-evaluated path.
func (w *Workspaces) Lookup(pwd string) (string, error) {
	s, err := w.loadSettings()
	if err != nil {
		return "", err
	}
	_, ws, ok := w.findAncestor(s, pwd)
	if !ok {
		return "", ErrNotRegistered
	}
	return filepath.Join(w.baseDir, workspacesDir, ws.Name), nil
}

// Init is idempotent: if pwd (or an ancestor) is already registered it
// returns the existing cache directory without touching settings. Otherwise
// it registers pwd, creates the cache directory, and persists settings.
// pwd MUST be an absolute, EvalSymlinks-evaluated path.
func (w *Workspaces) Init(pwd string) (string, error) {
	s, err := w.loadSettings()
	if err != nil {
		return "", err
	}
	if _, ws, ok := w.findAncestor(s, pwd); ok {
		return filepath.Join(w.baseDir, workspacesDir, ws.Name), nil
	}
	ws := Workspace{Name: workspaceName(pwd), CreatedAt: time.Now().UTC()}
	// loadSettings guarantees a non-nil Workspaces map.
	s.Workspaces[pwd] = ws
	workspaceDir := filepath.Join(w.baseDir, workspacesDir, ws.Name)
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		return "", fmt.Errorf("create workspace dir %s: %w", workspaceDir, err)
	}
	// If saveSettings fails after MkdirAll, the cache dir is left orphaned.
	// The next successful Init for the same pwd reclaims it (MkdirAll no-op).
	if err := w.saveSettings(s); err != nil {
		return "", err
	}
	return workspaceDir, nil
}

// workspaceName derives "<basename>-<sha256(absPath)[:6]>". When absPath is
// the filesystem root, "root" is used as the basename to keep it non-empty.
func workspaceName(absPath string) string {
	base := filepath.Base(absPath)
	if base == string(filepath.Separator) {
		base = "root"
	}
	sum := sha256.Sum256([]byte(absPath))
	return base + "-" + hex.EncodeToString(sum[:])[:6]
}
