//go:build unix

package files

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestWrite_PipeIsRefused: opening a pipe to write blocks until
// something reads it, so a pipe is refused without being opened.
func TestWrite_PipeIsRefused(t *testing.T) {
	root, tools := workspace(t)
	if err := syscall.Mkfifo(filepath.Join(root, "pkg", "pipe"), 0o644); err != nil {
		t.Skipf("no FIFO support: %v", err)
	}
	if res := run(t, tools["write_file"], map[string]any{"path": "pipe", "content": "x"}); !res.IsError {
		t.Error("a write to a pipe must be refused")
	}
}

// TestWrite_NewFileKeepsTheUmask: a new file is no more readable than
// the process umask allows. Not parallel: the umask is process-wide.
func TestWrite_NewFileKeepsTheUmask(t *testing.T) {
	root, tools := workspace(t)
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	ok(t, tools["write_file"], map[string]any{"path": "private.txt", "content": "x"})
	info, err := os.Stat(filepath.Join(root, "pkg", "private.txt"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, %v; want 0600 under umask 077", info.Mode().Perm(), err)
	}
}
