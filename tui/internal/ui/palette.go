package ui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/latebit-io/junto/engine/fuzzy"
)

// PaletteItem represents an entry in the command palette.
type PaletteItem struct {
	// Label is the display text shown in the palette (file path, command name).
	Label string
	// Category classifies the item type ("file", "command") for routing.
	Category string
	// Value is the action payload dispatched on selection (absolute path, command ID).
	Value string
}

// PaletteResultMsg is emitted when the user selects a palette item or cancels.
type PaletteResultMsg struct {
	// Item is the palette entry the user selected.
	Item PaletteItem
	// Category is copied from the selected item for convenient routing.
	Category string
	// Cancelled is true when the user dismissed the palette without selecting.
	Cancelled bool
}

// PaletteModel manages the command palette overlay.
// It is purely presentational — filtering delegates to engine/fuzzy,
// file opening delegates to session via PaletteResultMsg.
type PaletteModel struct {
	// Active is true when the palette overlay is visible and capturing input.
	Active bool
	// Query is the current user-typed search string.
	Query string
	// Items is the full set of palette entries provided on Open.
	Items  []PaletteItem
	labels []string // cached labels for fuzzy filtering
	// Filtered holds the fuzzy-matched results for the current query.
	Filtered []fuzzy.Match
	// Selected is the index into Filtered of the highlighted entry.
	Selected int
	// ScrollOffset is the first visible index in the results list.
	ScrollOffset int
	// Width is the terminal width available for rendering the overlay.
	Width int
	// Height is the terminal height available for rendering the overlay.
	Height int
}

// Palette rendering constants.
const (
	paletteMinWidth     = 40
	paletteMaxVisible   = 15
	paletteInputHeight  = 2 // input line + separator line (outer border is separate)
	paletteFooterHeight = 1
)

// Palette styles — reused across frames.
var (
	paletteBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("62"))
	paletteInputStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("230"))
	paletteSelectedStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("62")).
				Foreground(lipgloss.Color("230"))
	paletteNormalStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("252"))
	paletteMatchStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("213")).
				Bold(true)
	paletteMatchSelectedStyle = lipgloss.NewStyle().
					Background(lipgloss.Color("62")).
					Foreground(lipgloss.Color("213")).
					Bold(true)
	paletteDimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240"))
	paletteCursorStyle = lipgloss.NewStyle().
				Reverse(true)
)

// Open activates the palette with a set of items.
func (p *PaletteModel) Open(items []PaletteItem) {
	p.Active = true
	p.Query = ""
	p.Items = items
	p.Selected = 0
	p.ScrollOffset = 0

	// Cache labels for reuse across refilter calls.
	p.labels = make([]string, len(items))
	for i, item := range items {
		p.labels[i] = item.Label
	}
	p.Filtered = fuzzy.Filter("", p.labels)
}

// Close deactivates the palette.
func (p *PaletteModel) Close() {
	p.Active = false
	p.Items = nil
	p.labels = nil
	p.Filtered = nil
	p.Query = ""
}

// Update handles key input when the palette is active.
func (p *PaletteModel) Update(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.Code {
	case tea.KeyEscape:
		p.Close()
		return func() tea.Msg { return PaletteResultMsg{Cancelled: true} }

	case tea.KeyEnter:
		if len(p.Filtered) > 0 && p.Selected < len(p.Filtered) {
			match := p.Filtered[p.Selected]
			item := p.findItem(match.Text)
			p.Close()
			return func() tea.Msg {
				return PaletteResultMsg{Item: item, Category: item.Category}
			}
		}
		return nil

	case tea.KeyUp:
		if p.Selected > 0 {
			p.Selected--
			p.ensureSelectedVisible()
		}
		return nil

	case tea.KeyDown:
		if p.Selected < len(p.Filtered)-1 {
			p.Selected++
			p.ensureSelectedVisible()
		}
		return nil

	case tea.KeyBackspace:
		if len(p.Query) > 0 {
			runes := []rune(p.Query)
			p.Query = string(runes[:len(runes)-1])
			p.refilter()
		}
		return nil
	}

	// Printable text input
	if msg.Text != "" {
		if next, changed := appendPaletteQuery(p.Query, msg.Text); changed {
			p.Query = next
			p.refilter()
		}
		return nil
	}

	return nil
}

// paletteMaxQueryRunes caps the palette query to a reasonable length.
const paletteMaxQueryRunes = 4096

// appendPaletteQuery appends printable runes from src to dst, stripping
// control characters and enforcing paletteMaxQueryRunes.
func appendPaletteQuery(dst, src string) (string, bool) {
	q := []rune(dst)
	orig := len(q)
	for _, r := range src {
		if r < ' ' || r == 0x7f {
			continue
		}
		if len(q) >= paletteMaxQueryRunes {
			break
		}
		q = append(q, r)
	}
	return string(q), len(q) != orig
}

// refilter updates the filtered results based on the current query.
func (p *PaletteModel) refilter() {
	p.Filtered = fuzzy.Filter(p.Query, p.labels)
	p.Selected = 0
	p.ScrollOffset = 0
}

