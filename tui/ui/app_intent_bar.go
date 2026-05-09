package ui

import (
	"strings"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
)

// Intent bar rendering.
// Extracted from app.go (Phase 8 of AppModel decomposition).
//
// The intent bar is the single-row strip at the top of the TUI that
// names what the agent is currently doing. Three states:
//
//   - No agent wired: "Editor" — the TUI is in standalone mode.
//   - Active goal:    " <goalPath>" — the path of the goal the agent
//                     is working on, with bold/highlighted styling.
//   - Idle agent:     "Ready" — agent is wired but not currently
//                     pursuing a goal.
//
// The bar always occupies one row and never wraps; long goal paths are
// truncated with an ellipsis. Width-padding ensures the row fills the
// terminal so the styling renders edge-to-edge.

// Package-level styles for the intent bar — allocated once, not per frame.
var (
	intentIdleStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240")).
			Background(lipgloss.Color("236"))
	intentActiveStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("230")).
				Background(lipgloss.Color("235"))
)

// renderIntentBar renders the one-row intent bar. The text is truncated
// with an ellipsis if it would wrap, and padded to the full terminal
// width so the background style covers the whole row.
func (m *AppModel) renderIntentBar() string {
	var text string
	var style lipgloss.Style

	_, goalPath := m.Session.ActiveGoal()

	switch {
	case !m.Session.HasAgent():
		text = " Editor"
		style = intentIdleStyle
	case goalPath != "":
		text = " " + goalPath
		style = intentActiveStyle
	default:
		text = " Ready"
		style = intentIdleStyle
	}

	// Truncate to fit width (one line, never wraps)
	runes := []rune(text)
	if len(runes) > m.Width {
		runes = runes[:m.Width-1]
		text = string(runes) + "…"
	}

	// Pad to full width
	padding := m.Width - utf8.RuneCountInString(text)
	if padding > 0 {
		text += strings.Repeat(" ", padding)
	}

	return style.Render(text)
}
