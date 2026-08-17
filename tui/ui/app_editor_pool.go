package ui

import (
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/latebit-io/nib/coding/session"
	"github.com/latebit-io/nib/engine/buffer"
	"github.com/latebit-io/nib/engine/openfile"
	"github.com/latebit-io/nib/engine/syntax"
	"github.com/latebit-io/nib/tui/editor"
)

// Editor pool + file-buffer navigation.
//
// The editor pool keys `*editor.Editor` instances by canonical path so
// that switching between open files preserves cursor/selection/scroll
// state per file. Each editor wraps the same `*buffer.Buffer` the
// session's `*openfile.OpenFile` holds — Editor and OpenFile are two
// views over one buffer, not duplicates.
//
// Pool key resolution:
//   - Empty-path scratch handles use "" as the key (matches
//     Session.openFiles[""] which uses the same convention rather than
//     CanonPath("") that would resolve to the project root).
//   - Every other path is canonicalized via Session.CanonPath.
//
// This pattern is documented in /nib/architecture.md (TUI editor
// controller section) and was the load-bearing design that the
// editor-vs-buffer split (PR #143) crystallized.

// editorForOpenFile returns the editor for the given OpenFile, lazily
// creating it on first lookup. Pool key resolution: empty-path scratch
// handles use "" (matching the convention in
// [Session.openFiles] so future iteration/lookup paths stay consistent.
// Every other path is canonicalized.
func (m *AppModel) editorForOpenFile(of *openfile.OpenFile) *editor.Editor {
	if of == nil {
		return nil
	}
	key := of.Buf.Path
	if key != "" {
		key = m.Session.CanonPath(of.Buf.Path)
	}
	if m.editorPool == nil {
		m.editorPool = make(map[string]*editor.Editor)
	}
	if e, ok := m.editorPool[key]; ok {
		return e
	}
	e := editor.New(of.Buf)
	if m.highlighterFactory != nil && of.Buf.Path != "" {
		e.SetHighlighter(m.highlighterFactory(of.Buf.Path))
	}
	m.editorPool[key] = e
	return e
}

// activeEditor returns the editor for the session's currently active
// open file. May return nil when the session has no active file.
func (m *AppModel) activeEditor() *editor.Editor {
	return m.editorForOpenFile(m.Session.ActiveOpenFile())
}

// clampPooledEditor clamps the cursor and viewport for the pooled
// editor at path so a buffer-shrinking reload (external tool, agent
// edit, ReloadFromDisk) doesn't leave the cursor out-of-bounds.
// No-op if no editor is pooled for the path. The pool key resolution
// matches [editorForOpenFile]: empty-path scratch handles use "" as
// the key, every other path is canonicalized.
func (m *AppModel) clampPooledEditor(path string) {
	key := path
	if key != "" {
		key = m.Session.CanonPath(path)
	}
	ed, ok := m.editorPool[key]
	if !ok {
		return
	}
	// Self-call MoveCursorTo with current position so the editor's
	// internal clamp logic (line bounds, column bounds) runs against
	// the new buffer length.
	ed.MoveCursorTo(ed.CursorLine, ed.CursorCol)
	ed.ClampScroll()
}

// dropPooledEditors removes pool entries for a path and any children
// (when path was a directory), closing each editor's highlighter so
// tree-sitter grammar instances are released, and unwatching each file
// so the watcher's per-directory refcount does not leak. Mirrors the
// matching logic in [session.cleanupDeletedPath].
func (m *AppModel) dropPooledEditors(path string) {
	canon := m.Session.CanonPath(path)
	dirPrefix := canon + string(filepath.Separator)
	for k, ed := range m.editorPool {
		if k == canon || strings.HasPrefix(k, dirPrefix) {
			ed.Close()
			delete(m.editorPool, k)
			if m.fileWatcher != nil {
				m.fileWatcher.Unwatch(k)
			}
		}
	}
}

// SetHighlighterFactory installs the syntax-highlighter factory used by
// the editor pool. Existing pooled editors are re-decorated to match.
// Pass nil to disable highlighting. Called at startup by the composition
// root.
func (m *AppModel) SetHighlighterFactory(fn syntax.HighlighterFactory) {
	m.highlighterFactory = fn
	for _, e := range m.editorPool {
		if e == nil || e.Buf == nil || e.Buf.Path == "" {
			continue
		}
		if fn == nil {
			e.SetHighlighter(nil)
			continue
		}
		e.SetHighlighter(fn(e.Buf.Path))
	}
}

// rebuildEditorModel creates a new EditorModel wrapping the pooled
// editor for the session's currently active open file. Cursor/scroll
// state is preserved because [editorForOpenFile] returns the same
// editor instance across calls for a given path.
func (m *AppModel) rebuildEditorModel() {
	ed := m.activeEditor()
	if ed == nil {
		ed = editor.New(buffer.New())
	}
	m.Editor = NewEditorModel(ed, m.Keymap, m.Services)
	m.Editor.OnSave = func() { m.Session.NotifySaved() }
	m.Regions.ReplacePane("editor", m.Editor)
}

// switchBuffer cycles through open buffers by delta (+1 next, -1 prev).
func (m *AppModel) switchBuffer(delta int) (tea.Model, tea.Cmd) {
	files := m.Session.OpenFiles()
	if len(files) <= 1 {
		return m, nil
	}
	slices.Sort(files)
	active := m.Session.ActiveFile()
	idx := 0
	for i, f := range files {
		if f == active {
			idx = i
			break
		}
	}
	next := (idx + delta + len(files)) % len(files)
	return m.openFile(files[next])
}

// openFile switches the editor to a different file. Uses session.SwitchTo
// which keeps editors alive in the multi-buffer map. This method rebuilds
// the EditorModel and updates the region manager.
func (m *AppModel) openFile(path string) (tea.Model, tea.Cmd) {
	if err := m.Session.SwitchTo(path); err != nil {
		if errors.Is(err, session.ErrEditPending) {
			m.AgentPane.AppendMeta("[" + err.Error() + "]\n")
		} else {
			slog.Error("failed to open file", "path", path, "err", err)
			m.AgentPane.AppendMeta("[error: " + err.Error() + "]\n")
		}
		return m, nil
	}

	// Watch the newly opened file for external changes.
	if m.fileWatcher != nil {
		m.fileWatcher.Watch(m.Session.ActiveFile())
	}

	// Rebuild EditorModel with the new active editor from session.
	m.rebuildEditorModel()

	slog.Debug("file opened", "path", path)
	m.refreshDiagnostics(m.Session.ActiveFile())
	m.refreshProjectPane()
	return m, nil
}

// refreshDiagnostics queries the session for diagnostics on the given path
// and updates the editor model. Only updates if path matches the active file.
func (m *AppModel) refreshDiagnostics(path string) {
	if !m.Session.HasLanguageService() {
		return
	}
	canon := m.Session.CanonPath(path)
	if m.Session.ActiveFile() != canon {
		return
	}
	m.Editor.SetDiagnostics(m.Session.Diagnostics(canon))
}
