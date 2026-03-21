// Package ui provides the Bubble Tea TUI components for junto.
package ui

import (
	"fmt"
	"log/slog"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/latebit-io/junto/tui/internal/editor/buffer"
	"github.com/latebit-io/junto/tui/internal/editor/highlight"
	"github.com/mattn/go-runewidth"
)

// EditorModel is the Bubble Tea model for the code editor pane.
// It is a self-contained Pane: owns its own key handling, rendering,
// and buffer manipulation. AppModel delegates key events here when
// the editor has focus.
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

	// Keymap for action matching (shared with AppModel)
	Keymap *Keymap

	// Shared services (clipboard, etc.)
	Services *Services

	// Internal clipboard buffer
	Clipboard string
}

// NewEditorModel creates an editor model from a buffer.
func NewEditorModel(buf *buffer.Buffer, km *Keymap, svc *Services) *EditorModel {
	m := &EditorModel{
		Buf:      buf,
		Width:    80,
		Height:   24,
		Keymap:   km,
		Services: svc,
	}
	if buf.Path != "" {
		m.Highlighter = highlight.New(buf.Path)
		if m.Highlighter != nil {
			m.Highlighter.Parse(buf.Content())
		}
	}
	return m
}

// SetSize updates the editor dimensions and clamps scroll. Implements Pane.
func (m *EditorModel) SetSize(width, height int) {
	m.Width = width
	m.Height = height
	maxScroll := m.Buf.LineCount() - m.VisibleLines()
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.ScrollOffset > maxScroll {
		m.ScrollOffset = maxScroll
	}
}

