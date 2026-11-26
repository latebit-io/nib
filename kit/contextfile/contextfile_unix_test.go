//go:build unix

package contextfile

import (
	"path/filepath"
	"syscall"
	"testing"
)

// TestLoadChain_FIFOIsRefused: a pipe named AGENTS.md is reported
// instead of holding the session up waiting for a writer.
func TestLoadChain_FIFOIsRefused(t *testing.T) {
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "AGENTS.md"), 0o644); err != nil {
		t.Skipf("no FIFO support: %v", err)
	}
	files, err := LoadChain(root, root)
	if err == nil || len(files) != 0 {
		t.Errorf("LoadChain = %v, %v; want the pipe refused", files, err)
	}
}
