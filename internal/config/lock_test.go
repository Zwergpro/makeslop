package config

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Concurrent Load→mutate→Save under the lock must not lose updates.
func TestWithLock_SerializesLoadSave(t *testing.T) {
	base := t.TempDir()

	seed := &Settings{
		Version:    CurrentVersion,
		Image:      DefaultImage,
		Shell:      DefaultShell,
		TmpDirSize: DefaultTmpDirSize,
		Workspaces: map[string]Workspace{},
	}
	if err := Save(base, seed); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make([]error, goroutines)

	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			errs[i] = WithLock(base, func() error {
				s, err := Load(base)
				if err != nil {
					return err
				}
				// MigratedVersion doubles as a monotone counter here.
				s.MigratedVersion++
				return Save(base, s)
			})
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: WithLock error: %v", i, err)
		}
	}

	final, err := Load(base)
	if err != nil {
		t.Fatalf("final Load: %v", err)
	}
	if final.MigratedVersion != goroutines {
		t.Errorf("MigratedVersion = %d, want %d (lost update detected)",
			final.MigratedVersion, goroutines)
	}
}

// The lock must be released even when fn errors, so a later acquisition succeeds.
func TestWithLock_ReleasesOnFnError(t *testing.T) {
	base := t.TempDir()

	sentinel := errors.New("sentinel error")

	err := WithLock(base, func() error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithLock error = %v, want sentinel", err)
	}

	err = WithLock(base, func() error { return nil })
	if err != nil {
		t.Fatalf("second WithLock: %v", err)
	}
}

// Sequential acquisitions in one goroutine are fine (nested calls would
// self-deadlock — see WithLock's no-nesting invariant).
func TestWithLock_SequentialAcquisitionsSucceed(t *testing.T) {
	base := t.TempDir()

	for i := 0; i < 3; i++ {
		if err := WithLock(base, func() error { return nil }); err != nil {
			t.Fatalf("sequential acquisition %d: %v", i, err)
		}
	}
}

// WithLock creates .settings.lock and leaves it in place (not cleaned up).
func TestWithLock_CreatesLockFile(t *testing.T) {
	base := t.TempDir()

	if err := WithLock(base, func() error { return nil }); err != nil {
		t.Fatalf("WithLock: %v", err)
	}

	path := filepath.Join(base, lockFile)
	if _, err := os.Stat(path); err != nil {
		t.Errorf("lock file %s not found after WithLock: %v", path, err)
	}
}

// WithLock must MkdirAll so callers needn't pre-create the base directory.
func TestWithLock_CreatesMissingBaseDir(t *testing.T) {
	parent := t.TempDir()
	base := filepath.Join(parent, "does", "not", "exist")

	if err := WithLock(base, func() error { return nil }); err != nil {
		t.Fatalf("WithLock on missing dir: %v", err)
	}

	if _, err := os.Stat(base); err != nil {
		t.Errorf("base dir not created by WithLock: %v", err)
	}
}
