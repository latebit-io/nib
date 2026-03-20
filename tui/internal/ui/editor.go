// Package ui provides the Bubble Tea TUI components for junto.
package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/latebit-io/junto/tui/internal/editor/buffer"
	"github.com/latebit-io/junto/tui/internal/editor/highlight"
	"github.com/mattn/go-runewidth"
)

// EditorModel is the Bubble Tea model for the code editor pane.
type EditorModel struct {
	Buf    *buffer.Buffer
	Width  int
	Height int

	// Cursor position (0-indexed)
	CursorLine int
	CursorCol  int

	// Viewport scroll offset
	ScrollOffset int

	// Selection
	SelectionActive bool
	SelectStartLine int
	SelectStartCol  int

	// Syntax highlighting
	Highlighter  *highlight.Highlighter
	needsReparse bool

	// Transient status message (shown in status bar, cleared on next key)
	StatusMsg string
}

// NewEditorModel creates an editor model from a buffer.
func NewEditorModel(buf *buffer.Buffer) EditorModel {
	m := EditorModel{
		Buf:    buf,
		Width:  80,
		Height: 24,
	}
	if buf.Path != "" {
		m.Highlighter = highlight.New(buf.Path)
		if m.Highlighter != nil {
			m.Highlighter.Parse(buf.Content())
		}
	}
	return m
}

// VisibleLines returns the number of content lines visible (reserving 1 for status bar).
func (m *EditorModel) VisibleLines() int {
	return m.Height - 1
}

// GutterWidth returns the width of the line number gutter.
func (m *EditorModel) GutterWidth() int {
	digits := len(fmt.Sprintf("%d", m.Buf.LineCount()))
	if digits < 3 {
		digits = 3
	}
	return digits + 1 // +1 for space separator
}

// EnsureCursorVisible scrolls the viewport to keep the cursor visible.
func (m *EditorModel) EnsureCursorVisible() {
	vis := m.VisibleLines()
	if vis <= 0 {
		return
	}
	if m.CursorLine < m.ScrollOffset {
		m.ScrollOffset = m.CursorLine
	}
	if m.CursorLine >= m.ScrollOffset+vis {
		m.ScrollOffset = m.CursorLine - vis + 1
	}
}

// MoveCursor moves the cursor by the given delta, clamping to buffer bounds.
func (m *EditorModel) MoveCursor(dLine, dCol int) {
	m.CursorLine += dLine
	m.CursorCol += dCol

	// Clamp line
	if m.CursorLine < 0 {
		m.CursorLine = 0
	}
	if m.CursorLine >= m.Buf.LineCount() {
		m.CursorLine = m.Buf.LineCount() - 1
	}

	// Clamp col
	if m.CursorCol < 0 {
		// Wrap to end of previous line
		if dCol < 0 && m.CursorLine > 0 {
			m.CursorLine--
			m.CursorCol = m.Buf.LineLen(m.CursorLine)
		} else {
			m.CursorCol = 0
		}
	}
	lineLen := m.Buf.LineLen(m.CursorLine)
	if m.CursorCol > lineLen {
		// Wrap to start of next line
		if dCol > 0 && m.CursorLine < m.Buf.LineCount()-1 {
			m.CursorLine++
			m.CursorCol = 0
		} else {
			m.CursorCol = lineLen
		}
	}

	m.EnsureCursorVisible()
}

// MoveCursorTo sets the cursor to an absolute position.
func (m *EditorModel) MoveCursorTo(line, col int) {
	m.CursorLine = line
	m.CursorCol = col

	if m.CursorLine < 0 {
		m.CursorLine = 0
	}
	if m.CursorLine >= m.Buf.LineCount() {
		m.CursorLine = m.Buf.LineCount() - 1
	}
	if m.CursorCol < 0 {
		m.CursorCol = 0
	}
	lineLen := m.Buf.LineLen(m.CursorLine)
	if m.CursorCol > lineLen {
		m.CursorCol = lineLen
	}

	m.EnsureCursorVisible()
}

