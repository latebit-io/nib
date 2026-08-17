package ui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/latebit-io/nib/tui/keymap"
	"github.com/mattn/go-runewidth"
)

// HelpModel manages the help overlay state.
type HelpModel struct {
	// Active is true when the help overlay is visible and capturing input.
	Active    bool
	scrollOff int      // scroll offset in lines
	lines     []string // pre-built help content lines (unstyled box content)
}

// Help overlay styles: title, category header, key column, description,
// surrounding box, and footer hint line.
var (
	helpTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	helpCatStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	helpKeyStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("228"))
	helpDescStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	helpBoxStyle   = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("62")).
			Background(lipgloss.Color("235")).
			Padding(0, 1)
	helpFooterStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("245")).
			Background(lipgloss.Color("235"))
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
		maxScroll := h.maxScroll(viewHeight)
		if h.scrollOff < maxScroll {
			h.scrollOff++
		}
		return true
	case tea.KeyPgUp:
		vis := h.visibleLines(viewHeight)
		h.scrollOff -= vis
		if h.scrollOff < 0 {
			h.scrollOff = 0
		}
		return true
	case tea.KeyPgDown:
		vis := h.visibleLines(viewHeight)
		maxScroll := h.maxScroll(viewHeight)
		h.scrollOff += vis
		if h.scrollOff > maxScroll {
			h.scrollOff = maxScroll
		}
		return true
	}
	// Also dismiss on 'q' or F4 toggle.
	if msg.Text == "q" || msg.String() == "f4" {
		h.Active = false
		return true
	}
	return true // consume all keys while help is open
}

// visibleLines returns how many content lines fit inside the box at the given terminal height.
func (h *HelpModel) visibleLines(termHeight int) int {
	// Box takes ~80% of terminal height, minus 2 for border, minus 1 for footer.
	boxH := termHeight * 4 / 5
	vis := boxH - 3
	if vis < 1 {
		vis = 1
	}
	return vis
}

// maxScroll returns the maximum scroll offset.
func (h *HelpModel) maxScroll(termHeight int) int {
	ms := len(h.lines) - h.visibleLines(termHeight)
	if ms < 0 {
		return 0
	}
	return ms
}

// RenderOverlay renders the help panel on top of the base content,
// with the editor visible in the background.
func (h *HelpModel) RenderOverlay(base string, width, height int) string {
	if width <= 4 || height <= 4 {
		return base
	}

	vis := h.visibleLines(height)

	// Apply scroll offset to get the visible window.
	visible := h.lines
	if h.scrollOff > 0 && h.scrollOff < len(visible) {
		visible = visible[h.scrollOff:]
	}
	if len(visible) > vis {
		visible = visible[:vis]
	}

	// Build content: visible lines + footer.
	scrollInfo := ""
	if len(h.lines) > vis {
		scrollInfo = fmt.Sprintf("  (%d-%d of %d)", h.scrollOff+1, h.scrollOff+len(visible), len(h.lines))
	}
	footer := helpFooterStyle.Render("  Esc/Q to close  |  Arrow keys to scroll" + scrollInfo)

	// Box width: fit content or cap at terminal width - 4.
	boxInnerW := width - 6 // 2 border + 2 padding + 2 margin
	if boxInnerW < 20 {
		boxInnerW = 20
	}

	// Pad to fixed height so the box never changes size when scrolling.
	blankLine := strings.Repeat(" ", boxInnerW)
	paddedLines := make([]string, vis)
	for i := range vis {
		if i < len(visible) {
			lineW := runewidth.StringWidth(visible[i])
			if lineW < boxInnerW {
				paddedLines[i] = visible[i] + strings.Repeat(" ", boxInnerW-lineW)
			} else {
				paddedLines[i] = runewidth.Truncate(visible[i], boxInnerW, "")
			}
		} else {
			paddedLines[i] = blankLine
		}
	}

	content := strings.Join(paddedLines, "\n") + "\n" + footer
	box := helpBoxStyle.Width(boxInnerW).Render(content)

	// Overlay the box onto the background, same pattern as the palette.
	bgLines := strings.Split(base, "\n")
	for len(bgLines) < height {
		bgLines = append(bgLines, strings.Repeat(" ", width))
	}

	boxLines := strings.Split(box, "\n")
	topPad := height / 6
	if topPad+len(boxLines) > height {
		topPad = max(0, height-len(boxLines))
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

	var lines []string
	lines = append(lines, helpTitleStyle.Render("Keyboard Shortcuts"))
	lines = append(lines, "")

	for _, cat := range categories {
		items, ok := grouped[cat]
		if !ok {
			continue
		}
		lines = append(lines, helpCatStyle.Render(string(cat)))
		for _, b := range items {
			keyStr := strings.Join(b.Keys, ", ")
			padded := keyStr + strings.Repeat(" ", maxKeyWidth-runewidth.StringWidth(keyStr))
			lines = append(lines, "  "+helpKeyStyle.Render(padded)+"  "+helpDescStyle.Render(b.Label))
		}
		lines = append(lines, "")
	}

	return lines
}
