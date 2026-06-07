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
	{name: DockerfileFile, run: WriteDockerfile},
}

// WriteDockerfile atomically writes the embedded assets.Dockerfile to
// <baseDir>/Dockerfile, always overwriting any existing file. The write uses
// a temp-file + rename pattern so a crash mid-write cannot leave a partial
// file behind. It is also called by `build --refresh` to reset a hand-edited
// base Dockerfile to the shipped version WITHOUT running a migration or
// touching MigratedVersion.
func WriteDockerfile(baseDir string) error {
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

// MigrationStatus returns the current migrated version stored in s, the latest
// known MigrationVersion constant, and whether the config is stale (current <
// latest). A freshly-loaded Settings with MigratedVersion == 0 is always stale
// when MigrationVersion > 0.
func MigrationStatus(s *Settings) (current, latest int, stale bool) {
	current = s.MigratedVersion
	latest = MigrationVersion
	stale = current < latest
	return current, latest, stale
}

// Migrate runs all migration steps when the persisted migrated_version in
// <baseDir>/settings.json differs from MigrationVersion. It returns
// (true, nil) when migrations were applied and (false, nil) when already
// up to date. An error is returned only if a migration step or the subsequent
// Save fails.
//
// The Load→stamp→Save sequence is protected by WithLock so a concurrent
// makeslop init or config set cannot lose the MigratedVersion stamp.
// The migration steps themselves (e.g. WriteDockerfile) run outside the lock
// because they are idempotent and do not touch settings.json.
func Migrate(baseDir string) (applied bool, err error) {
	// Quick check outside the lock: if already up to date, skip immediately.
	// This is a best-effort optimisation; the definitive check is under the lock.
	s, err := Load(baseDir)
	if err != nil {
		return false, fmt.Errorf("migrate load settings: %w", err)
	}
	if s.MigratedVersion >= MigrationVersion {
		return false, nil
	}

	// Run the migration steps (idempotent; do not touch settings.json).
	for _, m := range migrations {
		if runErr := m.run(baseDir); runErr != nil {
			return false, fmt.Errorf("migrate %q: %w", m.name, runErr)
		}
	}

	// Stamp the new version under the lock to avoid a lost-update race with a
	// concurrent init re-stamp or config set.
	var stamped bool
	lockErr := WithLock(baseDir, func() error {
		// Re-load under the lock so we don't clobber concurrent workspace edits.
		fresh, loadErr := Load(baseDir)
		if loadErr != nil {
			return fmt.Errorf("migrate load settings: %w", loadErr)
		}
		// If a concurrent migration already stamped the version, nothing to do.
		if fresh.MigratedVersion >= MigrationVersion {
			return nil
		}
		fresh.MigratedVersion = MigrationVersion
		if saveErr := Save(baseDir, fresh); saveErr != nil {
			return fmt.Errorf("migrate save settings: %w", saveErr)
		}
		stamped = true
		return nil
	})
	if lockErr != nil {
		return false, lockErr
	}
	return stamped, nil
}
