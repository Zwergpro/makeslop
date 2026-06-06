package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

const lockFile = ".settings.lock"

// inProcessMu serializes WithLock callers within the same process.
// flock(2) provides cross-process mutual exclusion but does NOT block
// concurrent flock calls from different file descriptors within the same
// process (Linux kernel behavior). inProcessMu fills that gap so that
// goroutines within this binary are also serialized.
var inProcessMu sync.Mutex

// WithLock takes an exclusive advisory lock on <baseDir>/.settings.lock
// and calls fn while holding the lock. The lock is released (and the fd
// closed) when fn returns, whether or not it returns an error.
//
// The locking is two-level:
//   - An in-process sync.Mutex serializes goroutines within the same binary
//     (flock(2) does not block same-process callers on Linux).
//   - A POSIX flock(LOCK_EX) guards against concurrent invocations from
//     separate processes (e.g. two concurrent `makeslop init` shells).
//
// Both layers are released before WithLock returns.
//
// NO-NESTING INVARIANT: WithLock MUST NOT be nested. Each Load→mutate→Save
// site acquires its own short-lived lock sequentially; no caller may wrap
// another WithLock-protected call inside fn. A nested call in the same
// goroutine would deadlock on inProcessMu.
func WithLock(baseDir string, fn func() error) error {
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return fmt.Errorf("create base dir %s: %w", baseDir, err)
	}

	// Layer 1: in-process goroutine serialization.
	inProcessMu.Lock()
	defer inProcessMu.Unlock()

	// Layer 2: cross-process advisory flock.
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
