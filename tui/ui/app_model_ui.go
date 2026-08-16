package ui

import (
	tea "charm.land/bubbletea/v2"
)

// Model selector + OAuth + API-key flows.
// Extracted from app.go's Update() (Phase 2 of AppModel decomposition).
//
// Pipeline overview:
//
//   user opens selector → modelSelSwitchProfileMsg     ─┐
//                       (or ModelSelectorResultMsg     │  fetchModelsCmd
//                        with empty ModelID +          ├─> kicks ListModels async
//                        profile)                      │
//                                                       ▼
//                                              modelListMsg arrives:
//                                                ├ stale (profile mismatch)? drop
//                                                ├ err + OAuth profile w/o token? offer "Connect"
//                                                ├ err + key profile w/o key?    offer "Enter API key"
//                                                ├ err otherwise: log
//                                                └ ok: open inline selector
//
//   selector chooses _connect → ConnectOAuth → oauthInstructionMsg (intermediate)
//                                            → oauthConnectResultMsg (terminal) → SwitchModel
//   selector chooses _enter_key → AgentPane.StartAPIKeyInput → apiKeyEnteredMsg → SwitchModel
//   selector chooses ModelID    → SwitchModel directly

// fetchModelsCmd queues an async ListModels call for the given profile and
// records it as the pending request (so handleModelList can drop stale
// responses from superseded fetches). Returns nil if no ListModels callback
// is wired.
func (m *AppModel) fetchModelsCmd(profile string) tea.Cmd {
	if m.ListModels == nil {
		return nil
	}
	m.pendingModelProfile = profile
	m.AgentPane.AppendMeta("\n[fetching models for " + profile + "...]\n")
	listFn := m.ListModels
	return func() tea.Msg {
		items, err := listFn(profile)
		return modelListMsg{profile: profile, items: items, err: err}
	}
}

// handleModelSelSwitchProfile fetches models for the new profile when the
// user tab-cycles to a different provider in the model selector.
func (m *AppModel) handleModelSelSwitchProfile(msg modelSelSwitchProfileMsg) (tea.Model, tea.Cmd) {
	return m, m.fetchModelsCmd(msg.profile)
}

// handleOAuthInstruction surfaces an intermediate OAuth instruction (e.g., a
// device code) in the agent pane while the connection is still in flight.
func (m *AppModel) handleOAuthInstruction(msg oauthInstructionMsg) (tea.Model, tea.Cmd) {
	m.AgentPane.AppendMeta("[" + msg.instruction + "]\n")
	m.AgentPane.AppendMeta("[waiting for authorization...]\n")
	return m, nil
}

// handleAPIKeyEntered stores a user-entered API key for a profile and
// switches to that profile's default model. Cancellation (empty key) is
// a no-op.
func (m *AppModel) handleAPIKeyEntered(msg apiKeyEnteredMsg) (tea.Model, tea.Cmd) {
	if msg.key == "" {
		return m, nil
	}
	if m.StoreAPIKey == nil {
		return m, nil
	}
	if err := m.StoreAPIKey(msg.profile, msg.key); err != nil {
		m.AgentPane.AppendMeta("\n[failed to save key: " + err.Error() + "]\n")
		return m, nil
	}
	m.AgentPane.AppendMeta("\n[API key saved for " + msg.profile + "]\n")
	dm, switchErr := m.Session.SwitchModel(msg.profile, "")
	m.applySwitchResult(msg.profile, dm, switchErr)
	// The overlay only hands focus back to the textarea when an agent
	// already existed; a first key builds the agent here, so refocus now
	// that the input area is drawn.
	if m.Session.HasAgent() {
		m.AgentPane.SetInputActive(true)
	}
	return m, nil
}

// handleOAuthConnectResult finalizes an OAuth flow: on success switches to
// the connected profile's default model (the switcher builds the agent on
// first connect, so the connection is live without restart).
func (m *AppModel) handleOAuthConnectResult(msg oauthConnectResultMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.AgentPane.AppendMeta("\n[connection failed: " + msg.err.Error() + "]\n")
		return m, nil
	}
	m.AgentPane.AppendMeta("\n[connected to " + msg.profile + "!]\n")
	dm, switchErr := m.Session.SwitchModel(msg.profile, "")
	m.applySwitchResult(msg.profile, dm, switchErr)
	return m, nil
}

// handleModelList opens the inline model selector with the freshly-fetched
// list, or surfaces a recovery action when the fetch failed because the
// profile lacks credentials (OAuth → "Connect", API key → "Enter API key").
// Stale responses from superseded fetches are dropped.
func (m *AppModel) handleModelList(msg modelListMsg) (tea.Model, tea.Cmd) {
	if msg.profile != m.pendingModelProfile {
		return m, nil
	}
	if msg.err != nil {
		var profiles []string
		if m.LLMProfileNames != nil {
			profiles = m.LLMProfileNames()
		}
		// OAuth profile without token → offer to connect.
		if m.IsOAuthProfile != nil && m.HasOAuthToken != nil {
			if providerID := m.IsOAuthProfile(msg.profile); providerID != "" && !m.HasOAuthToken(msg.profile) {
				m.AgentPane.OpenModelSelector([]ModelSelectorItem{{
					ID: "_connect", Name: "Connect to " + msg.profile, Profile: msg.profile,
				}}, msg.profile, "", profiles)
				return m, nil
			}
		}
		// API key profile without key → offer to enter one.
		if m.StoreAPIKey != nil && m.HasAPIKey != nil && !m.HasAPIKey(msg.profile) {
			m.AgentPane.OpenModelSelector([]ModelSelectorItem{{
				ID: "_enter_key", Name: "Enter API key for " + msg.profile, Profile: msg.profile,
			}}, msg.profile, "", profiles)
			return m, nil
		}
		m.AgentPane.AppendMeta("\n[failed to list models: " + msg.err.Error() + "]\n")
		return m, nil
	}
	if len(msg.items) == 0 {
		m.AgentPane.AppendMeta("\n[no models available from provider]\n")
		return m, nil
	}
	var profiles []string
	if m.LLMProfileNames != nil {
		profiles = m.LLMProfileNames()
	}
	m.AgentPane.OpenModelSelector(msg.items, msg.profile, m.Session.LLMModel(), profiles)
	return m, nil
}

// handleModelSelectorResult routes the selector's terminal message: profile
// selection (multi-profile mode) refetches models; "_connect" kicks OAuth;
// "_enter_key" switches to API-key input mode; otherwise a real ModelID
// triggers a model switch.
func (m *AppModel) handleModelSelectorResult(msg ModelSelectorResultMsg) (tea.Model, tea.Cmd) {
	if msg.Cancelled {
		return m, nil
	}
	if msg.ModelID == "" && msg.Profile != "" && m.ListModels != nil {
		return m, m.fetchModelsCmd(msg.Profile)
	}
	if msg.ModelID == "_connect" && m.ConnectOAuth != nil {
		return m.startOAuthConnect(msg.Profile)
	}
	if msg.ModelID == "_enter_key" {
		m.AgentPane.StartAPIKeyInput(msg.Profile)
		return m, nil
	}
	if msg.ModelID != "" {
		dm, switchErr := m.Session.SwitchModel(msg.Profile, msg.ModelID)
		m.applySwitchResult(msg.Profile, dm, switchErr)
	}
	return m, nil
}
