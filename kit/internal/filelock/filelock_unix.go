//go:build unix

package filelock

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
)

// Lock blocks until it holds an exclusive flock on path (created 0600,
// parent 0700, if missing) and returns the release func. The kernel drops
// the lock if the process dies, so a crash never wedges other processes.
func Lock(path string) (unlock func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("filelock: create dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("filelock: open: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close() // the flock error is the one worth reporting
		return nil, fmt.Errorf("filelock: flock %s: %w", path, err)
	}
	return func() {
		// Closing the descriptor releases the lock even if LOCK_UN fails.
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
			slog.Debug("filelock: unlock", "path", path, "err", err)
		}
		if err := f.Close(); err != nil {
			slog.Debug("filelock: close", "path", path, "err", err)
		}
	}, nil
}
