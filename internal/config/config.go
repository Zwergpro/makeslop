// Package config owns the makeslop-global configuration stored under
// ~/.makeslop: the settings.json file (versioned shape plus workspaces
// registry payload) and the one-shot bootstrap of agent dirs/files
// (.codex/, .claude/, .claude.json, workspaces/).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Exported so internal/workspace can join paths against the same well-known
// names and stamp the same version on freshly created Settings.
const (
	SettingsFile   = "settings.json"
	WorkspacesDir  = "workspaces"
	CurrentVersion = 1
)

// omitempty + Load-time defaulting keeps pre-existing files byte-stable until a user overrides.
const (
	DefaultImage = "claudebox"
	DefaultShell = "/bin/zsh"
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
	Workspaces map[string]Workspace `json:"workspaces"`
}

func DefaultBaseDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".makeslop"), nil
}

// Load reads <baseDir>/settings.json. A missing file yields an empty Settings
// at CurrentVersion (not an error); malformed JSON is wrapped. Empty Image/Shell
// fields are populated from DefaultImage/DefaultShell so pre-existing configs
// that predate those fields keep working without an explicit migration step.
func Load(baseDir string) (*Settings, error) {
	path := filepath.Join(baseDir, SettingsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &Settings{
				Version:    CurrentVersion,
				Image:      DefaultImage,
				Shell:      DefaultShell,
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
	data = append(data, '\n') // POSIX-friendly trailing newline

	tmp, err := os.CreateTemp(baseDir, SettingsFile+".tmp-*")
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

	final := filepath.Join(baseDir, SettingsFile)
	if err := os.Rename(tmpName, final); err != nil {
		return fmt.Errorf("rename settings file into place: %w", err)
	}
	cleanup = false
	return nil
}

// Files are created with O_EXCL so concurrent runs and pre-existing user
// edits both degrade to no-ops rather than clobbering.
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
}

// Bootstrap is idempotent: it creates the makeslop-managed agent directories
// (.codex/, .claude/, workspaces/) and seed files (.claude.json = "{}\n")
// under baseDir if they don't already exist. It never overwrites a pre-existing
// file — O_EXCL + EEXIST-as-success guards user edits. settings.json is NOT
// touched here; the workspace registry's Save owns that file.
func Bootstrap(baseDir string) error {
	for _, sub := range bootstrapDirs {
		dir := filepath.Join(baseDir, sub)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create dir %s: %w", dir, err)
		}
	}
	for _, f := range bootstrapFiles {
		path := filepath.Join(baseDir, f.name)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return fmt.Errorf("create %s: %w", path, err)
		}
		_, writeErr := file.Write(f.content)
		closeErr := file.Close()
		if writeErr != nil {
			return fmt.Errorf("write %s: %w", path, writeErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close %s: %w", path, closeErr)
		}
	}
	return nil
}
