package ui

import (
	"log/slog"
	"path/filepath"

	tea "charm.land/bubbletea/v2"
)

// Overlay-message handlers (palette + search overlay).
//
// The pure overlay components live in `palette.go` / `search_overlay.go`.
// This file is the AppModel-side bridge — turning palette/search
// async-result messages into state mutations on AppModel.
//
// SearchOverlay's modal-active path is handled separately in
// `app_modal.go`. The cases here run when the overlay closed before the
// async message was delivered (race between user dismissal and async
// search result).

// handlePaletteError surfaces a file-listing error in the agent pane.
func (m *AppModel) handlePaletteError(msg paletteErrorMsg) (tea.Model, tea.Cmd) {
	slog.Error("failed to list files", "err", msg.err)
	m.AgentPane.AppendMeta("\n[file listing failed: " + msg.err + "]\n")
	return m, nil
}

// handlePaletteFiles opens the palette with completed file-listing results.
func (m *AppModel) handlePaletteFiles(msg paletteFilesMsg) (tea.Model, tea.Cmd) {
	m.Palette.Open(msg.items)
	return m, nil
}

// handlePaletteResult acts on the user's palette selection. Only "file"
// category selections are handled today; cancellation is a no-op.
func (m *AppModel) handlePaletteResult(msg PaletteResultMsg) (tea.Model, tea.Cmd) {
	if !msg.Cancelled && msg.Category == "file" {
		return m.openFile(msg.Item.Value)
	}
	return m, nil
}

// handleSearchOpenFile opens a file selected from a project search result.
// Reachable when the overlay closed before the async message arrived
// (the modal-active path in app_modal.go handles the in-overlay case).
func (m *AppModel) handleSearchOpenFile(msg SearchOpenFileMsg) (tea.Model, tea.Cmd) {
	model, cmd := m.openFile(filepath.Join(m.Session.ProjectRoot(), msg.Path))
	if cmd == nil {
		m.Editor.MoveCursorTo(msg.Line-1, 0)
		m.Editor.EnsureCursorVisible()
	}
	return model, cmd
}

// handleSearchResult forwards async search results to the overlay if it
// is still active (race: result arrived after the overlay reopened).
func (m *AppModel) handleSearchResult(msg searchResultMsg) (tea.Model, tea.Cmd) {
	if m.SearchOverlay.Active {
		cmd := m.SearchOverlay.Update(msg)
		return m, cmd
	}
	return m, nil
}
