//go:build unix

package skill

import (
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

// TestReadResource_FIFO: a FIFO with no writer is neither listed nor
// allowed to block a read.
func TestReadResource_FIFO(t *testing.T) {
	root := t.TempDir()
	s := bundle(t, root, "pdf")
	if err := syscall.Mkfifo(filepath.Join(root, "pdf", "pipe"), 0o644); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}

	files, _, err := bundled(s.Dir)
	if err != nil || slices.Contains(files, "pipe") {
		t.Errorf("bundled = %v, %v; a FIFO must not be listed", files, err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := s.ReadResource("pipe")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a FIFO must be refused as not a regular file")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadResource blocked on a FIFO with no writer")
	}
}
