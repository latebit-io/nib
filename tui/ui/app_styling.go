package ui

// Styling, evaluator, terse, and autonomy-dial callbacks and toggles.
// Extracted from app.go (Phase 1 of AppModel decomposition).

// SetStyleName sets the current coding style display name for the status bar.
// Pass empty string to clear the indicator.
func (m *AppModel) SetStyleName(name string) {
	m.styleName = name
}

// SetEvaluatorEnabled sets the evaluator status bar indicator.
func (m *AppModel) SetEvaluatorEnabled(enabled bool) {
	m.evaluatorEnabled = enabled
}

// cycleStyle advances to the next coding style via the CycleStyle callback.
// Does nothing if no styles are configured.
func (m *AppModel) cycleStyle() {
	if m.CycleStyle == nil {
		return
	}
	m.styleName = m.CycleStyle()
}

// toggleEvaluator flips the style evaluator on/off via the ToggleEvaluator callback.
// Does nothing if no callback is wired (no agent or no provider).
func (m *AppModel) toggleEvaluator() {
	if m.ToggleEvaluator == nil {
		return
	}
	m.evaluatorEnabled = m.ToggleEvaluator(!m.evaluatorEnabled)
}

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
