//go:build unix

package filelock

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLockExcludesSecondHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "f.lock")
	unlock, err := Lock(path)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	acquired := make(chan func(), 1)
	go func() {
		u, err := Lock(path) // separate descriptor: blocks like another process would
		if err != nil {
			t.Errorf("second Lock: %v", err)
			acquired <- func() {}
			return
		}
		acquired <- u
	}()
	select {
	case <-acquired:
		t.Fatal("second holder acquired the lock while the first held it")
	case <-time.After(50 * time.Millisecond):
	}

	unlock()
	select {
	case u := <-acquired:
		u()
	case <-time.After(5 * time.Second):
		t.Fatal("second holder never acquired the lock after release")
	}
}
