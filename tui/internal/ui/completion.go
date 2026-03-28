package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/latebit-io/junto/engine/lang"
	"github.com/mattn/go-runewidth"
)

// Completion popup styles — hoisted to package level.
var (
	completionStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("237")).
			Foreground(lipgloss.Color("252"))
	completionSelectedStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("62")).
				Foreground(lipgloss.Color("230"))
	completionBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("240"))
)

// maxCompletionVisible is the maximum number of items shown in the popup.
const maxCompletionVisible = 10

// CompletionPopup manages the state of the autocomplete popup.
// Language-agnostic — works with any lang.CompletionItem source.
type CompletionPopup struct {
	// Items is the full list of completion items from the language service.
	Items []lang.CompletionItem

	// Selected is the index of the currently highlighted item.
	Selected int

	// ScrollOffset is the first visible item index (for scrolling long lists).
	ScrollOffset int

	// Active is true when the popup is visible.
	Active bool

	// TriggerLine and TriggerCol record where the completion was triggered.
	// Used to detect staleness when the cursor moves.
	TriggerLine, TriggerCol int

	// Prefix is the text typed since the trigger point, used for filtering.
	Prefix string
}

// Show opens the popup with the given items at the cursor position.
func (c *CompletionPopup) Show(items []lang.CompletionItem, line, col int) {
	c.Items = items
	c.Selected = 0
	c.ScrollOffset = 0
	c.Active = true
	c.TriggerLine = line
	c.TriggerCol = col
	c.Prefix = ""
}

// Dismiss closes the popup.
func (c *CompletionPopup) Dismiss() {
	c.Active = false
	c.Items = nil
	c.Selected = 0
	c.ScrollOffset = 0
	c.Prefix = ""
}

// SelectNext moves the selection down, scrolling if needed.
func (c *CompletionPopup) SelectNext() {
	if len(c.Items) == 0 {
		return
	}
	c.Selected++
	if c.Selected >= len(c.Items) {
		c.Selected = 0
		c.ScrollOffset = 0
	}
	c.clampScroll()
}

// SelectPrev moves the selection up, scrolling if needed.
func (c *CompletionPopup) SelectPrev() {
	if len(c.Items) == 0 {
		return
	}
	c.Selected--
	if c.Selected < 0 {
		c.Selected = len(c.Items) - 1
		c.clampScroll()
	}
	c.clampScroll()
}

// SelectedItem returns the currently selected completion item, or nil.
func (c *CompletionPopup) SelectedItem() *lang.CompletionItem {
	if !c.Active || c.Selected < 0 || c.Selected >= len(c.Items) {
		return nil
	}
	return &c.Items[c.Selected]
}

// clampScroll ensures the selected item is visible.
func (c *CompletionPopup) clampScroll() {
	if c.Selected < c.ScrollOffset {
		c.ScrollOffset = c.Selected
	}
	if c.Selected >= c.ScrollOffset+maxCompletionVisible {
		c.ScrollOffset = c.Selected - maxCompletionVisible + 1
	}
}

// Render returns the popup as a styled string block.
// width is the max available width for the popup.
func (c *CompletionPopup) Render(width int) string {
	if !c.Active || len(c.Items) == 0 {
		return ""
	}

	maxWidth := width
	if maxWidth > 50 {
		maxWidth = 50
	}
	if maxWidth < 15 {
		return ""
	}
	// Content width inside border (2 chars).
	contentW := maxWidth - 2

	visCount := len(c.Items) - c.ScrollOffset
	if visCount > maxCompletionVisible {
		visCount = maxCompletionVisible
	}

	lines := make([]string, visCount)
	for i := range visCount {
		idx := c.ScrollOffset + i
		item := c.Items[idx]

		icon := completionKindIcon(item.Kind)

		// Build: "icon label  detail" — all on one line, truncated.
		// Sanitize external text to prevent ANSI/control sequence injection.
		label := sanitizeCompletionText(item.Label)
		detail := sanitizeCompletionText(item.Detail)
		text := icon + " " + label
		if detail != "" {
			text += " " + detail
		}
		text = runewidth.Truncate(text, contentW, "…")

		// Pad to contentW for consistent background.
		pad := contentW - runewidth.StringWidth(text)
		if pad > 0 {
			text += strings.Repeat(" ", pad)
		}

		if idx == c.Selected {
			lines[i] = completionSelectedStyle.Render(text)
		} else {
			lines[i] = completionStyle.Render(text)
		}
	}

	content := strings.Join(lines, "\n")
	return completionBorderStyle.Width(contentW).Render(content)
}

// sanitizeCompletionText strips ANSI escapes and control characters from
// language service text to prevent terminal injection.
func sanitizeCompletionText(s string) string {
	var b strings.Builder
	inEscape := false
	for _, r := range s {
		if inEscape {
			if r >= 0x40 && r <= 0x7e {
				inEscape = false
			}
			continue
		}
		if r == '\x1b' {
			inEscape = true
			continue
		}
		if r < ' ' && r != '\t' {
			continue // strip control chars except tab
		}
		b.WriteRune(r)
	}
	return b.String()
}

// completionKindIcon returns a single-char icon for a completion kind.
func completionKindIcon(kind lang.CompletionKind) string {
	switch kind {
	case lang.CompletionFunction:
		return "ƒ"
	case lang.CompletionMethod:
		return "m"
	case lang.CompletionVariable:
		return "v"
	case lang.CompletionField:
		return "·"
	case lang.CompletionType:
		return "T"
	case lang.CompletionConstant:
		return "c"
	case lang.CompletionModule:
		return "M"
	case lang.CompletionProperty:
		return "p"
	case lang.CompletionKeyword:
		return "k"
	case lang.CompletionSnippet:
		return "S"
	default:
		return " "
	}
}
