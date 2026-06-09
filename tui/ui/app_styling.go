package ui

import tea "charm.land/bubbletea/v2"

// Terse callbacks and toggle.
// Extracted from app.go (Phase 1 of AppModel decomposition).

// SetTerse sets the terse mode indicator. Use this at startup to sync
// the UI with the agent's initial state.
func (m *AppModel) SetTerse(on bool) {
	m.terse = on
}

// toggleTerse flips terse output mode on/off via the ToggleTerse callback.
// Does nothing if no callback is wired.
func (m *AppModel) toggleTerse() {
	if m.ToggleTerse == nil {
		return
	}
	m.terse = m.ToggleTerse(!m.terse)
}

// handleSetAgentCallbacks installs generic agent callbacks from
// inside the Update goroutine — the only race-free path for callers
// that send the message after the Bubble Tea event loop has started.
// Last call wins; per-handler nil-checks at every dispatch site
// already gate dispatch when a field is nil.
func (m *AppModel) handleSetAgentCallbacks(msg setAgentCallbacksMsg) (tea.Model, tea.Cmd) {
	m.ToggleTerse = msg.toggleTerse
	if msg.initialTerse {
		m.SetTerse(true)
	}
	return m, nil
}

// handleSetCodingCallbacks installs coding-flavored agent callbacks
// from inside the Update goroutine. Currently a no-op — kept for
// future coding-flavored callbacks.
func (m *AppModel) handleSetCodingCallbacks(_ setCodingCallbacksMsg) (tea.Model, tea.Cmd) {
	return m, nil
}