// VisibleLines returns the number of content lines visible (reserving 1 for status bar).
func (m *EditorModel) VisibleLines() int {
	if m.Height <= 1 {
		return 0
	}
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
			gutterText := fmt.Sprintf("%*s ", gutterW-1, "~")
			line.WriteString(gutterStyle.Render(gutterText))
			line.WriteString(strings.Repeat(" ", contentW))
		} else {
			gutterText := fmt.Sprintf("%*d ", gutterW-1, lineIdx+1)
			line.WriteString(gutterStyle.Render(gutterText))

			// Build tab→display column mapping and expanded line
			rawRunes := []rune(m.Buf.LineText(lineIdx))
			var expandedRunes []rune
			// bufToDisp maps buffer col → display col
			bufToDisp := make([]int, len(rawRunes)+1)
			dispCol := 0
			for bi, r := range rawRunes {
				bufToDisp[bi] = dispCol
				if r == '\t' {
					expandedRunes = append(expandedRunes, ' ', ' ', ' ', ' ')
					dispCol += 4
				} else {
					expandedRunes = append(expandedRunes, r)
					dispCol++
				}
			}
			bufToDisp[len(rawRunes)] = dispCol

			displayed := make([]rune, contentW)
			for j := range displayed {
				displayed[j] = ' '
			}
			for j := 0; j < len(expandedRunes) && j < contentW; j++ {
				displayed[j] = expandedRunes[j]
			}

			// Map cursor and selection to display coords
			displayCursorCol := -1
			if lineIdx == m.CursorLine && m.CursorCol >= 0 && m.CursorCol <= len(rawRunes) {
				displayCursorCol = bufToDisp[m.CursorCol]
			}

			charStyles := make([]lipgloss.Style, contentW)
			if m.Highlighter != nil {
				tokens := m.Highlighter.HighlightLine(lineIdx)
				for _, tok := range tokens {
					// Token cols are in buffer coords — convert to display
					dStart := 0
					if tok.Col < len(bufToDisp) {
						dStart = bufToDisp[tok.Col]
					}
					dEnd := dStart + tok.Len
					tokEnd := tok.Col + tok.Len
					if tokEnd < len(bufToDisp) {
						dEnd = bufToDisp[tokEnd]
					}
					for j := dStart; j < dEnd && j < contentW; j++ {
						charStyles[j] = tok.Style
					}
				}
			}

			// Precompute inverse mapping: display col → buffer col (O(1) lookup in render loop)
			// bufToDisp has len(rawRunes)+1 entries; the last maps to the EOL position.
			dispToBuf := make([]int, contentW)
			if m.SelectionActive {
				bufCol := 0
				for j := range contentW {
					// Advance bufCol while the next buffer position maps to this display col or earlier.
					// Allow advancing to len(rawRunes) (EOL) so trailing spaces map correctly.
					for bufCol+1 <= len(rawRunes) && bufToDisp[bufCol+1] <= j {
						bufCol++
					}
					dispToBuf[j] = bufCol
				}
			}

			for j := range contentW {
				ch := string(displayed[j])
				isCursor := j == displayCursorCol
				isSel := m.SelectionActive && m.isSelected(lineIdx, dispToBuf[j])

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

// DisplayColToBufferCol converts a display column (after tab expansion) to a buffer column.
func (m *EditorModel) DisplayColToBufferCol(line, displayCol int) int {
	if line < 0 || line >= m.Buf.LineCount() {
		return 0
	}
	runes := []rune(m.Buf.LineText(line))
	dc := 0
	for bi, r := range runes {
		width := 1
		if r == '\t' {
			width = 4
		}
		if displayCol < dc+width {
			return bi
		}
		dc += width
	}
	return len(runes)
}

// Update handles key and mouse events for the editor pane. Implements Pane.
func (m *EditorModel) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.MouseMsg:
		return m.handleMouse(msg)
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return nil
}

func (m *EditorModel) handleMouse(msg tea.MouseMsg) tea.Cmd {
	scrollLines := 3

	switch {
	case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonWheelUp:
		m.ScrollOffset -= scrollLines
		if m.ScrollOffset < 0 {
			m.ScrollOffset = 0
		}
		return nil

	case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonWheelDown:
		maxScroll := m.Buf.LineCount() - m.VisibleLines()
		if maxScroll < 0 {
			maxScroll = 0
		}
		m.ScrollOffset += scrollLines
		if m.ScrollOffset > maxScroll {
			m.ScrollOffset = maxScroll
		}
		return nil
	}

	// Click/drag — local X coordinate (already translated by RegionManager)
	if msg.Button == tea.MouseButtonLeft && msg.Y < m.VisibleLines() {
		gutterW := m.GutterWidth()
		displayCol := msg.X - gutterW
		if displayCol < 0 {
			displayCol = 0
		}
		line := m.ScrollOffset + msg.Y
		// Clamp to valid buffer range before computing buffer column
		var bufCol int
		if line >= m.Buf.LineCount() {
			line = m.Buf.LineCount() - 1
			if line < 0 {
				line = 0
			}
			bufCol = m.Buf.LineLen(line) // click past EOF → end of last line
		} else {
			bufCol = m.DisplayColToBufferCol(line, displayCol)
		}

		switch msg.Action {
		case tea.MouseActionPress:
			m.ClearSelection()
			m.MoveCursorTo(line, bufCol)
			m.SelectionActive = true
			m.SelectStartLine = m.CursorLine
			m.SelectStartCol = m.CursorCol
		case tea.MouseActionMotion:
			if m.SelectionActive {
				m.MoveCursorTo(line, bufCol)
			}
		case tea.MouseActionRelease:
			if m.SelectionActive &&
				m.CursorLine == m.SelectStartLine &&
				m.CursorCol == m.SelectStartCol {
				m.ClearSelection()
			}
		}
	}

	return nil
}

func (m *EditorModel) handleKey(keyMsg tea.KeyMsg) tea.Cmd {

	m.StatusMsg = "" // clear transient status on any key

	isShift := keyMsg.Type == tea.KeyShiftUp || keyMsg.Type == tea.KeyShiftDown ||
		keyMsg.Type == tea.KeyShiftLeft || keyMsg.Type == tea.KeyShiftRight ||
		keyMsg.Type == tea.KeyShiftHome || keyMsg.Type == tea.KeyShiftEnd

	action := m.Keymap.Match(keyMsg)

	switch action {
	case ActionSave:
		if err := m.Buf.Save(); err != nil {
			m.StatusMsg = "Save failed: " + err.Error()
		} else {
			m.StatusMsg = "Saved"
		}
		return nil

	case ActionUndo:
		l, c, ok := m.Buf.Undo()
		if ok {
			m.ClearSelection()
			m.MoveCursorTo(l, c)
			m.MarkDirty()
		}
		return nil

	case ActionRedo:
		l, c, ok := m.Buf.Redo()
		if ok {
			m.ClearSelection()
			m.MoveCursorTo(l, c)
			m.MarkDirty()
		}
		return nil

	case ActionCopy:
		if m.SelectionActive {
			m.Clipboard = m.SelectedText()
			if err := m.Services.Clipboard.Write(m.Clipboard); err != nil {
				slog.Warn("system clipboard write failed", "err", err)
			}
		}
		return nil

	case ActionCut:
		if m.SelectionActive {
			m.Clipboard = m.SelectedText()
			if err := m.Services.Clipboard.Write(m.Clipboard); err != nil {
				slog.Warn("system clipboard write failed", "err", err)
			}
			m.DeleteSelection()
			m.MarkDirty()
		}
		return nil

	case ActionPaste:
		if sys := m.Services.Clipboard.Read(); sys != "" {
			m.Clipboard = sys
		}
		if m.Clipboard != "" {
			m.PasteText(m.Clipboard)
		}
		return nil

	case ActionSelectAll:
		m.SelectionActive = true
		m.SelectStartLine = 0
		m.SelectStartCol = 0
		lastLine := m.Buf.LineCount() - 1
		m.CursorLine = lastLine
		m.CursorCol = m.Buf.LineLen(lastLine)
		return nil
	}

	// Escape (clear selection) — only reached when AppModel has no pending edit
	if keyMsg.Type == tea.KeyEscape {
		m.ClearSelection()
		return nil
	}

	// Navigation and editing
	switch keyMsg.Type {

	// Selection navigation
	case tea.KeyShiftUp:
		m.StartSelection()
		m.MoveCursor(-1, 0)
		return nil
	case tea.KeyShiftDown:
		m.StartSelection()
		m.MoveCursor(1, 0)
		return nil
	case tea.KeyShiftLeft:
		m.StartSelection()
		m.MoveCursor(0, -1)
		return nil
	case tea.KeyShiftRight:
		m.StartSelection()
		m.MoveCursor(0, 1)
		return nil
	case tea.KeyShiftHome:
		m.StartSelection()
		m.Home()
		return nil
	case tea.KeyShiftEnd:
		m.StartSelection()
		m.End()
		return nil

	// Navigation
	case tea.KeyUp:
		m.ClearSelection()
		m.MoveCursor(-1, 0)
		return nil
	case tea.KeyDown:
		m.ClearSelection()
		m.MoveCursor(1, 0)
		return nil
	case tea.KeyLeft:
		m.ClearSelection()
		m.MoveCursor(0, -1)
		return nil
	case tea.KeyRight:
		m.ClearSelection()
		m.MoveCursor(0, 1)
		return nil
	case tea.KeyHome:
		m.ClearSelection()
		m.Home()
		return nil
	case tea.KeyEnd:
		m.ClearSelection()
		m.End()
		return nil
	case tea.KeyPgUp:
		m.ClearSelection()
		m.PageUp()
		return nil
	case tea.KeyPgDown:
		m.ClearSelection()
		m.PageDown()
		return nil

	// Word navigation
	case tea.KeyCtrlRight:
		m.ClearSelection()
		m.WordRight()
		return nil
	case tea.KeyCtrlLeft:
		m.ClearSelection()
		m.WordLeft()
		return nil

	// Editing
	case tea.KeyEnter:
		if m.SelectionActive {
			m.DeleteSelection()
		}
		m.InsertNewline()
		return nil
	case tea.KeyTab:
		if m.SelectionActive {
			m.DeleteSelection()
		}
		m.InsertTab()
		return nil
	case tea.KeyBackspace:
		if m.SelectionActive {
			m.DeleteSelection()
			m.MarkDirty()
		} else {
			m.Backspace()
		}
		return nil
	case tea.KeyDelete:
		if m.SelectionActive {
			m.DeleteSelection()
			m.MarkDirty()
		} else {
			m.DeleteChar()
		}
		return nil

	// Character input (also handles Cmd+V paste on macOS — arrives as multi-char KeyRunes)
	case tea.KeyRunes:
		// Drop leaked mouse escape sequence fragments (SGR: <N;N;NM)
		if isLeakedMouseSequence(keyMsg.Runes) {
			return nil
		}
		if len(keyMsg.Runes) > 1 {
			m.PasteText(string(keyMsg.Runes))
		} else {
			if m.SelectionActive {
				m.DeleteSelection()
			}
			for _, r := range keyMsg.Runes {
				m.InsertChar(r)
			}
		}
		return nil
	}

	if !isShift {
		m.ClearSelection()
	}

	return nil
}

// PasteText inserts text at the cursor, replacing any active selection.
func (m *EditorModel) PasteText(text string) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	if m.SelectionActive {
		m.DeleteSelection()
	}
	m.Buf.Insert(m.CursorLine, m.CursorCol, text)
	cl, cc := m.CursorLine, m.CursorCol
	for _, r := range text {
		if r == '\n' {
			cl++
			cc = 0
		} else {
			cc++
		}
	}
	m.MoveCursorTo(cl, cc)
	m.MarkDirty()
}

