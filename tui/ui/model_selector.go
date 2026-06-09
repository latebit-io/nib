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
// It replaces the agent pane input area when active. All state is
// unexported — mutations go through Open/Close/Update to preserve
// invariants (e.g. Selected always in range, layout recomputation).
type ModelSelectorModel struct {
	active   bool
	items    []ModelSelectorItem
	selected int
	current  string   // current model ID (highlighted with ●)
	profile  string   // profile name shown in title
	profiles []string // all available profiles (for Tab cycling)
}

// IsActive reports whether the model selector is currently open.
func (s *ModelSelectorModel) IsActive() bool { return s.active }

// Open activates the model selector with the given items and context.
func (s *ModelSelectorModel) Open(items []ModelSelectorItem, profile, currentModel string, profiles []string) {
	s.active = true
	s.items = items
	s.profile = profile
	s.current = currentModel
	s.profiles = profiles
	s.selected = 0
	for i, item := range items {
		if item.ID == currentModel {
			s.selected = i
			break
		}
	}
}

// Close deactivates the model selector and clears items.
func (s *ModelSelectorModel) Close() {
	s.active = false
	s.items = nil
}

// Update handles key input for the model selector.
// Returns a tea.Cmd if a selection or cancellation occurred.
func (s *ModelSelectorModel) Update(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.Code {
	case tea.KeyEscape:
		s.Close()
		return func() tea.Msg { return ModelSelectorResultMsg{Cancelled: true} }
	case tea.KeyEnter:
		if s.selected < len(s.items) {
			item := s.items[s.selected]
			profile := s.profile
			s.Close()
			return func() tea.Msg {
				return ModelSelectorResultMsg{Profile: profile, ModelID: item.ID}
			}
		}
		return nil
	case tea.KeyTab:
		if len(s.profiles) > 1 {
			next := s.nextProfile()
			s.profile = next
			s.items = nil
			s.selected = 0
			return func() tea.Msg {
				return modelSelSwitchProfileMsg{profile: next}
			}
		}
		return nil
	case tea.KeyUp:
		if s.selected > 0 {
			s.selected--
		}
		return nil
	case tea.KeyDown:
		if len(s.items) == 0 {
			return nil
		}
		if s.selected < len(s.items)-1 {
			s.selected++
		}
		return nil
	}
	return nil
}

// nextProfile returns the profile after the current one, wrapping around.
func (s *ModelSelectorModel) nextProfile() string {
	for i, p := range s.profiles {
		if p == s.profile {
			return s.profiles[(i+1)%len(s.profiles)]
		}
	}
	return s.profiles[0]
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
		if s.profile != "" {
			title = " " + sanitizeInlineDisplay(s.profile) + " — Select Model"
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
	if s.selected >= visible {
		scrollOff = s.selected - visible + 1
	}

	for i := range visible {
		if *row >= height {
			break
		}
		idx := scrollOff + i
		if idx < len(s.items) {
			item := s.items[idx]
			indicator := "  "
			if item.ID == s.current {
				indicator = "● "
			}
			label := indicator + sanitizeInlineDisplay(item.Name)
			label = runewidth.Truncate(label, width, "…")
			padW := width - runewidth.StringWidth(label)
			if padW > 0 {
				label += strings.Repeat(" ", padW)
			}
			if idx == s.selected {
				output[*row] = modelSelSelectedStyle.Render(label)
			} else if item.ID == s.current {
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
		if len(s.profiles) > 1 {
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
