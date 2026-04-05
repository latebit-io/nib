package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// DialogResultMsg is emitted when the user selects a dialog option.
// Choice is the index of the selected option, or -1 if cancelled.
type DialogResultMsg struct {
	Choice int
}

// DialogModel renders a centered modal overlay that captures all key input
// until the user picks an option or cancels with Escape.
type DialogModel struct {
	Message  string
	Options  []string
	Selected int
	Active   bool
}

// Show activates the dialog with a message and options.
// At least one option is required; if none are provided the dialog is not shown.
func (d *DialogModel) Show(message string, options ...string) {
	if len(options) == 0 {
		return
	}
	d.Message = message
	d.Options = options
	d.Selected = 0
	d.Active = true
}

// Update handles key input when the dialog is active.
// Returns a DialogResultMsg when the user makes a choice.
func (d *DialogModel) Update(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.Code {
	case tea.KeyLeft:
		if d.Selected > 0 {
			d.Selected--
		}
	case tea.KeyRight:
		if d.Selected < len(d.Options)-1 {
			d.Selected++
		}
	case tea.KeyEnter:
		choice := d.Selected
		d.Active = false
		return func() tea.Msg { return DialogResultMsg{Choice: choice} }
	case tea.KeyEscape:
		d.Active = false
		return func() tea.Msg { return DialogResultMsg{Choice: -1} }
	}
	return nil
}

// RenderOverlay draws the dialog centered over the given background content.
func (d *DialogModel) RenderOverlay(base string, width, height int) string {
	if !d.Active || width == 0 || height == 0 {
		return base
	}

	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("62")).
		Padding(1, 2).
		Align(lipgloss.Center)

	selectedStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("230")).
		Background(lipgloss.Color("62")).
		Padding(0, 1)
	normalStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("240")).
		Padding(0, 1)

	// Build option buttons
	var buttons []string
	for i, opt := range d.Options {
		if i == d.Selected {
			buttons = append(buttons, selectedStyle.Render(opt))
		} else {
			buttons = append(buttons, normalStyle.Render(opt))
		}
	}

	content := d.Message + "\n\n" + strings.Join(buttons, "  ")
	box := boxStyle.Render(content)

	// Overlay the box onto the background
	bgLines := strings.Split(base, "\n")
	for len(bgLines) < height {
		bgLines = append(bgLines, strings.Repeat(" ", width))
	}

	boxLines := strings.Split(box, "\n")
	topPad := (height - len(boxLines)) / 2
	if topPad < 0 {
		topPad = 0
	}

	for i, boxLine := range boxLines {
		row := topPad + i
		if row >= height {
			break
		}
		bgLines[row] = lipgloss.Place(width, 1, lipgloss.Center, lipgloss.Center, boxLine)
	}

	return strings.Join(bgLines[:height], "\n")
}
