package ui

import tea "charm.land/bubbletea/v2"

// Terse and autonomy-dial callbacks and toggles.
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

// cycleDial advances the autonomy dial to its next level and notifies the
// OnDialChange callback if wired.
func (m *AppModel) cycleDial() {
	m.dial = m.dial.Cycle()
	if m.OnDialChange != nil {
		m.OnDialChange(m.dial)
	}
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
// from inside the Update goroutine. Same race-avoidance rationale as
// [AppModel.handleSetAgentCallbacks].
func (m *AppModel) handleSetCodingCallbacks(msg setCodingCallbacksMsg) (tea.Model, tea.Cmd) {
	m.OnDialChange = msg.onDialChange
	return m, nil
}
