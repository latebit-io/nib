package session

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/latebit-io/nib/engine/buffer"
	"github.com/latebit-io/nib/engine/editor"
)

// Editor lifecycle methods — opening, switching, reloading, deleting buffers,
// plus the highlighter-decoration helpers that every constructor path runs
// through. State (editors map, activeEditor/activeFile, mu) lives on Session
// in session.go; this file groups the methods that mutate that state through
// the workflow-enforcing path.

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

// ActiveEditor returns the currently active editor. Safe to call from
// any goroutine — reads under mu.RLock to avoid racing with SwitchTo.
func (s *Session) ActiveEditor() *editor.Editor {
	s.mu.RLock()
	e := s.activeEditor
	s.mu.RUnlock()
	return e
}

// OpenFiles returns the paths of all open editors.
func (s *Session) OpenFiles() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	files := make([]string, 0, len(s.editors))
	for path := range s.editors {
		files = append(files, path)
	}
	return files
}

// EditorForPath returns the editor for a given path, or nil if not open.
func (s *Session) EditorForPath(path string) *editor.Editor {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.editors[s.CanonPath(path)]
}

// newEditor constructs an editor and installs a highlighter when a factory
// has been configured. Session-internal editor construction goes through
// this helper so every editor the session manages is decorated uniformly.
//
// Caller must NOT hold s.mu — decorateEditor takes RLock. Use editor.New
// directly at sites that hold s.mu (write), then call decorateEditor after
// releasing it.
func (s *Session) newEditor(buf *buffer.Buffer) *editor.Editor {
	e := editor.New(buf)
	s.decorateEditor(e)
	return e
}

// decorateEditor installs a highlighter on an already-constructed editor
// to match the session's current factory. Safe to call on a nil or
// path-less editor — it no-ops. Must be called with s.mu unlocked.
//
// Linearizable w.r.t. concurrent [SetHighlighterFactory] via a
// generation counter: read (factory, gen) under RLock, install the
// highlighter outside the lock, then re-read gen — if it changed, the
// factory was swapped mid-install and we retry with the new one. Each
// retry overwrites the previous highlighter (SetHighlighter closes the
// old one), so no resources leak.
func (s *Session) decorateEditor(e *editor.Editor) {
	if e == nil || e.Buf == nil || e.Buf.Path == "" {
		return
	}
	for {
		s.mu.RLock()
		factory := s.highlighterFactory
		gen := s.highlighterFactoryGen
		s.mu.RUnlock()

		if factory == nil {
			e.SetHighlighter(nil)
		} else {
			e.SetHighlighter(factory(e.Buf.Path))
		}

		s.mu.RLock()
		stable := gen == s.highlighterFactoryGen
		s.mu.RUnlock()
		if stable {
			return
		}
	}
}

// SwitchTo switches the active editor to a different file. If the file is
// already open, switches to it. If not, opens it from disk. Does NOT cancel
// the agent or clear intent — multi-file work continues across switches.
// Returns ErrEditPending if an edit is awaiting approval or mid-animation.
// Returns an error if the file cannot be opened.
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
		if !s.isProjectMeta(canon) && !s.InContext(canon) {
			s.AddContext(canon)
		}
		return nil
	}

	// Check if already open
	if e, ok := s.editors[canon]; ok {
		s.activeEditor = e
		s.activeFile = canon
		s.mu.Unlock()
		if !s.isProjectMeta(canon) && !s.InContext(canon) {
			s.AddContext(canon)
		}
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
	e := editor.New(buf)
	s.editors[canon] = e
	addToContext := !s.isProjectMeta(canon)
	if addToContext {
		s.contextSet[canon] = true
	}
	s.activeEditor = e
	s.activeFile = canon
	s.mu.Unlock()

	// Decorate after unlock — decorateEditor takes s.mu.RLock.
	s.decorateEditor(e)

	// Wire LSP sync for the new editor.
	s.wireBufferSync(e)

	if addToContext {
		s.saveContext()
	}
	return nil
}

