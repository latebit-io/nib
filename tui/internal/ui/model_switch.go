package ui

import (
	"log/slog"

	tea "charm.land/bubbletea/v2"

	"github.com/latebit-io/junto/engine/llmconfig"
)

// Model-selector + OAuth connect flow for AppModel.
//
// applySwitchResult updates the agent pane after a model switch
// completes; openModelSelector launches the inline picker (or offers
// OAuth connection when no provider is wired); startOAuthConnect kicks
// off the async OAuth handshake.
//
// All three are state-mutation methods on AppModel — they read
// callbacks (ListModels, ConnectOAuth, IsOAuthProfile, HasOAuthToken,
// LLMProfileNames) wired by the composition root and update the agent
// pane / pendingModelProfile field. Grouped here so the auth flow does
// not interleave with file/dialog/event handling in app.go.

// applySwitchResult updates the agent pane after a model switch attempt.
// Handles three outcomes: full success, success with persistence warning
// (displayModel non-empty but err non-nil), and outright failure.
func (m *AppModel) applySwitchResult(profile, displayModel string, err error) {
	if displayModel == "" && err != nil {
		m.AgentPane.AppendMeta("\n[switch failed: " + err.Error() + "]\n")
		return
	}
	label := displayModel
	if profile != "" {
		label = profile + ": " + displayModel
	}
	m.AgentPane.SetModelLabel(label)
	if err != nil {
		m.AgentPane.AppendMeta("\n[switched to " + label + " — selection may not persist]\n")
	} else {
		m.AgentPane.AppendMeta("\n[switched to " + label + "]\n")
	}
}

func (m *AppModel) openModelSelector() tea.Cmd {
	if m.ListModels == nil {
		// No provider configured — check if we can offer OAuth profiles to connect.
		if m.IsOAuthProfile != nil && m.HasOAuthToken != nil && m.LLMProfileNames != nil {
			profiles := m.LLMProfileNames()
			slog.Debug("model selector: no provider, checking profiles", "count", len(profiles))
			var connectItems []ModelSelectorItem
			for _, p := range profiles {
				if providerID := m.IsOAuthProfile(p); providerID != "" && !m.HasOAuthToken(p) {
					connectItems = append(connectItems, ModelSelectorItem{
						ID:      "_connect",
						Name:    "Connect to " + p,
						Profile: p,
					})
				}
			}
			slog.Debug("model selector: connect items", "count", len(connectItems))
			if len(connectItems) > 0 {
				m.AgentPane.OpenModelSelector(connectItems, connectItems[0].Profile, "", profiles)
				slog.Debug("model selector: opened", "active", m.AgentPane.IsModelSelectorActive())
				return nil
			}
		} else {
			slog.Debug("model selector: callbacks missing", "isOAuth", m.IsOAuthProfile != nil, "hasToken", m.HasOAuthToken != nil, "profiles", m.LLMProfileNames != nil)
		}
		globalPath := llmconfig.GlobalConfigPath()
		if globalPath == "" {
			globalPath = "<user-config-dir>/junto/llm.json"
		}
		m.AgentPane.AppendMeta("\n[no LLM configured — create " + globalPath + " or .project/llm.json]\n")
		return nil
	}
	// Fetch models for the current profile. Tab cycles providers if multiple exist.
	profile := m.Session.LLMProfile()
	m.pendingModelProfile = profile
	m.AgentPane.AppendMeta("\n[fetching models...]\n")
	listFn := m.ListModels
	return func() tea.Msg {
		items, err := listFn(profile)
		return modelListMsg{profile: profile, items: items, err: err}
	}
}

// startOAuthConnect initiates an OAuth connection flow for the given profile.
// The flow runs fully async — no blocking on the TUI thread.
func (m *AppModel) startOAuthConnect(profile string) (tea.Model, tea.Cmd) {
	m.AgentPane.AppendMeta("\n[connecting to " + profile + "...]\n")
	return m, m.ConnectOAuth(profile)
}
