package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

const lockFile = ".settings.lock"

// inProcessMu serializes same-process callers: flock(2) does NOT block
// concurrent flock calls from different fds within one process (Linux kernel
// behavior), so this mutex covers goroutines within this binary.
var inProcessMu sync.Mutex

// Update skips Save when mutate fails, leaving settings intact. Its callback
// must not call WithLock: the nested lock would deadlock on inProcessMu.
func Update(baseDir string, mutate func(*Settings) error) error {
	return WithLock(baseDir, func() error {
		s, err := Load(baseDir)
		if err != nil {
			return err
		}
		if err := mutate(s); err != nil {
			return err
		}
		return Save(baseDir, s)
	})
}

// WithLock guards settings across processes and goroutines. Never nest it;
// inProcessMu is not reentrant.
func WithLock(baseDir string, fn func() error) error {
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return fmt.Errorf("create base dir %s: %w", baseDir, err)
	}

	inProcessMu.Lock()
	defer inProcessMu.Unlock()

	path := filepath.Join(baseDir, lockFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open lock file %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("acquire lock %s: %w", path, err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

	return fn()
}
