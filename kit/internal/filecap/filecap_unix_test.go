//go:build unix

package filecap

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
)

// TestRead_FIFOIsRefused: a pipe with no writer is refused at once; a
// blocking open would wait for a writer that never comes.
func TestRead_FIFOIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Skipf("no FIFO support: %v", err)
	}
	if _, err := Read(path, 1<<20, "test"); !errors.Is(err, ErrNotRegular) {
		t.Errorf("Read = %v, want ErrNotRegular", err)
	}
	var v struct{}
	if found, err := ReadJSON(path, 1<<20, "test", &v); found || !errors.Is(err, ErrNotRegular) {
		t.Errorf("ReadJSON = %v, %v; want ErrNotRegular", found, err)
	}
	if found, err := ReadJSON(filepath.Join(t.TempDir(), "none.json"), 1<<20, "test", &v); found || err != nil {
		t.Errorf("missing file: %v, %v; want not found and no error", found, err)
	}
}