// ReloadFile re-reads a file from disk, replacing the in-memory buffer content.
// If the file is not currently open in the editors map, this is a no-op.
// Returns ErrEditPending if an edit is being reviewed or animated.
func (s *Session) ReloadFile(path string) error {
	canon := s.CanonPath(path)

	s.mu.Lock()
	if s.pendingEdit != nil || s.stagedEditFile != "" {
		s.mu.Unlock()
		return ErrEditPending
	}
	e, ok := s.editors[canon]
	s.mu.Unlock()

	if !ok {
		return nil // not open — nothing to reload
	}
	if err := e.Buf.ReloadFromDisk(); err != nil {
		return fmt.Errorf("reload %s: %w", path, err)
	}
	// Clamp cursor and scroll to safe positions after content change.
	e.MoveCursorTo(e.CursorLine, e.CursorCol)
	e.ClampScroll()
	slog.Debug("file reloaded from disk", "path", canon)
	return nil
}

// DeleteFile removes a file or directory from disk and cleans up session state.
// Closes editor buffers, removes context/modified entries, and unwires LSP sync
// for the deleted path and any children. If the active editor is affected, falls
// back to another open editor or installs a fresh empty one.
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
	if s.pendingEdit != nil || s.stagedEditFile != "" {
		return ErrEditPending
	}

	if err := removeFromDisk(absPath, path); err != nil {
		return err
	}

	removed := s.cleanupDeletedPath(canon)
	for _, e := range removed {
		s.unwireBufferSync(e)
		e.Close()
	}

	s.saveContext()
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

// cleanupDeletedPath removes all session state (editors, context, modified)
// for the given canonical path and any children (if a directory was deleted).
// Returns removed editors so the caller can unwire and close them.
func (s *Session) cleanupDeletedPath(canon string) []*editor.Editor {
	dirPrefix := canon + string(filepath.Separator)
	matches := func(p string) bool {
		return p == canon || strings.HasPrefix(p, dirPrefix)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var removed []*editor.Editor
	for p, e := range s.editors {
		if matches(p) {
			removed = append(removed, e)
			delete(s.editors, p)
			if s.activeFile == p {
				s.activeFile = ""
				s.activeEditor = nil
			}
		}
	}
	deleteMatching(s.contextSet, matches)
	deleteMatching(s.modifiedFiles, matches)

	// Ensure s.activeEditor is never nil.
	if s.activeEditor == nil {
		for p, ed := range s.editors {
			s.activeEditor = ed
			s.activeFile = p
			break
		}
		if s.activeEditor == nil {
			e := editor.New(buffer.New())
			s.activeEditor = e
			// Track the empty editor so Session.Close() can release its resources.
			s.editors[""] = e
			s.activeFile = ""
		}
	}
	return removed
}

// deleteMatching removes all entries from a map whose keys satisfy pred.
func deleteMatching(m map[string]bool, pred func(string) bool) {
	for k := range m {
		if pred(k) {
			delete(m, k)
		}
	}
}

// editorForEdit returns the editor targeted by the current pending edit.
// Falls back to the active editor if no path is set (backward compat).
// If the target file isn't open yet, auto-opens it from disk — the agent
// may have read the file via read_file (which doesn't create a buffer)
// and then proposed an edit_file on it.
//
// Callers must hold a non-nil s.pendingEdit — all public entry points
// (ReviewEdit, ApproveEdit, PrepareApproval) early-return before reaching
// here, so the nil case is not defended against.
func (s *Session) editorForEdit() *editor.Editor {
	path := s.pendingEdit.Path
	if path == "" {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.activeEditor
	}
	canon := s.CanonPath(path)

	s.mu.RLock()
	e, ok := s.editors[canon]
	s.mu.RUnlock()
	if ok {
		return e
	}

	// Auto-open: the agent proposed an edit to a file that isn't open yet.
	absPath, err := s.resolvePath(path)
	if err != nil {
		return nil
	}
	buf, err := buffer.NewFromFile(absPath)
	if err != nil {
		return nil
	}
	e = s.newEditor(buf)
	s.mu.Lock()
	s.editors[canon] = e
	s.mu.Unlock()

	// Wire LSP sync for auto-opened editor.
	s.wireBufferSync(e)

	return e
}