// MarkDirty flags the highlighter for reparse on next render.
func (m *EditorModel) MarkDirty() {
	m.needsReparse = true
}

// ReparseIfNeeded reparses the buffer for syntax highlighting.
func (m *EditorModel) ReparseIfNeeded() {
	if m.needsReparse && m.Highlighter != nil {
		m.Highlighter.Parse(m.Buf.Content())
		m.needsReparse = false
	}
}

// InsertChar inserts a character at the cursor position.
func (m *EditorModel) InsertChar(ch rune) {
	m.Buf.Insert(m.CursorLine, m.CursorCol, string(ch))
	m.CursorCol++
	m.MarkDirty()
}

// InsertNewline inserts a newline at the cursor, with auto-indent.
func (m *EditorModel) InsertNewline() {
	// Detect leading whitespace for auto-indent
	lineText := m.Buf.LineText(m.CursorLine)
	indent := ""
	for _, ch := range lineText {
		if ch == ' ' || ch == '\t' {
			indent += string(ch)
		} else {
			break
		}
	}

	m.Buf.Insert(m.CursorLine, m.CursorCol, "\n"+indent)
	m.CursorLine++
	m.CursorCol = len([]rune(indent))
	m.MarkDirty()
	m.EnsureCursorVisible()
}

// InsertTab inserts a tab (4 spaces) at the cursor.
func (m *EditorModel) InsertTab() {
	m.Buf.Insert(m.CursorLine, m.CursorCol, "    ")
	m.CursorCol += 4
	m.MarkDirty()
}

// Backspace deletes the character before the cursor.
func (m *EditorModel) Backspace() {
	if m.CursorCol > 0 {
		m.Buf.Delete(m.CursorLine, m.CursorCol-1, 1)
		m.CursorCol--
		m.MarkDirty()
	} else if m.CursorLine > 0 {
		prevLen := m.Buf.LineLen(m.CursorLine - 1)
		m.Buf.Delete(m.CursorLine-1, prevLen, 1)
		m.CursorLine--
		m.CursorCol = prevLen
		m.MarkDirty()
	}
	m.EnsureCursorVisible()
}

// DeleteChar deletes the character at the cursor.
func (m *EditorModel) DeleteChar() {
	lineLen := m.Buf.LineLen(m.CursorLine)
	if m.CursorCol < lineLen {
		m.Buf.Delete(m.CursorLine, m.CursorCol, 1)
		m.MarkDirty()
	} else if m.CursorLine < m.Buf.LineCount()-1 {
		m.Buf.Delete(m.CursorLine, m.CursorCol, 1)
		m.MarkDirty()
	}
}

// WordRight moves cursor to the start of the next word.
func (m *EditorModel) WordRight() {
	line := []rune(m.Buf.LineText(m.CursorLine))
	col := m.CursorCol

	// Skip current word chars
	for col < len(line) && !isWordSeparator(line[col]) {
		col++
	}
	// Skip whitespace
	for col < len(line) && isWordSeparator(line[col]) {
		col++
	}

	if col >= len(line) && m.CursorLine < m.Buf.LineCount()-1 {
		m.CursorLine++
		m.CursorCol = 0
	} else {
		m.CursorCol = col
	}
	m.EnsureCursorVisible()
}

// WordLeft moves cursor to the start of the previous word.
func (m *EditorModel) WordLeft() {
	line := []rune(m.Buf.LineText(m.CursorLine))
	col := m.CursorCol

	if col == 0 {
		if m.CursorLine > 0 {
			m.CursorLine--
			m.CursorCol = m.Buf.LineLen(m.CursorLine)
		}
		m.EnsureCursorVisible()
		return
	}

	col--
	// Skip whitespace backwards
	for col > 0 && isWordSeparator(line[col]) {
		col--
	}
	// Skip word chars backwards
	for col > 0 && !isWordSeparator(line[col-1]) {
		col--
	}

	m.CursorCol = col
	m.EnsureCursorVisible()
}

