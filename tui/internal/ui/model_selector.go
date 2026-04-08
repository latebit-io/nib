package ui

import (
	"charm.land/lipgloss/v2"
)

// ModelSelectorItem represents an entry in the model selector.
type ModelSelectorItem struct {
	ID      string // model identifier sent to the provider
	Name    string // display name
	Profile string // which profile/provider this belongs to
}

// ModelSelectorResultMsg is emitted when the user selects a model or cancels.
type ModelSelectorResultMsg struct {
	Profile   string // selected profile
	ModelID   string // selected model ID (empty for profile-level selection)
	Cancelled bool
}

// modelListMsg carries the result of an async ListModels call.
type modelListMsg struct {
	profile string
	items   []ModelSelectorItem
	err     error
}

// Model selector styles — reused across frames by the agent pane's inline renderer.
var (
	modelSelSelectedStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("62")).
				Foreground(lipgloss.Color("230"))
	modelSelCurrentStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("34"))
	modelSelNormalStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("252"))
)
