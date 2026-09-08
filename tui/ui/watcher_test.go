package ui

import (
	"path/filepath"
	"testing"
	"time"
)

func TestFileWatcherUnwatchCancelsPendingTimer(t *testing.T) {
	fw := NewFileWatcher(filepath.Clean)
	if fw == nil {
		t.Fatal("NewFileWatcher returned nil")
	}
	t.Cleanup(fw.Close)

	path := filepath.Join(t.TempDir(), "watched.go")
	fw.Watch(path)

	timer := time.AfterFunc(time.Hour, func() {})
	t.Cleanup(func() { timer.Stop() })
	fw.mu.Lock()
	fw.timers[path] = timer
	fw.mu.Unlock()

	fw.Unwatch(path)

	fw.mu.Lock()
	_, exists := fw.timers[path]
	fw.mu.Unlock()
	if exists {
		t.Error("pending timer retained after Unwatch")
	}
	if timer.Stop() {
		t.Error("pending timer remained active after Unwatch")
	}
}