// Home moves cursor to start of line (or first non-whitespace).
func (m *EditorModel) Home() {
	line := m.Buf.LineText(m.CursorLine)
	firstNonWS := 0
	for _, ch := range line {
		if ch != ' ' && ch != '\t' {
			break
		}
		firstNonWS++
	}
	if m.CursorCol == firstNonWS {
		m.CursorCol = 0
	} else {
		m.CursorCol = firstNonWS
	}
}

// End moves cursor to end of line.
func (m *EditorModel) End() {
	m.CursorCol = m.Buf.LineLen(m.CursorLine)
}

// PageUp moves the cursor up by a page.
func (m *EditorModel) PageUp() {
	m.MoveCursor(-m.VisibleLines(), 0)
}

// PageDown moves the cursor down by a page.
func (m *EditorModel) PageDown() {
	m.MoveCursor(m.VisibleLines(), 0)
}

// StartSelection begins a selection at the current cursor position.
func (m *EditorModel) StartSelection() {
	if !m.SelectionActive {
		m.SelectionActive = true
		m.SelectStartLine = m.CursorLine
		m.SelectStartCol = m.CursorCol
	}
}

// ClearSelection clears the active selection.
func (m *EditorModel) ClearSelection() {
	m.SelectionActive = false
}

// SelectedRange returns the normalized (start, end) of the selection.
// Returns (startLine, startCol, endLine, endCol).
func (m *EditorModel) SelectedRange() (int, int, int, int) {
	if !m.SelectionActive {
		return m.CursorLine, m.CursorCol, m.CursorLine, m.CursorCol
	}
	sl, sc := m.SelectStartLine, m.SelectStartCol
	el, ec := m.CursorLine, m.CursorCol
	if sl > el || (sl == el && sc > ec) {
		sl, sc, el, ec = el, ec, sl, sc
	}
	return sl, sc, el, ec
}

// SelectedText returns the text in the current selection.
func (m *EditorModel) SelectedText() string {
	if !m.SelectionActive {
		return ""
	}
	sl, sc, el, ec := m.SelectedRange()
	if sl == el {
		runes := []rune(m.Buf.LineText(sl))
		lineLen := len(runes)
		if sc > lineLen {
			sc = lineLen
		}
		if ec > lineLen {
			ec = lineLen
		}
		if sc > ec {
			sc = ec
		}
		return string(runes[sc:ec])
	}

	var sb strings.Builder
	// First line
	first := []rune(m.Buf.LineText(sl))
	if sc > len(first) {
		sc = len(first)
	}
	sb.WriteString(string(first[sc:]))
	sb.WriteRune('\n')
	// Middle lines
	for i := sl + 1; i < el; i++ {
		sb.WriteString(m.Buf.LineText(i))
		sb.WriteRune('\n')
	}
	// Last line
	last := []rune(m.Buf.LineText(el))
	if ec > len(last) {
		ec = len(last)
	}
	sb.WriteString(string(last[:ec]))
	return sb.String()
}

// DeleteSelection deletes the selected text.
func (m *EditorModel) DeleteSelection() {
	if !m.SelectionActive {
		return
	}
	text := m.SelectedText()
	sl, sc, _, _ := m.SelectedRange()
	m.Buf.Delete(sl, sc, len([]rune(text)))
	m.CursorLine = sl
	m.CursorCol = sc
	m.SelectionActive = false
	m.EnsureCursorVisible()
}

// isSelected returns whether the given position is within the selection.
func (m *EditorModel) isSelected(line, col int) bool {
	if !m.SelectionActive {
		return false
	}
	sl, sc, el, ec := m.SelectedRange()
	if line < sl || line > el {
		return false
	}
	if line == sl && col < sc {
		return false
	}
	if line == el && col >= ec {
		return false
	}
	return true
}

