package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/latebit-io/junto/engine/keymap"
	"github.com/mattn/go-runewidth"
)

// HelpModel manages the help overlay state.
type HelpModel struct {
	Active    bool
	scrollOff int // scroll offset in lines
	lines     []string
}

var (
	helpTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	helpCatStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	helpKeyStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("228"))
	helpDescStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
)

// Open activates the help overlay and rebuilds the content.
func (h *HelpModel) Open() {
	h.Active = true
	h.scrollOff = 0
	h.lines = buildHelpLines()
}

// Update handles key events while the help overlay is active.
// Returns true if the event was consumed.
func (h *HelpModel) Update(msg tea.KeyPressMsg, viewHeight int) bool {
	switch msg.Code {
	case tea.KeyEscape, tea.KeyEnter:
		h.Active = false
		return true
	case tea.KeyUp:
		if h.scrollOff > 0 {
			h.scrollOff--
		}
		return true
	case tea.KeyDown:
		maxScroll := len(h.lines) - viewHeight
		if maxScroll < 0 {
			maxScroll = 0
		}
		if h.scrollOff < maxScroll {
			h.scrollOff++
		}
		return true
	case tea.KeyPgUp:
		h.scrollOff -= viewHeight
		if h.scrollOff < 0 {
			h.scrollOff = 0
		}
		return true
	case tea.KeyPgDown:
		maxScroll := len(h.lines) - viewHeight
		if maxScroll < 0 {
			maxScroll = 0
		}
		h.scrollOff += viewHeight
		if h.scrollOff > maxScroll {
			h.scrollOff = maxScroll
		}
		return true
	}
	// Also dismiss on 'q' or F1 toggle
	if msg.Text == "q" || msg.String() == "f1" {
		h.Active = false
		return true
	}
	return true // consume all keys while help is open
}

// RenderOverlay renders the help screen on top of the base content.
func (h *HelpModel) RenderOverlay(base string, width, height int) string {
	if width <= 0 || height <= 2 {
		return base
	}

	viewHeight := height - 2 // leave room for top/bottom margin
	if viewHeight < 1 {
		return base
	}

	// Apply scroll offset to get the visible window.
	visible := h.lines
	if h.scrollOff > 0 && h.scrollOff < len(visible) {
		visible = visible[h.scrollOff:]
	}
	if len(visible) > viewHeight {
		visible = visible[:viewHeight]
	}

	topPad := (height - len(visible)) / 2
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
		if i >= topPad && i < topPad+len(visible) {
			line := visible[i-topPad]
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

// buildHelpLines generates the formatted help text as a slice of lines.
func buildHelpLines() []string {
	bindings := keymap.DefaultBindings()
	categories := keymap.CategoryOrder()

	grouped := make(map[keymap.Category][]keymap.Binding)
	for _, b := range bindings {
		grouped[b.Category] = append(grouped[b.Category], b)
	}

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

	sb.WriteString("  Press Escape, Enter, or Q to close  |  Arrow keys to scroll")

	return strings.Split(sb.String(), "\n")
}
