package ui

import (
	"log/slog"

	tea "charm.land/bubbletea/v2"
)

// AppModel ↔ FileWatcher bridge.
//
// The pure FileWatcher component (fsnotify-driven, ref-counted dir watching,
// debounced delivery) lives in `watcher.go`. This file is the application-side
// glue: turning the watcher's channel into a tea.Cmd, deciding when an
// external change should clobber an editor buffer, and exposing a
// shutdown surface.
//
// External-reload safety: `handleFileChanged` skips buffers the user has
// modified in-editor — silently merging an external change into a dirty
// buffer would lose work. Buffer-length shrinks after a reload also force
// a `clampPooledEditor` so a stale cursor doesn't dereference past EOF.

// CloseWatcher shuts down the file watcher. Safe to call if the watcher is nil.
func (m *AppModel) CloseWatcher() {
	if m.fileWatcher != nil {
		m.fileWatcher.Close()
	}
}

// listenForFileChanges returns a tea.Cmd that blocks on the file watcher
// channel and delivers the next change as a tea.Msg.
func (m *AppModel) listenForFileChanges() tea.Cmd {
	ch := m.fileWatcher.Changes()
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

// reloadAllBuffers reloads all open buffers from disk. Called after bash
// tool calls that may have modified files outside the edit approval flow.
// Skips buffers the user has modified in-editor to avoid clobbering unsaved work.
func (m *AppModel) reloadAllBuffers() {
	for _, path := range m.Session.OpenFiles() {
		m.handleFileChanged(path)
	}
}

// handleFileChanged reloads a file that was modified externally.
// Skips reload if the buffer has unsaved in-editor changes.
func (m *AppModel) handleFileChanged(path string) {
	// Don't reload buffers the user has modified in-editor.
	of := m.Session.OpenFileForPath(path)
	if of != nil && of.Modified() {
		slog.Debug("skip external reload (buffer modified)", "path", path)
		return
	}
	if err := m.Session.ReloadFile(path); err != nil {
		slog.Warn("auto-reload failed", "path", path, "err", err)
		return
	}
	// Buffer length may have shrunk — clamp the pooled editor so a
	// stale cursor doesn't reference an out-of-bounds line/column.
	m.clampPooledEditor(path)
	// If the changed file is the active one, rebuild the editor model.
	if path == m.Session.ActiveFile() {
		m.rebuildEditorModel()
	}
	slog.Debug("auto-reloaded file", "path", path)
}

// reloadActiveFile re-reads the active file from disk into its buffer.
func (m *AppModel) reloadActiveFile() (tea.Model, tea.Cmd) {
	path := m.Session.ActiveFile()
	if path == "" {
		return m, nil
	}
	if err := m.Session.ReloadFile(path); err != nil {
		slog.Error("reload file failed", "path", path, "err", err)
		m.AgentPane.AppendMeta("[error: " + err.Error() + "]\n")
		return m, nil
	}
	// Buffer length may have shrunk — clamp the pooled editor cursor.
	m.clampPooledEditor(path)
	m.rebuildEditorModel()
	slog.Debug("file reloaded", "path", path)
	return m, nil
}
