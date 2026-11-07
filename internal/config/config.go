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

const (
	SettingsFile  = "settings.json"
	WorkspacesDir = "workspaces"
)

const (
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

// Load defaults empty shell and tmpfs settings; an unset image still requires
// an explicit choice from the user.
func Load(baseDir string) (*Settings, error) {
	path := filepath.Join(baseDir, SettingsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &Settings{
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

var bootstrapDirs = []string{
	"",
	"shared",
	WorkspacesDir,
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

// Bootstrap creates structural storage directories without seeding agent data.
// Existing content and settings.json are not touched.
func Bootstrap(baseDir string) error {
	for _, sub := range bootstrapDirs {
		dir := filepath.Join(baseDir, sub)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create dir %s: %w", dir, err)
		}
	}
	return nil
}
