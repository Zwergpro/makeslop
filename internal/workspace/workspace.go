// Package workspace manages the on-disk makeslop workspace registry stored
// under ~/.makeslop. It exposes lookup and registration operations keyed by
// the absolute, symlink-evaluated path of a workspace root. The settings file
// itself is owned by internal/config; this package only operates on the
// Workspaces map inside it.
package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Zwergpro/makeslop/internal/config"
)

type Workspaces struct {
	baseDir string
}

var ErrNotRegistered = errors.New("no workspace registered for path")

func New(baseDir string) *Workspaces {
	return &Workspaces{baseDir: baseDir}
}

// pwd MUST be an absolute, EvalSymlinks-evaluated path.
func (w *Workspaces) findAncestor(s *config.Settings, pwd string) (matchedPath string, ws config.Workspace, ok bool) {
	for p := pwd; ; {
		if entry, found := s.Workspaces[p]; found {
			return p, entry, true
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", config.Workspace{}, false
		}
		p = parent
	}
}

// Lookup returns the registered ancestor root (callers must mount this, not
// pwd) and its cache directory. Never mutates state on disk.
// pwd MUST be an absolute, EvalSymlinks-evaluated path.
func (w *Workspaces) Lookup(pwd string) (matchedRoot, cacheDir string, err error) {
	s, err := config.Load(w.baseDir)
	if err != nil {
		return "", "", err
	}
	matched, ws, ok := w.findAncestor(s, pwd)
	if !ok {
		return "", "", ErrNotRegistered
	}
	return matched, filepath.Join(w.baseDir, config.WorkspacesDir, ws.Name), nil
}

// Init is idempotent: an already-registered pwd (or ancestor) is a no-op that
// does not touch settings or the scaffolded template.
// pwd MUST be an absolute, EvalSymlinks-evaluated path.
func (w *Workspaces) Init(pwd string) (string, error) {
	s, err := config.Load(w.baseDir)
	if err != nil {
		return "", err
	}
	if _, ws, ok := w.findAncestor(s, pwd); ok {
		return filepath.Join(w.baseDir, config.WorkspacesDir, ws.Name), nil
	}
	ws := config.Workspace{Name: workspaceName(pwd), CreatedAt: time.Now().UTC()}
	// config.Load guarantees a non-nil Workspaces map.
	s.Workspaces[pwd] = ws
	workspaceDir := filepath.Join(w.baseDir, config.WorkspacesDir, ws.Name)
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		return "", fmt.Errorf("create workspace dir %s: %w", workspaceDir, err)
	}
	if err := scaffoldTemplate(workspaceDir); err != nil {
		return "", err
	}
	// If Save fails after MkdirAll, the cache dir is left orphaned.
	// The next successful Init for the same pwd reclaims it (MkdirAll no-op).
	if err := config.Save(w.baseDir, s); err != nil {
		return "", err
	}
	return workspaceDir, nil
}

func scaffoldTemplate(workspaceDir string) error {
	for _, d := range []string{".claude", ".codex", "docs"} {
		p := filepath.Join(workspaceDir, d)
		if err := os.MkdirAll(p, 0o755); err != nil {
			return fmt.Errorf("scaffold %s: %w", p, err)
		}
	}
	p := filepath.Join(workspaceDir, "CLAUDE.md")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("scaffold %s: %w", p, err)
	}
	if err == nil {
		f.Close()
	}
	return nil
}

// Filesystem root maps to "root" so the basename is never empty.
func workspaceName(absPath string) string {
	base := filepath.Base(absPath)
	if base == string(filepath.Separator) {
		base = "root"
	}
	sum := sha256.Sum256([]byte(absPath))
	return base + "-" + hex.EncodeToString(sum[:])[:6]
}
