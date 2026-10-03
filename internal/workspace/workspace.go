package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

// Lookup returns the registered ancestor because mounting pwd would omit
// parent project files. Callers pass loaded settings to avoid another read.
// pwd must be absolute and resolved; nil settings mean no registrations.
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

// Info joins the registry path key with its stored fields for display.
type Info struct {
	Name      string
	Path      string
	CreatedAt time.Time
}

// List sorts by name so CLI output is stable despite map iteration order.
func (w *Workspaces) List(s *config.Settings) []Info {
	if s == nil {
		return nil
	}
	out := make([]Info, 0, len(s.Workspaces))
	for path, ws := range s.Workspaces {
		out = append(out, Info{Name: ws.Name, Path: path, CreatedAt: ws.CreatedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (w *Workspaces) cacheDir(name string) string {
	return filepath.Join(w.baseDir, config.WorkspacesDir, name)
}

// Init locks the read-modify-write so concurrent registrations cannot lose
// entries. pwd must be absolute and resolved.
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

// Remove updates the registry under lock and returns the cache path for
// deletion afterward. WithLock skips Save on a miss and exposes the path
// computed inside the lock. The CLI replaces ErrNotRegistered's generic text.
func (w *Workspaces) Remove(name string) (cacheDir string, err error) {
	err = config.WithLock(w.baseDir, func() error {
		s, err := config.Load(w.baseDir)
		if err != nil {
			return err
		}
		// Registry keys are paths, while the CLI accepts names.
		var foundKey string
		for key, ws := range s.Workspaces {
			if ws.Name == name {
				foundKey = key
				break
			}
		}
		if foundKey == "" {
			return ErrNotRegistered
		}
		delete(s.Workspaces, foundKey)
		cacheDir = w.cacheDir(name)
		return config.Save(w.baseDir, s)
	})
	if err != nil {
		return "", err
	}
	return cacheDir, nil
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
