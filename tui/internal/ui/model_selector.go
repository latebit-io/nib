package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"
)

// ModelSelectorResultMsg is emitted when the user selects a profile or cancels.
type ModelSelectorResultMsg struct {
	Profile   string // selected profile name
	Cancelled bool
}

// ModelSelectorModel manages the model/profile selector overlay.
type ModelSelectorModel struct {
	Active   bool
	Profiles []string // profile names
	Current  string   // currently active profile (highlighted)
	Selected int      // cursor index
	Width    int
	Height   int
}

// Model selector rendering constants.
const (
	modelSelMinWidth   = 30
	modelSelMaxVisible = 10
)

// Model selector styles — reused across frames.
var (
	modelSelBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("62"))
	modelSelTitleStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("230")).
				Bold(true)
	modelSelSelectedStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("62")).
				Foreground(lipgloss.Color("230"))
	modelSelCurrentStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("34"))
	modelSelNormalStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("252"))
	modelSelDimStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("240"))
)

// Open activates the selector with the given profile names.
func (s *ModelSelectorModel) Open(profiles []string, current string) {
	s.Active = true
	s.Profiles = profiles
	s.Current = current
	s.Selected = 0
	// Pre-select the current profile if found.
	for i, p := range profiles {
		if p == current {
			s.Selected = i
			break
		}
	}
}

// Close deactivates the selector.
func (s *ModelSelectorModel) Close() {
	s.Active = false
	s.Profiles = nil
}

// Update handles key input when the selector is active.
func (s *ModelSelectorModel) Update(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.Code {
	case tea.KeyEscape:
		s.Close()
		return func() tea.Msg { return ModelSelectorResultMsg{Cancelled: true} }

	case tea.KeyEnter:
		if len(s.Profiles) > 0 && s.Selected < len(s.Profiles) {
			profile := s.Profiles[s.Selected]
			s.Close()
			return func() tea.Msg {
				return ModelSelectorResultMsg{Profile: profile}
			}
		}
		return nil

	case tea.KeyUp:
		if s.Selected > 0 {
			s.Selected--
		}
		return nil

	case tea.KeyDown:
		if s.Selected < len(s.Profiles)-1 {
			s.Selected++
		}
		return nil
	}
	return nil
}

// RenderOverlay renders the selector as a floating box over the background.
func (s *ModelSelectorModel) RenderOverlay(background string, width, height int) string {
	if len(s.Profiles) == 0 {
		s.Close()
		return background
	}

	bgLines := strings.Split(background, "\n")
	for len(bgLines) < height {
		bgLines = append(bgLines, "")
	}

	// Bail out if the terminal is too narrow for any useful display.
	if width < 10 || height < 5 {
		return background
	}

	// Determine box dimensions.
	maxLabelW := 0
	for _, p := range s.Profiles {
		w := runewidth.StringWidth(p)
		if w > maxLabelW {
			maxLabelW = w
		}
	}
	// Add space for indicator (● ) and padding.
	boxInnerW := maxLabelW + 4
	if boxInnerW < modelSelMinWidth {
		boxInnerW = modelSelMinWidth
	}
	maxInner := width - 4
	if maxInner < 1 {
		maxInner = 1
	}
	if boxInnerW > maxInner {
		boxInnerW = maxInner
	}

	visible := len(s.Profiles)
	if visible > modelSelMaxVisible {
		visible = modelSelMaxVisible
	}

	// Build content lines.
	var lines []string
	title := modelSelTitleStyle.Render(" Select Model Profile")
	titlePad := boxInnerW - runewidth.StringWidth(" Select Model Profile")
	if titlePad > 0 {
		title += strings.Repeat(" ", titlePad)
	}
	lines = append(lines, title)
	lines = append(lines, modelSelDimStyle.Render(strings.Repeat("─", boxInnerW)))

	scrollOff := 0
	if s.Selected >= scrollOff+visible {
		scrollOff = s.Selected - visible + 1
	}

	for i := scrollOff; i < scrollOff+visible && i < len(s.Profiles); i++ {
		p := s.Profiles[i]
		indicator := "  "
		if p == s.Current {
			indicator = "● "
		}
		availW := boxInnerW - runewidth.StringWidth(indicator)
		if availW < 0 {
			availW = 0
		}
		label := indicator + runewidth.Truncate(p, availW, "…")
		padW := boxInnerW - runewidth.StringWidth(label)
		if padW > 0 {
			label += strings.Repeat(" ", padW)
		}
		if i == s.Selected {
			lines = append(lines, modelSelSelectedStyle.Render(label))
		} else if p == s.Current {
			lines = append(lines, modelSelCurrentStyle.Render(label))
		} else {
			lines = append(lines, modelSelNormalStyle.Render(label))
		}
	}

	footer := modelSelDimStyle.Render(strings.Repeat("─", boxInnerW))
	lines = append(lines, footer)
	hint := modelSelDimStyle.Render(" ↑↓ navigate · Enter select · Esc cancel")
	hintPad := boxInnerW - runewidth.StringWidth(" ↑↓ navigate · Enter select · Esc cancel")
	if hintPad > 0 {
		hint += strings.Repeat(" ", hintPad)
	}
	lines = append(lines, hint)

	// Build bordered box.
	boxContent := strings.Join(lines, "\n")
	box := modelSelBorderStyle.Render(boxContent)
	boxLines := strings.Split(box, "\n")

	// Center vertically.
	topPad := (height - len(boxLines)) / 3
	if topPad < 1 {
		topPad = 1
	}

	// Overlay box onto background.
	for i, bLine := range boxLines {
		row := topPad + i
		if row >= height {
			break
		}
		centered := lipgloss.Place(width, 1, lipgloss.Center, lipgloss.Center, bLine)
		bgLines[row] = centered
	}

	return strings.Join(bgLines[:height], "\n")
}
