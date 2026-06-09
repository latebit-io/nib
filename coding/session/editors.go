package session

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/latebit-io/nib/engine/buffer"
	"github.com/latebit-io/nib/engine/openfile"
)

// Open-file lifecycle methods — opening, switching, reloading, deleting
// buffer-bound handles. State (openFiles map, activeOpenFile / activeFile,
// mu) lives on Session in session.go; this file groups the methods that
// mutate that state through the workflow-enforcing path.
//
// Session never holds a frontend editor controller; the TUI owns its own
// pool of editors keyed by canonical path and wraps the buffer-only
// [openfile.OpenFile] handles surfaced here.

// ErrEditPending is returned by SwitchTo when a file switch is blocked
// because an edit is pending approval or an animated edit is in progress.
var ErrEditPending = errors.New("cannot switch files while an edit is pending")

// ActiveFile returns the path of the currently active file.
// Safe to call from any goroutine — reads under mu.RLock to
// avoid racing with SwitchTo.
func (s *Session) ActiveFile() string {
	s.mu.RLock()
	f := s.activeFile
	s.mu.RUnlock()
	return f
}

// ActiveOpenFile returns the currently active open file. Safe to call
// from any goroutine — reads under mu.RLock to avoid racing with
// SwitchTo.
func (s *Session) ActiveOpenFile() *openfile.OpenFile {
	s.mu.RLock()
	of := s.activeOpenFile
	s.mu.RUnlock()
	return of
}

// OpenFiles returns the paths of all open files.
func (s *Session) OpenFiles() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	files := make([]string, 0, len(s.openFiles))
	for path := range s.openFiles {
		files = append(files, path)
	}
	return files
}

// OpenFileForPath returns the open file for a given path, or nil if not open.
func (s *Session) OpenFileForPath(path string) *openfile.OpenFile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.openFiles[s.CanonPath(path)]
}

// newOpenFile constructs an [openfile.OpenFile] around the given buffer.
// Session-internal open-file construction goes through this helper so any
// future cross-cutting concerns (provenance, metadata) flow through one
// site.
func (s *Session) newOpenFile(buf *buffer.Buffer) *openfile.OpenFile {
	return openfile.New(buf)
}

// SwitchTo switches the active open file to a different path. If the file
// is already open, switches to it. If not, opens it from disk. Does NOT
// cancel the agent or clear intent — multi-file work continues across
// switches. Returns ErrEditPending if an edit is awaiting approval or
// mid-animation. Returns an error if the file cannot be opened.
func (s *Session) SwitchTo(path string) error {
	canon := s.CanonPath(path)

	s.mu.Lock()

	// Block switching while an edit is pending approval.
	// PendingEdit covers the review phase; stagedEditFile covers the
	// post-PrepareApproval window before CompleteApproval/AbortApproval.
	if s.pendingEdit != nil || s.stagedEditFile != "" {
		s.mu.Unlock()
		return ErrEditPending
	}

	// Already active
	if canon == s.activeFile {
		s.mu.Unlock()
		return nil
	}

	// Check if already open
	if of, ok := s.openFiles[canon]; ok {
		s.activeOpenFile = of
		s.activeFile = canon
		s.mu.Unlock()
		return nil
	}

	// Open from disk
	absPath, err := s.resolvePath(path)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	buf, err := buffer.NewFromFile(absPath)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("open %s: %w", path, err)
	}
	of := openfile.New(buf)
	s.openFiles[canon] = of
	s.activeOpenFile = of
	s.activeFile = canon
	s.mu.Unlock()

	// Wire LSP sync for the newly opened file.
	s.wireBufferSync(of)

	return nil
}

// ReloadFile re-reads a file from disk, replacing the in-memory buffer content.
// If the file is not currently open, this is a no-op.
// Returns ErrEditPending if an edit is being reviewed or animated.
func (s *Session) ReloadFile(path string) error {
	canon := s.CanonPath(path)

	s.mu.Lock()
	if s.pendingEdit != nil || s.stagedEditFile != "" {
		s.mu.Unlock()
		return ErrEditPending
	}
	of, ok := s.openFiles[canon]
	s.mu.Unlock()

	if !ok {
		return nil // not open — nothing to reload
	}
	if err := of.Buf.ReloadFromDisk(); err != nil {
		return fmt.Errorf("reload %s: %w", path, err)
	}
	slog.Debug("file reloaded from disk", "path", canon)
	return nil
}

