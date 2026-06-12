// Package config manages global settings (settings.json) and one-shot agent
// directory bootstrap under ~/.makeslop.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/Zwergpro/makeslop/internal/assets"
)

const (
	SettingsFile   = "settings.json"
	WorkspacesDir  = "workspaces"
	DockerfileFile = "Dockerfile"

	// ConfigVersion is the single version governing both the settings schema and the
	// one-shot ~/.makeslop asset refresh. Bump when the embedded assets OR the Settings
	// shape change; `migrate` re-runs all idempotent steps and re-stamps.
	ConfigVersion = 1
)

// omitempty + Load-time defaulting keeps pre-existing files byte-stable until a user overrides.
const (
	DefaultImage      = "claudebox"
	DefaultShell      = "/bin/zsh"
	DefaultTmpDirSize = "100m"
)

type Workspace struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// Settings is the persisted shape of <baseDir>/settings.json. Workspaces is
// keyed by absolute, symlink-evaluated workspace root paths.
type Settings struct {
	Version    int                  `json:"version"`
	Image      string               `json:"image,omitempty"`
	Shell      string               `json:"shell,omitempty"`
	TmpDirSize string               `json:"tmp_dir_size,omitempty"`
	Workspaces map[string]Workspace `json:"workspaces"`
}

func DefaultBaseDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".makeslop"), nil
}

// Load reads <baseDir>/settings.json. A missing file yields default Settings
// (not an error); malformed JSON is an error. Empty Image/Shell/TmpDirSize
// default for backward compatibility.
func Load(baseDir string) (*Settings, error) {
	path := filepath.Join(baseDir, SettingsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &Settings{
				Image:      DefaultImage,
				Shell:      DefaultShell,
				TmpDirSize: DefaultTmpDirSize,
				Workspaces: map[string]Workspace{},
			}, nil
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
	if s.Image == "" {
		s.Image = DefaultImage
	}
	if s.Shell == "" {
		s.Shell = DefaultShell
	}
	if s.TmpDirSize == "" {
		s.TmpDirSize = DefaultTmpDirSize
	}
	return &s, nil
}

// Save atomically writes settings via temp-file + intra-dir rename so a crash
// mid-write cannot leave a half-written settings.json behind.
func Save(baseDir string, s *Settings) error {
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return fmt.Errorf("create base dir %s: %w", baseDir, err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(baseDir, SettingsFile+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp settings file: %w", err)
	}
	tmpName := tmp.Name()
	// Cleared once rename succeeds; otherwise the defer removes the temp file.
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

	final := filepath.Join(baseDir, SettingsFile)
	if err := os.Rename(tmpName, final); err != nil {
		return fmt.Errorf("rename settings file into place: %w", err)
	}
	cleanup = false
	return nil
}

// bootstrapFile writes content to path via temp-file+rename so a failed write
// never leaves a partial file. A no-op (idempotent) if path already exists.
func bootstrapFile(path string, content []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file for %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file for %s: %w", path, err)
	}

	// Existence-check then Rename (not Link): os.Link fails with EPERM on
	// overlayfs and other filesystems (Docker, CI, NFS); checking first
	// preserves the idempotent no-overwrite contract.
	if _, err := os.Lstat(path); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		// Real Lstat failure (e.g. EACCES on parent) — surface it rather than
		// fall through to Rename and produce a misleading message.
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("install %s: %w", path, err)
	}
	cleanup = false
	// Normalise to 0o644 regardless of the process umask.
	if err := os.Chmod(path, 0o644); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}

var bootstrapDirs = []string{
	"",
	".codex",
	".claude",
	WorkspacesDir,
}

var bootstrapFiles = []struct {
	name    string
	content []byte
}{
	{".claude.json", []byte("{}\n")},
	{DockerfileFile, assets.Dockerfile},
}

// BaseConfigExists reports whether <baseDir>/settings.json exists. Returns
// (false, nil) when absent and (false, err) for any other stat failure, so
// callers can distinguish "not initialised" from "unreadable".
func BaseConfigExists(baseDir string) (bool, error) {
	path := filepath.Join(baseDir, SettingsFile)
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("stat %s: %w", path, err)
}

// Bootstrap is idempotent: creates the agent directories and seed files under
// baseDir, never overwriting existing content. settings.json is not touched.
func Bootstrap(baseDir string) error {
	for _, sub := range bootstrapDirs {
		dir := filepath.Join(baseDir, sub)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create dir %s: %w", dir, err)
		}
	}
	for _, f := range bootstrapFiles {
		path := filepath.Join(baseDir, f.name)
		if err := bootstrapFile(path, f.content); err != nil {
			return err
		}
	}
	return nil
}
