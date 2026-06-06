package config

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestWithLock_SerializesLoadSave verifies that two goroutines incrementing a
// counter via Load→mutate→Save under the lock both persist without a lost update.
func TestWithLock_SerializesLoadSave(t *testing.T) {
	base := t.TempDir()

	// Seed an initial settings file.
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
				// Use MigratedVersion as a monotone counter proxy.
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

// TestWithLock_ReleasesOnFnError verifies that the lock is released when fn
// returns an error, so a subsequent sequential acquisition succeeds.
func TestWithLock_ReleasesOnFnError(t *testing.T) {
	base := t.TempDir()

	sentinel := errors.New("sentinel error")

	// First acquisition: fn returns an error.
	err := WithLock(base, func() error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithLock error = %v, want sentinel", err)
	}

	// Second acquisition: must succeed (lock was released by the first).
	err = WithLock(base, func() error { return nil })
	if err != nil {
		t.Fatalf("second WithLock: %v", err)
	}
}

// TestWithLock_SequentialAcquisitionsSucceed verifies that back-to-back
// sequential acquisitions in the same goroutine both succeed. This documents
// the no-nesting boundary: sequential calls are fine; nested calls would
// self-deadlock.
func TestWithLock_SequentialAcquisitionsSucceed(t *testing.T) {
	base := t.TempDir()

	for i := 0; i < 3; i++ {
		if err := WithLock(base, func() error { return nil }); err != nil {
			t.Fatalf("sequential acquisition %d: %v", i, err)
		}
	}
}

// TestWithLock_CreatesLockFile verifies that WithLock creates the .settings.lock
// file as a side effect, and that the lock file persists (not cleaned up).
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

// TestWithLock_CreatesMissingBaseDir verifies that WithLock calls MkdirAll so
// callers don't need to pre-create the base directory.
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
