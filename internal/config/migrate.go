package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Zwergpro/makeslop/internal/assets"
)

// migration is a single idempotent migration step.
type migration struct {
	name string
	run  func(baseDir string) error
}

// migrations is the ordered list of steps. Append here and bump MigrationVersion
// in config.go when adding or changing a step.
//
// INVARIANT: every step must be idempotent. When MigratedVersion differs from
// MigrationVersion all steps re-run in full — there is no per-step skip logic.
var migrations = []migration{
	{name: DockerfileFile, run: WriteDockerfile},
}

// WriteDockerfile atomically writes the embedded assets.Dockerfile to
// <baseDir>/Dockerfile, always overwriting any existing file (temp-file+rename).
// Also called by `build --refresh` to reset a hand-edited Dockerfile without
// running a migration or touching MigratedVersion.
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
	// Rename consumed the temp path; the deferred remove must not fire even if
	// Chmod fails below.
	cleanup = false
	// Normalise to 0o644 (temp file inherits umask).
	if err := os.Chmod(final, 0o644); err != nil {
		return fmt.Errorf("chmod Dockerfile: %w", err)
	}
	return nil
}

// MigrationStatus returns s.MigratedVersion, the latest MigrationVersion, and
// whether the config is stale (current < latest).
func MigrationStatus(s *Settings) (current, latest int, stale bool) {
	current = s.MigratedVersion
	latest = MigrationVersion
	stale = current < latest
	return current, latest, stale
}

// Migrate runs all migration steps when migrated_version differs from
// MigrationVersion, returning (true, nil) when applied and (false, nil) when
// already up to date.
//
// The Load→stamp→Save sequence is protected by WithLock so a concurrent init
// or config set cannot lose the MigratedVersion stamp. The steps themselves
// run outside the lock because they are idempotent and don't touch settings.json.
func Migrate(baseDir string) (applied bool, err error) {
	// Best-effort early skip; the definitive check is under the lock below.
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

	var stamped bool
	lockErr := WithLock(baseDir, func() error {
		// Re-load under the lock so we don't clobber concurrent workspace edits.
		fresh, loadErr := Load(baseDir)
		if loadErr != nil {
			return fmt.Errorf("migrate load settings: %w", loadErr)
		}
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
