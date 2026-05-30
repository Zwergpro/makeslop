package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Zwergpro/makeslop/internal/assets"
)

// migration describes a single idempotent migration step. All steps in the
// migrations slice are run (unconditionally) whenever the persisted
// migrated_version differs from MigrationVersion.
type migration struct {
	name string
	run  func(baseDir string) error
}

// migrations is the ordered list of migration steps. Append new steps here and
// bump MigrationVersion in config.go when adding or changing a step.
//
// INVARIANT: every step must be idempotent. When MigratedVersion differs from
// MigrationVersion (including a binary downgrade that later re-upgrades), all
// steps are re-run in full — there is no per-step skip logic.
var migrations = []migration{
	{name: DockerfileFile, run: writeDockerfile},
}

// writeDockerfile atomically writes the embedded assets.Dockerfile to
// <baseDir>/Dockerfile, always overwriting any existing file. The write uses
// a temp-file + rename pattern so a crash mid-write cannot leave a partial
// file behind.
func writeDockerfile(baseDir string) error {
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return fmt.Errorf("create base dir %s: %w", baseDir, err)
	}

	tmp, err := os.CreateTemp(baseDir, DockerfileFile+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp Dockerfile: %w", err)
	}
	tmpName := tmp.Name()
	// Cleared once rename succeeds; otherwise the defer removes the temp file.
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(assets.Dockerfile); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp Dockerfile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp Dockerfile: %w", err)
	}

	final := filepath.Join(baseDir, DockerfileFile)
	if err := os.Rename(tmpName, final); err != nil {
		return fmt.Errorf("rename Dockerfile into place: %w", err)
	}
	// Rename succeeded; the temp path no longer exists, so the deferred
	// remove must not fire even if Chmod fails.
	cleanup = false
	// Ensure final file has the expected permission (temp file inherits umask).
	if err := os.Chmod(final, 0o644); err != nil {
		return fmt.Errorf("chmod Dockerfile: %w", err)
	}
	return nil
}

// Migrate runs all migration steps when the persisted migrated_version in
// <baseDir>/settings.json differs from MigrationVersion. It returns
// (true, nil) when migrations were applied and (false, nil) when already
// up to date. An error is returned only if a migration step or the subsequent
// Save fails.
func Migrate(baseDir string) (applied bool, err error) {
	s, err := Load(baseDir)
	if err != nil {
		return false, fmt.Errorf("migrate load settings: %w", err)
	}
	if s.MigratedVersion >= MigrationVersion {
		return false, nil
	}

	for _, m := range migrations {
		if runErr := m.run(baseDir); runErr != nil {
			return false, fmt.Errorf("migrate %q: %w", m.name, runErr)
		}
	}

	s.MigratedVersion = MigrationVersion
	if err := Save(baseDir, s); err != nil {
		return false, fmt.Errorf("migrate save settings: %w", err)
	}
	return true, nil
}
