package ui

import (
	"strings"

	lipgloss "charm.land/lipgloss/v2"
	"github.com/latebit-io/junto/engine/keymap"
	"github.com/mattn/go-runewidth"
)

// HelpModel manages the help overlay state.
type HelpModel struct {
	Active bool
}

var (
	helpTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	helpCatStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	helpKeyStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("228"))
	helpDescStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
)

// RenderOverlay renders the help screen on top of the base content.
func (h *HelpModel) RenderOverlay(base string, width, height int) string {
	bindings := keymap.DefaultBindings()
	categories := keymap.CategoryOrder()

	// Group bindings by category.
	grouped := make(map[keymap.Category][]keymap.Binding)
	for _, b := range bindings {
		grouped[b.Category] = append(grouped[b.Category], b)
	}

	// Find the widest key string for alignment.
	maxKeyWidth := 0
	for _, b := range bindings {
		w := runewidth.StringWidth(strings.Join(b.Keys, ", "))
		if w > maxKeyWidth {
			maxKeyWidth = w
		}
	}

	var sb strings.Builder
	sb.WriteString(helpTitleStyle.Render("  Keyboard Shortcuts"))
	sb.WriteString("\n\n")

	for _, cat := range categories {
		items, ok := grouped[cat]
		if !ok {
			continue
		}
		sb.WriteString(helpCatStyle.Render("  " + string(cat)))
		sb.WriteString("\n")
		for _, b := range items {
			keyStr := strings.Join(b.Keys, ", ")
			padded := keyStr + strings.Repeat(" ", maxKeyWidth-runewidth.StringWidth(keyStr))
			sb.WriteString("    ")
			sb.WriteString(helpKeyStyle.Render(padded))
			sb.WriteString("  ")
			sb.WriteString(helpDescStyle.Render(b.Label))
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	sb.WriteString("  Press Escape to close")

	helpText := sb.String()
	helpLines := strings.Split(helpText, "\n")

	// Center the help overlay.
	overlayHeight := len(helpLines)
	if overlayHeight > height-2 {
		overlayHeight = height - 2
		helpLines = helpLines[:overlayHeight]
	}
	topPad := (height - overlayHeight) / 2
	if topPad < 0 {
		topPad = 0
	}

	// Build full-screen output with the help content centered vertically.
	baseLines := strings.Split(base, "\n")
	for len(baseLines) < height {
		baseLines = append(baseLines, "")
	}

	result := make([]string, height)
	for i := range height {
		if i >= topPad && i < topPad+overlayHeight {
			helpIdx := i - topPad
			// Pad help line to full width to cover the base.
			line := helpLines[helpIdx]
			lineW := runewidth.StringWidth(line)
			if lineW < width {
				line += strings.Repeat(" ", width-lineW)
			}
			result[i] = line
		} else {
			result[i] = baseLines[i]
		}
	}

	return strings.Join(result, "\n")
}
