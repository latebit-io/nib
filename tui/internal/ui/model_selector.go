package ui

import (
	"charm.land/lipgloss/v2"
)

// ModelSelectorItem represents an entry in the model selector.
type ModelSelectorItem struct {
	// ID is the model identifier sent to the provider.
	ID string
	// Name is the human-readable display name.
	Name string
	// Profile is the provider profile this model belongs to.
	Profile string
}

// ModelSelectorResultMsg is emitted when the user selects a model or cancels.
type ModelSelectorResultMsg struct {
	// Profile is the selected provider profile.
	Profile string
	// ModelID is the selected model ID (empty for profile-level selection).
	ModelID string
	// Cancelled is true when the user dismissed the selector without choosing.
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
