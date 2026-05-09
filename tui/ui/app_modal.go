package ui

import (
	"path/filepath"

	tea "charm.land/bubbletea/v2"
)

// Modal-overlay input gating.
// Extracted from app.go's Update() (Phase 7 of AppModel decomposition).
//
// Five overlays can be modal — Help, ModelSelector, Palette, SearchOverlay,
// Dialog. While any is active, input messages (key + mouse) are consumed
// by the overlay and never reach the main switch. Non-input messages
// (engine events, ticks, window resize, file watcher, async results) must
// still flow through, so this gate only intercepts input — not all
// messages.
//
// Ctrl+Q (ActionQuit) is the one input the ModelSelector explicitly does
// NOT swallow: a quit signal must escape modal capture so the user can
// always exit. Other modals don't need this carve-out because Esc maps
// inside their own Update() to "close the overlay."
//
// SearchOverlay additionally consumes its own async result messages
// (`searchResultMsg`, `SearchOpenFileMsg`) while open — the post-modal
// switch has fallthrough cases for the same messages so they still land
// when the overlay closed before delivery.

// handleModalInput consumes input messages while a modal overlay is
// active. Returns (cmd, true) if the message was handled by a modal;
// (nil, false) otherwise so the caller falls through to its main
// dispatcher.
func (m *AppModel) handleModalInput(msg tea.Msg) (tea.Cmd, bool) {
	// Help overlay is modal for user input only — non-input messages
	// (engine events, window resize, ticks) must still be processed.
	if m.Help.Active {
		switch typed := msg.(type) {
		case tea.KeyPressMsg:
			m.Help.Update(typed, m.Height-2)
			return nil, true
		case tea.MouseMsg:
			return nil, true
		}
	}

	// Model selector is modal — captures most input when active.
	// Ctrl+Q always quits regardless of modal state.
	if m.AgentPane.IsModelSelectorActive() {
		switch typed := msg.(type) {
		case tea.KeyPressMsg:
			if m.Keymap.Match(typed) == ActionQuit {
				m.Quit = true
				return tea.Quit, true
			}
			cmd := m.AgentPane.UpdateModelSelector(typed)
			return cmd, true
		case tea.MouseMsg:
			return nil, true
		}
	}

	// Palette is modal — captures all input when active.
	if m.Palette.Active {
		switch typed := msg.(type) {
		case tea.KeyPressMsg:
			cmd := m.Palette.Update(typed)
			return cmd, true
		case tea.MouseMsg:
			return nil, true
		}
	}

	// Search overlay is modal — captures all input + its own async results
	// (searchResultMsg / SearchOpenFileMsg) while open.
	if m.SearchOverlay.Active {
		switch typed := msg.(type) {
		case tea.KeyPressMsg:
			cmd := m.SearchOverlay.Update(typed)
			return cmd, true
		case searchResultMsg:
			cmd := m.SearchOverlay.Update(typed)
			return cmd, true
		case SearchOpenFileMsg:
			m.SearchOverlay.Close()
			_, cmd := m.openFile(filepath.Join(m.Session.ProjectRoot(), typed.Path))
			if cmd == nil {
				// Navigate to the specific line.
				m.Editor.MoveCursorTo(typed.Line-1, 0)
				m.Editor.EnsureCursorVisible()
			}
			return cmd, true
		case tea.MouseMsg:
			return nil, true
		}
	}

	// Dialog is modal — captures all input when active.
	if m.Dialog.Active {
		switch typed := msg.(type) {
		case tea.KeyPressMsg:
			cmd := m.Dialog.Update(typed)
			return cmd, true
		case tea.MouseMsg:
			return nil, true // swallow mouse while dialog is visible
		}
	}

	return nil, false
}
