package ui

import (
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// fileChangedMsg is sent when a watched file changes on disk.
type fileChangedMsg struct{ Path string }

// FileWatcher monitors open files for external changes and delivers
// fileChangedMsg events through a channel that Bubble Tea can poll.
//
// Directories are watched (not individual files) so that atomic-save
// flows (write temp + rename) are detected. A reference count per
// directory tracks how many watched files live there; when the last
// file in a directory is unwatched, the directory watch is removed.
type FileWatcher struct {
	watcher *fsnotify.Watcher
	canon   func(string) string
	ch      chan fileChangedMsg

	mu            sync.Mutex
	watchingFiles map[string]bool        // canonical file paths to react to
	dirRefCount   map[string]int         // parent dir → number of watched files in it
	timers        map[string]*time.Timer // pending debounce timers per file path
	closed        bool
}

// NewFileWatcher creates a watcher that monitors open editor files.
// canon normalizes paths (symlinks, case) so fsnotify event names and
// Watch/Unwatch arguments compare equal; typically session.CanonPath.
// Returns nil if the underlying OS watcher cannot be created.
func NewFileWatcher(canon func(string) string) *FileWatcher {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Warn("file watcher unavailable", "err", err)
		return nil
	}
	fw := &FileWatcher{
		watcher:       w,
		canon:         canon,
		ch:            make(chan fileChangedMsg, 16),
		watchingFiles: make(map[string]bool),
		dirRefCount:   make(map[string]int),
		timers:        make(map[string]*time.Timer),
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
	for {
		select {
		case ev, ok := <-fw.watcher.Events:
			if !ok {
				return
			}
			if !ev.Has(fsnotify.Write) && !ev.Has(fsnotify.Create) {
				continue
			}
			canon := fw.canon(ev.Name)

			fw.mu.Lock()
			// Only emit for files we're explicitly tracking.
			if !fw.watchingFiles[canon] {
				fw.mu.Unlock()
				continue
			}
			// Cancel previous timer for this path (debounce).
			if t, exists := fw.timers[canon]; exists {
				t.Stop()
			}
			fw.timers[canon] = time.AfterFunc(debounceDelay, func() {
				fw.mu.Lock()
				defer fw.mu.Unlock()
				delete(fw.timers, canon)
				if fw.closed || !fw.watchingFiles[canon] {
					return
				}
				select {
				case fw.ch <- fileChangedMsg{Path: canon}:
				default:
					slog.Warn("file change notification dropped (channel full)", "path", canon)
				}
			})
			fw.mu.Unlock()

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
	canon := fw.canon(path)
	dir := filepath.Dir(canon)
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if fw.closed || fw.watchingFiles[canon] {
		return
	}
	if fw.dirRefCount[dir] == 0 {
		if err := fw.watcher.Add(dir); err != nil {
			slog.Warn("watch failed", "dir", dir, "err", err)
			return
		}
	}
	fw.dirRefCount[dir]++
	fw.watchingFiles[canon] = true
}

// Unwatch removes a file from the watch list. When the last watched file
// in a directory is removed, the directory watch is also removed.
// Safe to call for paths that were never watched.
func (fw *FileWatcher) Unwatch(path string) {
	canon := fw.canon(path)
	dir := filepath.Dir(canon)
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if timer, ok := fw.timers[canon]; ok {
		timer.Stop()
		delete(fw.timers, canon)
	}
	if !fw.watchingFiles[canon] {
		return
	}
	delete(fw.watchingFiles, canon)
	fw.dirRefCount[dir]--
	if fw.dirRefCount[dir] <= 0 {
		delete(fw.dirRefCount, dir)
		if !fw.closed {
			if err := fw.watcher.Remove(dir); err != nil {
				slog.Warn("unwatch dir failed", "dir", dir, "err", err)
			}
		}
	}
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
