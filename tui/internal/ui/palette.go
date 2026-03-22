package ui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/latebit-io/junto/engine/fuzzy"
)

// PaletteItem represents an entry in the command palette.
type PaletteItem struct {
	Label    string // display text (file path, command name)
	Category string // "file", "command" — for future extensibility
	Value    string // action payload (absolute path, command ID)
}

// PaletteResultMsg is emitted when the user selects a palette item or cancels.
type PaletteResultMsg struct {
	Item      PaletteItem
	Category  string
	Cancelled bool
}

// PaletteModel manages the command palette overlay.
// It is purely presentational — filtering delegates to engine/fuzzy,
// file opening delegates to session via PaletteResultMsg.
type PaletteModel struct {
	Active       bool
	Query        string
	Items        []PaletteItem
	Filtered     []fuzzy.Match
	Selected     int
	ScrollOffset int
	Width        int
	Height       int
}

// Palette rendering constants.
const (
	paletteWidthRatio   = 0.6 // 60% of terminal width
	paletteMinWidth     = 40
	paletteMaxVisible   = 15
	paletteInputHeight  = 3 // border + input + border
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

	// Build labels for fuzzy filtering.
	labels := make([]string, len(items))
	for i, item := range items {
		labels[i] = item.Label
	}
	p.Filtered = fuzzy.Filter("", labels)
}

// Close deactivates the palette.
func (p *PaletteModel) Close() {
	p.Active = false
	p.Items = nil
	p.Filtered = nil
	p.Query = ""
}

// Update handles key input when the palette is active.
func (p *PaletteModel) Update(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
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

	case tea.KeyRunes:
		p.Query += string(msg.Runes)
		p.refilter()
		return nil
	}

	return nil
}

// refilter updates the filtered results based on the current query.
func (p *PaletteModel) refilter() {
	labels := make([]string, len(p.Items))
	for i, item := range p.Items {
		labels[i] = item.Label
	}
	p.Filtered = fuzzy.Filter(p.Query, labels)
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
	maxVis := p.maxVisible()
	if p.Selected < p.ScrollOffset {
		p.ScrollOffset = p.Selected
	}
	if p.Selected >= p.ScrollOffset+maxVis {
		p.ScrollOffset = p.Selected - maxVis + 1
	}
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

	// Full-width palette — no blank side gaps.
	boxWidth := width - 2
	if boxWidth < paletteMinWidth {
		boxWidth = paletteMinWidth
	}
	innerWidth := boxWidth - 2 // border only
	if innerWidth < 1 {
		innerWidth = 1
	}

	// Input line with cursor.
	queryDisplay := p.Query + " "
	if len(queryDisplay) > innerWidth {
		queryDisplay = queryDisplay[len(queryDisplay)-innerWidth:]
	}
	qRunes := []rune(queryDisplay)
	var inputLine strings.Builder
	for i, r := range qRunes {
		ch := string(r)
		if i == len([]rune(p.Query)) {
			inputLine.WriteString(paletteCursorStyle.Render(ch))
		} else {
			inputLine.WriteString(paletteInputStyle.Render(ch))
		}
	}
	inputRendered := inputLine.String()

	// Results list.
	maxVis := p.maxVisible()
	var resultLines []string
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

	for len(resultLines) < maxVis {
		resultLines = append(resultLines, strings.Repeat(" ", innerWidth))
	}

	footer := paletteDimStyle.Render(fmt.Sprintf(" %d / %d", len(p.Filtered), len(p.Items)))

	content := " " + inputRendered + "\n" +
		strings.Repeat("─", innerWidth) + "\n" +
		strings.Join(resultLines, "\n") + "\n" +
		" " + footer

	box := paletteBorderStyle.Width(innerWidth).Render(content)

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

	// Build a set of matched positions for O(1) lookup.
	posSet := make(map[int]bool, len(match.Positions))
	for _, pos := range match.Positions {
		posSet[pos] = true
	}

	var line strings.Builder
	for i, r := range runes {
		ch := string(r)
		isMatch := posSet[i]
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