// ApplyEdit applies a search-and-replace edit to the buffer.
// Returns (true, "") on success, or (false, reason) on failure.
// Edits are grouped for undo and the cursor is moved to the edit location.
func (m *EditorModel) ApplyEdit(search, replace string) (bool, string) {
	content := m.Buf.Content()
	count := strings.Count(content, search)
	switch count {
	case 1:
		idx := strings.Index(content, search)
		line, col := 0, 0
		for _, r := range content[:idx] {
			if r == '\n' {
				line++
				col = 0
			} else {
				col++
			}
		}
		searchRunes := len([]rune(search))
		m.Buf.BeginGroup()
		m.Buf.Delete(line, col, searchRunes)
		m.Buf.Insert(line, col, replace)
		m.Buf.EndGroup()
		m.ClearSelection()
		m.MoveCursorTo(line, col)
		m.MarkDirty()
		return true, ""
	case 0:
		return false, "Edit could not be applied — text not found"
	default:
		return false, fmt.Sprintf("Edit could not be applied — %d matches found, expected 1", count)
	}
}

func isWordSeparator(r rune) bool {
	return r == ' ' || r == '\t' || r == '.' || r == ',' || r == ';' ||
		r == ':' || r == '(' || r == ')' || r == '[' || r == ']' ||
		r == '{' || r == '}' || r == '"' || r == '\''
}