// Render renders the editor view.
func (m *EditorModel) Render() string {
	m.ReparseIfNeeded()

	gutterW := m.GutterWidth()
	contentW := m.Width - gutterW
	if contentW < 1 {
		contentW = 1
	}

	vis := m.VisibleLines()
	gutterStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

	output := make([]string, m.Height)
	row := 0

	cursorStyle := lipgloss.NewStyle().Reverse(true)
	selectionStyle := lipgloss.NewStyle().Background(lipgloss.Color("24"))

	for i := range vis {
		if row >= m.Height {
			break
		}
		lineIdx := m.ScrollOffset + i
		var line strings.Builder

		if lineIdx >= m.Buf.LineCount() {
			gutter := gutterStyle.Render(fmt.Sprintf("%*s ", gutterW-1, "~"))
			line.WriteString(gutter)
			line.WriteString(strings.Repeat(" ", contentW))
		} else {
			gutter := gutterStyle.Render(fmt.Sprintf("%*d ", gutterW-1, lineIdx+1))
			line.WriteString(gutter)

			lineRunes := []rune(m.Buf.LineText(lineIdx))
			displayed := make([]rune, contentW)
			for j := range displayed {
				displayed[j] = ' '
			}
			for j := 0; j < len(lineRunes) && j < contentW; j++ {
				displayed[j] = lineRunes[j]
			}

			charStyles := make([]lipgloss.Style, contentW)
			if m.Highlighter != nil {
				tokens := m.Highlighter.HighlightLine(lineIdx)
				for _, tok := range tokens {
					for j := tok.Col; j < tok.Col+tok.Len && j < contentW; j++ {
						charStyles[j] = tok.Style
					}
				}
			}

			for j := range contentW {
				ch := string(displayed[j])
				isCursor := lineIdx == m.CursorLine && j == m.CursorCol
				isSel := m.isSelected(lineIdx, j)

				if isCursor {
					line.WriteString(cursorStyle.Render(ch))
				} else if isSel {
					line.WriteString(selectionStyle.Render(ch))
				} else if charStyles[j].GetForeground() != nil {
					line.WriteString(charStyles[j].Render(ch))
				} else {
					line.WriteString(ch)
				}
			}
		}

		output[row] = line.String()
		row++
	}

	// Fill any remaining rows (if vis < Height-1)
	for row < m.Height-1 {
		output[row] = gutterStyle.Render(fmt.Sprintf("%*s ", gutterW-1, "~")) + strings.Repeat(" ", contentW)
		row++
	}

	// Status bar (last row)
	if row < m.Height {
		output[row] = m.renderStatusBar()
	}

	return strings.Join(output, "\n")
}

func (m *EditorModel) renderStatusBar() string {
	statusStyle := lipgloss.NewStyle().
		Background(lipgloss.Color("62")).
		Foreground(lipgloss.Color("230")).
		Bold(true)

	name := m.Buf.Path
	if name == "" {
		name = "[new]"
	}
	modified := ""
	if m.Buf.Modified {
		modified = " [+]"
	}

	left := fmt.Sprintf(" %s%s", name, modified)
	if m.StatusMsg != "" {
		left += "  " + m.StatusMsg
	}
	right := fmt.Sprintf(" %d:%d ", m.CursorLine+1, m.CursorCol+1)

	leftW := runewidth.StringWidth(left)
	rightW := runewidth.StringWidth(right)
	padding := m.Width - leftW - rightW
	if padding < 0 {
		padding = 0
	}

	bar := left + strings.Repeat(" ", padding) + right
	bar = runewidth.Truncate(bar, m.Width, "")

	return statusStyle.Render(bar)
}

func isWordSeparator(r rune) bool {
	return r == ' ' || r == '\t' || r == '.' || r == ',' || r == ';' ||
		r == ':' || r == '(' || r == ')' || r == '[' || r == ']' ||
		r == '{' || r == '}' || r == '"' || r == '\''
}
