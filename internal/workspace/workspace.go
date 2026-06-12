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

// Lookup returns the registered ancestor root (mount this, not pwd) and its
// cache directory. pwd must be absolute and EvalSymlinks-evaluated.
// The caller must supply a previously-loaded *config.Settings; Lookup does not
// load settings itself, which lets callers load once and pass the result to
// both Lookup and any subsequent settings-dependent logic.
// A nil settings is treated as an empty settings (no workspaces registered).
func (w *Workspaces) Lookup(s *config.Settings, pwd string) (matchedRoot, cacheDir string, err error) {
	if s == nil {
		return "", "", ErrNotRegistered
	}
	matched, ws, ok := w.findAncestor(s, pwd)
	if !ok {
		return "", "", ErrNotRegistered
	}
	return matched, w.cacheDir(ws.Name), nil
}

// cacheDir is the per-workspace cache directory under the base dir.
func (w *Workspaces) cacheDir(name string) string {
	return filepath.Join(w.baseDir, config.WorkspacesDir, name)
}

// Init registers pwd (absolute, EvalSymlinks-evaluated); registering an
// already-known pwd or ancestor is a no-op. The Load→mutate→Save sequence runs
// under config.WithLock so concurrent Init calls for distinct paths don't lose
// updates.
func (w *Workspaces) Init(pwd string) (string, error) {
	var workspaceDir string
	err := config.WithLock(w.baseDir, func() error {
		s, err := config.Load(w.baseDir)
		if err != nil {
			return err
		}
		if _, ws, ok := w.findAncestor(s, pwd); ok {
			workspaceDir = w.cacheDir(ws.Name)
			return nil
		}
		ws := config.Workspace{Name: workspaceName(pwd), CreatedAt: time.Now().UTC()}
		s.Workspaces[pwd] = ws
		workspaceDir = w.cacheDir(ws.Name)
		if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
			return fmt.Errorf("create workspace dir %s: %w", workspaceDir, err)
		}
		if err := scaffoldTemplate(workspaceDir); err != nil {
			return err
		}
		// Save failure leaves the cache dir orphaned; the next Init for this pwd reclaims it.
		return config.Save(w.baseDir, s)
	})
	if err != nil {
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