// DeleteFile removes a file or directory from disk and cleans up session state.
// Closes session bookkeeping for affected paths. The TUI is responsible for
// cleaning up its own editor pool entries via the matching navigate/close
// signals; Session no longer owns any UI state to release here.
func (s *Session) DeleteFile(path string) error {
	absPath, err := s.resolvePath(path)
	if err != nil {
		return err
	}

	canon := s.CanonPath(absPath)

	if canon == filepath.Clean(s.projectRoot) {
		return fmt.Errorf("cannot delete project root")
	}

	if canon == filepath.Join(filepath.Clean(s.projectRoot), ".project") || s.isProjectMeta(canon) {
		return fmt.Errorf("cannot delete project metadata: %s", path)
	}

	// Block deletion while an edit is pending approval or mid-animation,
	// same guard as SwitchTo. Approval state may reference the deleted path.
	s.mu.RLock()
	pending := s.pendingEdit != nil || s.stagedEditFile != ""
	s.mu.RUnlock()
	if pending {
		return ErrEditPending
	}

	if err := removeFromDisk(absPath, path); err != nil {
		return err
	}

	removed := s.cleanupDeletedPath(canon)
	for _, of := range removed {
		s.unwireBufferSync(of)
	}

	return nil
}

// removeFromDisk removes a file or directory from disk.
func removeFromDisk(absPath, displayPath string) error {
	info, err := os.Stat(absPath)
	if err != nil {
		return fmt.Errorf("delete %s: %w", displayPath, err)
	}
	if info.IsDir() {
		err = os.RemoveAll(absPath)
	} else {
		err = os.Remove(absPath)
	}
	if err != nil {
		return fmt.Errorf("delete %s: %w", displayPath, err)
	}
	return nil
}

// cleanupDeletedPath removes all session state (open files, context,
// modified) for the given canonical path and any children (if a directory
// was deleted). Returns removed open-file handles so the caller can
// unwire LSP sync for them.
func (s *Session) cleanupDeletedPath(canon string) []*openfile.OpenFile {
	dirPrefix := canon + string(filepath.Separator)
	matches := func(p string) bool {
		return p == canon || strings.HasPrefix(p, dirPrefix)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var removed []*openfile.OpenFile
	for p, of := range s.openFiles {
		if matches(p) {
			removed = append(removed, of)
			delete(s.openFiles, p)
			if s.activeFile == p {
				s.activeFile = ""
				s.activeOpenFile = nil
			}
		}
	}
	deleteMatching(s.modifiedFiles, matches)

	// Ensure s.activeOpenFile is never nil so callers (intent.go reads
	// activeOpenFile.Buf.Content() unconditionally) cannot panic on the
	// turn following a delete.
	if s.activeOpenFile == nil {
		for p, of := range s.openFiles {
			s.activeOpenFile = of
			s.activeFile = p
			break
		}
		if s.activeOpenFile == nil {
			of := openfile.New(buffer.New())
			s.activeOpenFile = of
			// Track the empty handle so external observers see a
			// consistent map.
			s.openFiles[""] = of
			s.activeFile = ""
		}
	}
	return removed
}

// isProjectMeta returns true if the canonical path is inside .project/.
// Used as a guard against deleting project metadata files via DeleteFile.
func (s *Session) isProjectMeta(canon string) bool {
	prefix := filepath.Join(s.projectRoot, ".project") + string(filepath.Separator)
	return strings.HasPrefix(canon, prefix)
}

// deleteMatching removes all entries from a map whose keys satisfy pred.
func deleteMatching(m map[string]bool, pred func(string) bool) {
	for k := range m {
		if pred(k) {
			delete(m, k)
		}
	}
}

// openFileForEdit returns the open file targeted by the current pending
// edit. Falls back to the active open file if no path is set (backward
// compat). If the target file isn't open yet, auto-opens it from disk —
// the agent may have read the file via read_file (which doesn't create a
// buffer) and then proposed an edit_file on it.
//
// Callers must hold a non-nil s.pendingEdit — all public entry points
// (ReviewEdit, PrepareApproval) early-return before reaching here, so
// the nil case is not defended against.
func (s *Session) openFileForEdit() *openfile.OpenFile {
	path := s.pendingEdit.Path
	if path == "" {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.activeOpenFile
	}
	canon := s.CanonPath(path)

	s.mu.RLock()
	of, ok := s.openFiles[canon]
	s.mu.RUnlock()
	if ok {
		return of
	}

	// Auto-open: the agent proposed an edit to a file that isn't open yet.
	absPath, err := s.resolvePath(path)
	if err != nil {
		slog.Warn("openFileForEdit: resolve path failed", "path", path, "err", err)
		return nil
	}
	buf, err := buffer.NewFromFile(absPath)
	if err != nil {
		slog.Warn("openFileForEdit: open buffer failed", "path", path, "err", err)
		return nil
	}
	of = s.newOpenFile(buf)
	s.mu.Lock()
	s.openFiles[canon] = of
	s.mu.Unlock()

	// Wire LSP sync for auto-opened file.
	s.wireBufferSync(of)

	return of
}
