package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"
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

// modelSelSwitchProfileMsg requests fetching models for a different profile.
type modelSelSwitchProfileMsg struct {
	profile string
}

// Model selector styles — reused across frames.
var (
	modelSelSelectedStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("62")).
				Foreground(lipgloss.Color("230"))
	modelSelCurrentStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("34"))
	modelSelNormalStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("252"))
)

// ModelSelectorModel encapsulates the inline model selector widget.
// It replaces the agent pane input area when active.
type ModelSelectorModel struct {
	Active   bool
	Items    []ModelSelectorItem
	Selected int
	Current  string   // current model ID (highlighted with ●)
	Profile  string   // profile name shown in title
	Profiles []string // all available profiles (for Tab cycling)
}

// Open activates the model selector with the given items and context.
func (s *ModelSelectorModel) Open(items []ModelSelectorItem, profile, currentModel string, profiles []string) {
	s.Active = true
	s.Items = items
	s.Profile = profile
	s.Current = currentModel
	s.Profiles = profiles
	s.Selected = 0
	for i, item := range items {
		if item.ID == currentModel {
			s.Selected = i
			break
		}
	}
}

// Close deactivates the model selector and clears items.
func (s *ModelSelectorModel) Close() {
	s.Active = false
	s.Items = nil
}

// Update handles key input for the model selector.
// Returns a tea.Cmd if a selection or cancellation occurred.
func (s *ModelSelectorModel) Update(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.Code {
	case tea.KeyEscape:
		s.Close()
		return func() tea.Msg { return ModelSelectorResultMsg{Cancelled: true} }
	case tea.KeyEnter:
		if s.Selected < len(s.Items) {
			item := s.Items[s.Selected]
			profile := s.Profile
			s.Close()
			return func() tea.Msg {
				return ModelSelectorResultMsg{Profile: profile, ModelID: item.ID}
			}
		}
		return nil
	case tea.KeyTab:
		if len(s.Profiles) > 1 {
			next := s.nextProfile()
			s.Profile = next
			s.Items = nil
			s.Selected = 0
			return func() tea.Msg {
				return modelSelSwitchProfileMsg{profile: next}
			}
		}
		return nil
	case tea.KeyUp:
		if s.Selected > 0 {
			s.Selected--
		}
		return nil
	case tea.KeyDown:
		if len(s.Items) == 0 {
			return nil
		}
		if s.Selected < len(s.Items)-1 {
			s.Selected++
		}
		return nil
	}
	return nil
}

// nextProfile returns the profile after the current one, wrapping around.
func (s *ModelSelectorModel) nextProfile() string {
	for i, p := range s.Profiles {
		if p == s.Profile {
			return s.Profiles[(i+1)%len(s.Profiles)]
		}
	}
	return s.Profiles[0]
}

// Height returns the number of rows the selector needs, given the pane height.
func (s *ModelSelectorModel) Height(paneHeight int) int {
	maxH := paneHeight - 2
	if maxH < 0 {
		maxH = 0
	}
	h := paneHeight / 2
	if h < 10 {
		h = 10
	}
	if h > maxH {
		h = maxH
	}
	return h
}

// Render writes the model selector into the output slice starting at *row.
func (s *ModelSelectorModel) Render(output []string, row *int, width, height, startRow, endRow int) {
	totalRows := endRow - startRow
	if totalRows < 0 {
		totalRows = 0
	}

	// Title row showing provider/profile.
	if totalRows > 0 && *row < height {
		title := " Select Model"
		if s.Profile != "" {
			title = " " + sanitizeInlineDisplay(s.Profile) + " — Select Model"
		}
		title = runewidth.Truncate(title, width, "…")
		padW := width - runewidth.StringWidth(title)
		if padW > 0 {
			title += strings.Repeat(" ", padW)
		}
		output[*row] = agentDimStyle.Render(title)
		*row++
		totalRows--
	}

	// Hint row at the bottom.
	hintRows := 0
	if totalRows > 2 {
		hintRows = 1
		totalRows--
	}

	// Model list with scroll.
	visible := totalRows
	if visible <= 0 {
		return
	}
	scrollOff := 0
	if s.Selected >= visible {
		scrollOff = s.Selected - visible + 1
	}

	for i := range visible {
		if *row >= height {
			break
		}
		idx := scrollOff + i
		if idx < len(s.Items) {
			item := s.Items[idx]
			indicator := "  "
			if item.ID == s.Current {
				indicator = "● "
			}
			label := indicator + sanitizeInlineDisplay(item.Name)
			label = runewidth.Truncate(label, width, "…")
			padW := width - runewidth.StringWidth(label)
			if padW > 0 {
				label += strings.Repeat(" ", padW)
			}
			if idx == s.Selected {
				output[*row] = modelSelSelectedStyle.Render(label)
			} else if item.ID == s.Current {
				output[*row] = modelSelCurrentStyle.Render(label)
			} else {
				output[*row] = modelSelNormalStyle.Render(label)
			}
		} else {
			output[*row] = strings.Repeat(" ", width)
		}
		*row++
	}

	// Hint row.
	if hintRows > 0 && *row < height {
		hint := " ↑↓ navigate · Enter select · Esc cancel"
		if len(s.Profiles) > 1 {
			hint = " ↑↓ navigate · Tab provider · Enter select · Esc cancel"
		}
		hint = runewidth.Truncate(hint, width, "")
		padW := width - runewidth.StringWidth(hint)
		if padW > 0 {
			hint += strings.Repeat(" ", padW)
		}
		output[*row] = agentDimStyle.Render(hint)
		*row++
	}
}