// findItem returns the PaletteItem matching the given label.
func (p *PaletteModel) findItem(label string) PaletteItem {
	for _, item := range p.Items {
		if item.Label == label {
			return item
		}
	}
	return PaletteItem{}
}

// ensureSelectedVisible adjusts scroll so the selected item is visible.
func (p *PaletteModel) ensureSelectedVisible() {
	p.ScrollOffset = clampScrollOffset(p.Selected, p.ScrollOffset, p.maxVisible())
}

func (p *PaletteModel) maxVisible() int {
	// Calculate from available height minus input and footer.
	available := p.Height - paletteInputHeight - paletteFooterHeight - 2 // borders
	if available > paletteMaxVisible {
		available = paletteMaxVisible
	}
	if available < 1 {
		available = 1
	}
	return available
}

// RenderOverlay draws the palette as a floating overlay on top of the
// background view. The editor remains visible behind the palette box.
func (p *PaletteModel) RenderOverlay(background string, width, height int) string {
	p.Width = width
	p.Height = height

	// Near-full-width palette with 1-char margin on each side.
	boxWidth := width - 2
	boxWidth = max(boxWidth, paletteMinWidth)
	boxWidth = min(boxWidth, width) // never exceed terminal width
	innerWidth := boxWidth - 2      // border only
	if innerWidth < 1 {
		innerWidth = 1
	}

	// Input line with cursor — all operations in rune space.
	qRunes := []rune(p.Query)
	// Append a space for the cursor position at the end.
	displayRunes := append(qRunes, ' ')
	cursorIdx := len(qRunes) // cursor is on the trailing space
	// Truncate from the left if too wide, keeping the cursor visible.
	// Reserve 1 column for left padding (matching renderMatch).
	maxInputWidth := max(1, innerWidth-1)
	if len(displayRunes) > maxInputWidth {
		start := len(displayRunes) - maxInputWidth
		displayRunes = displayRunes[start:]
		cursorIdx = len(displayRunes) - 1
	}
	var inputLine strings.Builder
	for i, r := range displayRunes {
		ch := string(r)
		if i == cursorIdx {
			inputLine.WriteString(paletteCursorStyle.Render(ch))
		} else {
			inputLine.WriteString(paletteInputStyle.Render(ch))
		}
	}
	inputRendered := inputLine.String()

	// Results list.
	maxVis := p.maxVisible()
	resultLines := make([]string, 0, maxVis)
	end := p.ScrollOffset + maxVis
	if end > len(p.Filtered) {
		end = len(p.Filtered)
	}

	for i := p.ScrollOffset; i < end; i++ {
		match := p.Filtered[i]
		isSelected := i == p.Selected
		line := p.renderMatch(match, isSelected, innerWidth)
		resultLines = append(resultLines, line)
	}

	blankLine := strings.Repeat(" ", innerWidth)
	for len(resultLines) < maxVis {
		resultLines = append(resultLines, blankLine)
	}

	footer := paletteDimStyle.Render(fmt.Sprintf(" %d / %d", len(p.Filtered), len(p.Items)))

	content := " " + inputRendered + "\n" +
		" " + strings.Repeat("─", max(0, innerWidth-1)) + "\n" +
		strings.Join(resultLines, "\n") + "\n" +
		" " + footer

	box := paletteBorderStyle.Width(boxWidth).Render(content)

	// Overlay the palette onto the background. The editor stays visible
	// above and below the palette. Palette rows replace background rows
	// entirely — Bubble Tea can't do per-cell ANSI compositing.
	bgLines := strings.Split(background, "\n")
	for len(bgLines) < height {
		bgLines = append(bgLines, strings.Repeat(" ", width))
	}

	boxLines := strings.Split(box, "\n")
	boxH := len(boxLines)

	topPad := height / 4
	if topPad+boxH > height {
		topPad = max(0, height-boxH)
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

// renderMatch renders a single result line with match positions highlighted.
func (p *PaletteModel) renderMatch(match fuzzy.Match, selected bool, maxWidth int) string {
	// Reserve 1 char for left padding.
	textWidth := maxWidth - 1
	if textWidth < 1 {
		textWidth = 1
	}
	runes := []rune(match.Text)
	if len(runes) > textWidth {
		runes = runes[:textWidth]
	}

	// Walk positions slice in sync with runes — both are in ascending order.
	posIdx := 0
	var line strings.Builder
	for i, r := range runes {
		ch := string(r)
		isMatch := posIdx < len(match.Positions) && match.Positions[posIdx] == i
		if isMatch {
			posIdx++
		}
		switch {
		case selected && isMatch:
			line.WriteString(paletteMatchSelectedStyle.Render(ch))
		case selected:
			line.WriteString(paletteSelectedStyle.Render(ch))
		case isMatch:
			line.WriteString(paletteMatchStyle.Render(ch))
		default:
			line.WriteString(paletteNormalStyle.Render(ch))
		}
	}

	// Pad to textWidth.
	remaining := textWidth - len(runes)
	if remaining > 0 {
		pad := strings.Repeat(" ", remaining)
		if selected {
			line.WriteString(paletteSelectedStyle.Render(pad))
		} else {
			line.WriteString(pad)
		}
	}

	return " " + line.String()
}
