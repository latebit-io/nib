package ui

import (
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/latebit-io/junto/engine/session"
)

// fileChangedMsg is sent when a watched file changes on disk.
type fileChangedMsg struct{ Path string }

// FileWatcher monitors open files for external changes and delivers
// fileChangedMsg events through a channel that Bubble Tea can poll.
type FileWatcher struct {
	watcher *fsnotify.Watcher
	session *session.Session
	ch      chan fileChangedMsg

	mu            sync.Mutex
	watchingFiles map[string]bool // canonical file paths to react to
	watchingDirs  map[string]bool // parent dirs registered with fsnotify
	closed        bool
}

// NewFileWatcher creates a watcher that monitors open editor files.
// Returns nil if the underlying OS watcher cannot be created.
func NewFileWatcher(sess *session.Session) *FileWatcher {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Warn("file watcher unavailable", "err", err)
		return nil
	}
	fw := &FileWatcher{
		watcher:       w,
		session:       sess,
		ch:            make(chan fileChangedMsg, 16),
		watchingFiles: make(map[string]bool),
		watchingDirs:  make(map[string]bool),
	}
	go fw.loop()
	return fw
}

// debounceDelay is the time to wait after the last write event before
// emitting a change notification. Editors and tools often write files
// in multiple steps (truncate + write, or rename + write).
const debounceDelay = 100 * time.Millisecond

// loop reads fsnotify events and debounces writes per path.
func (fw *FileWatcher) loop() {
	timers := make(map[string]*time.Timer)
	for {
		select {
		case ev, ok := <-fw.watcher.Events:
			if !ok {
				return
			}
			if !ev.Has(fsnotify.Write) && !ev.Has(fsnotify.Create) {
				continue
			}
			canon := fw.session.CanonPath(ev.Name)

			// Only emit for files we're explicitly tracking.
			fw.mu.Lock()
			tracked := fw.watchingFiles[canon]
			fw.mu.Unlock()
			if !tracked {
				continue
			}

			// Cancel previous timer for this path (debounce).
			if t, exists := timers[canon]; exists {
				t.Stop()
			}
			timers[canon] = time.AfterFunc(debounceDelay, func() {
				fw.mu.Lock()
				defer fw.mu.Unlock()
				if fw.closed {
					return
				}
				select {
				case fw.ch <- fileChangedMsg{Path: canon}:
				default:
					slog.Warn("file change notification dropped (channel full)", "path", canon)
				}
			})

		case err, ok := <-fw.watcher.Errors:
			if !ok {
				return
			}
			slog.Warn("file watcher error", "err", err)
		}
	}
}

// Watch adds a file path to the watch list. Registers the parent directory
// with fsnotify (not the file itself) so that atomic-save flows
// (write temp + rename) are detected correctly.
// Safe to call multiple times with the same path.
func (fw *FileWatcher) Watch(path string) {
	canon := fw.session.CanonPath(path)
	dir := filepath.Dir(canon)
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if fw.closed || fw.watchingFiles[canon] {
		return
	}
	if !fw.watchingDirs[dir] {
		if err := fw.watcher.Add(dir); err != nil {
			slog.Warn("watch failed", "dir", dir, "err", err)
			return
		}
		fw.watchingDirs[dir] = true
	}
	fw.watchingFiles[canon] = true
}

// Changes returns the channel that delivers file change notifications.
func (fw *FileWatcher) Changes() <-chan fileChangedMsg {
	return fw.ch
}

// Close shuts down the file watcher and unblocks any consumer waiting on Changes().
func (fw *FileWatcher) Close() {
	fw.mu.Lock()
	if fw.closed {
		fw.mu.Unlock()
		return
	}
	fw.closed = true
	fw.mu.Unlock()
	if err := fw.watcher.Close(); err != nil {
		slog.Warn("close file watcher", "err", err)
	}
	close(fw.ch)
}
