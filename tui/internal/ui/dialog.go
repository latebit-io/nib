package ui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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
func (d *DialogModel) Show(message string, options ...string) {
	d.Message = message
	d.Options = options
	d.Selected = 0
	d.Active = true
}

// Update handles key input when the dialog is active.
// Returns a DialogResultMsg when the user makes a choice.
func (d *DialogModel) Update(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
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

// Render draws the dialog as a centered overlay within the given dimensions.
func (d *DialogModel) Render(width, height int) string {
	if !d.Active || width == 0 || height == 0 {
		return ""
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

	// Center the box vertically
	boxLines := strings.Split(box, "\n")
	boxH := len(boxLines)
	topPad := (height - boxH) / 2
	if topPad < 0 {
		topPad = 0
	}

	// Center each line horizontally
	var output []string
	for range topPad {
		output = append(output, strings.Repeat(" ", width))
	}
	for _, line := range boxLines {
		lineW := lipgloss.Width(line)
		leftPad := (width - lineW) / 2
		if leftPad < 0 {
			leftPad = 0
		}
		output = append(output, strings.Repeat(" ", leftPad)+line)
	}
	// Fill remaining lines
	for len(output) < height {
		output = append(output, strings.Repeat(" ", width))
	}

	return strings.Join(output[:height], "\n")
}
