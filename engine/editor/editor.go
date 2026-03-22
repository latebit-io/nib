// Package editor provides a frontend-agnostic code editor controller.
// It manages cursor, selection, scroll, and text operations on top of a buffer.
// Frontends map their input events to Editor methods and read Editor state to render.
package editor

import (
	"fmt"
	"strings"

	"github.com/latebit-io/junto/engine/buffer"
	"github.com/latebit-io/junto/engine/highlight"
)

// Editor is the frontend-agnostic editor controller.
// It owns cursor state, selection, scroll position, and text operations.
// Frontends call methods to manipulate state and read fields to render.
type Editor struct {
	Buf    *buffer.Buffer
	Width  int
	Height int

	// Cursor position (0-indexed, in buffer coordinates)
	CursorLine int
	CursorCol  int

	// Viewport scroll offset (in visual-line space when ExtraVisualLines > 0)
	ScrollOffset int

	// ExtraVisualLines is the number of virtual lines inserted into the viewport
	// by the frontend (e.g., inline diff overlay). Scroll methods account for
	// these so the viewport can scroll through all visual content.
	ExtraVisualLines int

	// Selection
	SelectionActive bool
	SelectStartLine int
	SelectStartCol  int

	// Syntax highlighting (internal — use HighlightLine to access)
	highlighter  *highlight.Highlighter
	needsReparse bool
}

// New creates an editor wrapping the given buffer.
func New(buf *buffer.Buffer) *Editor {
	e := &Editor{
		Buf:    buf,
		Width:  80,
		Height: 24,
	}
	if buf.Path != "" {
		e.highlighter = highlight.New(buf.Path)
		if e.highlighter != nil {
			e.highlighter.Parse(buf.Content())
		}
	}
	return e
}

// Close frees native tree-sitter resources. Call on shutdown.
func (e *Editor) Close() {
	if e.highlighter != nil {
		e.highlighter.Close()
	}
}

// SetSize updates the editor dimensions and clamps scroll.
func (e *Editor) SetSize(width, height int) {
	e.Width = width
	e.Height = height
	e.ClampScroll()
}

// VisibleLines returns the number of content lines visible (reserving 1 for status bar).
func (e *Editor) VisibleLines() int {
	if e.Height <= 1 {
		return 0
	}
	return e.Height - 1
}

// GutterWidth returns the width of the line number gutter.
func (e *Editor) GutterWidth() int {
	digits := len(fmt.Sprintf("%d", e.Buf.LineCount()))
	if digits < 3 {
		digits = 3
	}
	return digits + 1 // +1 for space separator
}

// ContentWidth returns the width available for text content (total width minus gutter).
func (e *Editor) ContentWidth() int {
	w := e.Width - e.GutterWidth()
	if w < 1 {
		w = 1
	}
	return w
}

// totalVisualLines returns the total number of visual lines in the viewport,
// including any extra virtual lines set by the frontend.
func (e *Editor) totalVisualLines() int {
	return e.Buf.LineCount() + e.ExtraVisualLines
}

// ClampScroll clamps the scroll offset to valid range.
func (e *Editor) ClampScroll() {
	maxScroll := e.totalVisualLines() - e.VisibleLines()
	if maxScroll < 0 {
		maxScroll = 0
	}
	if e.ScrollOffset > maxScroll {
		e.ScrollOffset = maxScroll
	}
	if e.ScrollOffset < 0 {
		e.ScrollOffset = 0
	}
}

// EnsureCursorVisible scrolls the viewport to keep the cursor visible.
func (e *Editor) EnsureCursorVisible() {
	vis := e.VisibleLines()
	if vis <= 0 {
		return
	}
	if e.CursorLine < e.ScrollOffset {
		e.ScrollOffset = e.CursorLine
	}
	if e.CursorLine >= e.ScrollOffset+vis {
		e.ScrollOffset = e.CursorLine - vis + 1
	}
}

// --- Cursor Movement ---

// MoveCursor moves the cursor by the given delta, clamping to buffer bounds.
func (e *Editor) MoveCursor(dLine, dCol int) {
	e.CursorLine += dLine
	e.CursorCol += dCol

	// Clamp line
	if e.CursorLine < 0 {
		e.CursorLine = 0
	}
	if e.CursorLine >= e.Buf.LineCount() {
		e.CursorLine = e.Buf.LineCount() - 1
	}

	// Clamp col
	if e.CursorCol < 0 {
		// Wrap to end of previous line
		if dCol < 0 && e.CursorLine > 0 {
			e.CursorLine--
			e.CursorCol = e.Buf.LineLen(e.CursorLine)
		} else {
			e.CursorCol = 0
		}
	}
	lineLen := e.Buf.LineLen(e.CursorLine)
	if e.CursorCol > lineLen {
		// Wrap to start of next line
		if dCol > 0 && e.CursorLine < e.Buf.LineCount()-1 {
			e.CursorLine++
			e.CursorCol = 0
		} else {
			e.CursorCol = lineLen
		}
	}

	e.EnsureCursorVisible()
}

// MoveCursorTo sets the cursor to an absolute position.
func (e *Editor) MoveCursorTo(line, col int) {
	e.CursorLine = line
	e.CursorCol = col

	if e.CursorLine < 0 {
		e.CursorLine = 0
	}
	if e.CursorLine >= e.Buf.LineCount() {
		e.CursorLine = e.Buf.LineCount() - 1
	}
	if e.CursorCol < 0 {
		e.CursorCol = 0
	}
	lineLen := e.Buf.LineLen(e.CursorLine)
	if e.CursorCol > lineLen {
		e.CursorCol = lineLen
	}

	e.EnsureCursorVisible()
}

// WordRight moves cursor to the start of the next word.
func (e *Editor) WordRight() {
	line := []rune(e.Buf.LineText(e.CursorLine))
	col := e.CursorCol

	// Skip current word chars
	for col < len(line) && !IsWordSeparator(line[col]) {
		col++
	}
	// Skip whitespace
	for col < len(line) && IsWordSeparator(line[col]) {
		col++
	}

	if col >= len(line) && e.CursorLine < e.Buf.LineCount()-1 {
		e.CursorLine++
		e.CursorCol = 0
	} else {
		e.CursorCol = col
	}
	e.EnsureCursorVisible()
}

// WordLeft moves cursor to the start of the previous word.
func (e *Editor) WordLeft() {
	line := []rune(e.Buf.LineText(e.CursorLine))
	col := e.CursorCol

	if col == 0 {
		if e.CursorLine > 0 {
			e.CursorLine--
			e.CursorCol = e.Buf.LineLen(e.CursorLine)
		}
		e.EnsureCursorVisible()
		return
	}

	col--
	// Skip whitespace backwards
	for col > 0 && IsWordSeparator(line[col]) {
		col--
	}
	// Skip word chars backwards
	for col > 0 && !IsWordSeparator(line[col-1]) {
		col--
	}

	e.CursorCol = col
	e.EnsureCursorVisible()
}

// Home moves cursor to start of line (or first non-whitespace).
func (e *Editor) Home() {
	line := e.Buf.LineText(e.CursorLine)
	firstNonWS := 0
	for _, ch := range line {
		if ch != ' ' && ch != '\t' {
			break
		}
		firstNonWS++
	}
	if e.CursorCol == firstNonWS {
		e.CursorCol = 0
	} else {
		e.CursorCol = firstNonWS
	}
}

// End moves cursor to end of line.
func (e *Editor) End() {
	e.CursorCol = e.Buf.LineLen(e.CursorLine)
}

// PageUp moves the cursor up by a page.
func (e *Editor) PageUp() {
	e.MoveCursor(-e.VisibleLines(), 0)
}

// PageDown moves the cursor down by a page.
func (e *Editor) PageDown() {
	e.MoveCursor(e.VisibleLines(), 0)
}

// ScrollUp scrolls the viewport up by the given number of lines.
func (e *Editor) ScrollUp(lines int) {
	e.ScrollOffset -= lines
	if e.ScrollOffset < 0 {
		e.ScrollOffset = 0
	}
}

// ScrollDown scrolls the viewport down by the given number of lines.
func (e *Editor) ScrollDown(lines int) {
	e.ScrollOffset += lines
	e.ClampScroll()
}

// --- Selection ---

// StartSelection begins a selection at the current cursor position.
func (e *Editor) StartSelection() {
	if !e.SelectionActive {
		e.SelectionActive = true
		e.SelectStartLine = e.CursorLine
		e.SelectStartCol = e.CursorCol
	}
}

// ClearSelection clears the active selection.
func (e *Editor) ClearSelection() {
	e.SelectionActive = false
}

// SelectAll selects all text in the buffer.
func (e *Editor) SelectAll() {
	e.SelectionActive = true
	e.SelectStartLine = 0
	e.SelectStartCol = 0
	lastLine := e.Buf.LineCount() - 1
	e.CursorLine = lastLine
	e.CursorCol = e.Buf.LineLen(lastLine)
}

// SelectedRange returns the normalized (start, end) of the selection.
// Returns (startLine, startCol, endLine, endCol).
func (e *Editor) SelectedRange() (int, int, int, int) {
	if !e.SelectionActive {
		return e.CursorLine, e.CursorCol, e.CursorLine, e.CursorCol
	}
	sl, sc := e.SelectStartLine, e.SelectStartCol
	el, ec := e.CursorLine, e.CursorCol
	if sl > el || (sl == el && sc > ec) {
		sl, sc, el, ec = el, ec, sl, sc
	}
	return sl, sc, el, ec
}

// SelectedText returns the text in the current selection.
func (e *Editor) SelectedText() string {
	if !e.SelectionActive {
		return ""
	}
	sl, sc, el, ec := e.SelectedRange()
	if sl == el {
		runes := []rune(e.Buf.LineText(sl))
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
	first := []rune(e.Buf.LineText(sl))
	if sc > len(first) {
		sc = len(first)
	}
	sb.WriteString(string(first[sc:]))
	sb.WriteRune('\n')
	// Middle lines
	for i := sl + 1; i < el; i++ {
		sb.WriteString(e.Buf.LineText(i))
		sb.WriteRune('\n')
	}
	// Last line
	last := []rune(e.Buf.LineText(el))
	if ec > len(last) {
		ec = len(last)
	}
	sb.WriteString(string(last[:ec]))
	return sb.String()
}

// DeleteSelection deletes the selected text.
func (e *Editor) DeleteSelection() {
	if !e.SelectionActive {
		return
	}
	text := e.SelectedText()
	sl, sc, _, _ := e.SelectedRange()
	e.Buf.Delete(sl, sc, len([]rune(text)))
	e.CursorLine = sl
	e.CursorCol = sc
	e.SelectionActive = false
	e.EnsureCursorVisible()
}

// IsSelected returns whether the given position is within the selection.
func (e *Editor) IsSelected(line, col int) bool {
	if !e.SelectionActive {
		return false
	}
	sl, sc, el, ec := e.SelectedRange()
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

// --- Text Operations ---

// InsertChar inserts a character at the cursor position.
func (e *Editor) InsertChar(ch rune) {
	e.Buf.Insert(e.CursorLine, e.CursorCol, string(ch))
	e.CursorCol++
	e.MarkDirty()
}

// InsertNewline inserts a newline at the cursor, with auto-indent.
func (e *Editor) InsertNewline() {
	// Detect leading whitespace for auto-indent, capped at cursor column
	// so that splitting a whitespace-only line doesn't accumulate spaces.
	runes := []rune(e.Buf.LineText(e.CursorLine))
	indent := ""
	for i, ch := range runes {
		if i >= e.CursorCol {
			break
		}
		if ch == ' ' || ch == '\t' {
			indent += string(ch)
		} else {
			break
		}
	}

	e.Buf.Insert(e.CursorLine, e.CursorCol, "\n"+indent)
	e.CursorLine++
	e.CursorCol = len([]rune(indent))
	e.MarkDirty()
	e.EnsureCursorVisible()
}

// InsertTab inserts a tab (4 spaces) at the cursor.
func (e *Editor) InsertTab() {
	e.Buf.Insert(e.CursorLine, e.CursorCol, "    ")
	e.CursorCol += 4
	e.MarkDirty()
}

// Backspace deletes the character before the cursor.
func (e *Editor) Backspace() {
	if e.CursorCol > 0 {
		e.Buf.Delete(e.CursorLine, e.CursorCol-1, 1)
		e.CursorCol--
		e.MarkDirty()
	} else if e.CursorLine > 0 {
		prevLen := e.Buf.LineLen(e.CursorLine - 1)
		e.Buf.Delete(e.CursorLine-1, prevLen, 1)
		e.CursorLine--
		e.CursorCol = prevLen
		e.MarkDirty()
	}
	e.EnsureCursorVisible()
}

// DeleteChar deletes the character at the cursor.
func (e *Editor) DeleteChar() {
	lineLen := e.Buf.LineLen(e.CursorLine)
	if e.CursorCol < lineLen {
		e.Buf.Delete(e.CursorLine, e.CursorCol, 1)
		e.MarkDirty()
	} else if e.CursorLine < e.Buf.LineCount()-1 {
		e.Buf.Delete(e.CursorLine, e.CursorCol, 1)
		e.MarkDirty()
	}
}

// PasteText inserts text at the cursor, replacing any active selection.
func (e *Editor) PasteText(text string) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	if e.SelectionActive {
		e.DeleteSelection()
	}
	e.Buf.Insert(e.CursorLine, e.CursorCol, text)
	cl, cc := e.CursorLine, e.CursorCol
	for _, r := range text {
		if r == '\n' {
			cl++
			cc = 0
		} else {
			cc++
		}
	}
	e.MoveCursorTo(cl, cc)
	e.MarkDirty()
}

// Undo undoes the last operation and moves cursor to the undo position.
func (e *Editor) Undo() bool {
	l, c, ok := e.Buf.Undo()
	if ok {
		e.ClearSelection()
		e.MoveCursorTo(l, c)
		e.MarkDirty()
	}
	return ok
}

// Redo redoes the last undone operation and moves cursor to the redo position.
func (e *Editor) Redo() bool {
	l, c, ok := e.Buf.Redo()
	if ok {
		e.ClearSelection()
		e.MoveCursorTo(l, c)
		e.MarkDirty()
	}
	return ok
}

// Save saves the buffer to disk. Returns nil on success.
func (e *Editor) Save() error {
	return e.Buf.Save()
}

// ApplyEdit applies a search-and-replace edit to the buffer.
// Returns (true, "") on success, or (false, reason) on failure.
// Edits are grouped for undo and the cursor is moved to the edit location.
func (e *Editor) ApplyEdit(search, replace string) (bool, string) {
	content := e.Buf.Content()
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
		e.Buf.BeginGroup()
		e.Buf.Delete(line, col, searchRunes)
		e.Buf.Insert(line, col, replace)
		e.Buf.EndGroup()
		e.ClearSelection()
		e.MoveCursorTo(line, col)
		e.MarkDirty()
		return true, ""
	case 0:
		return false, "Edit could not be applied — text not found"
	default:
		return false, fmt.Sprintf("Edit could not be applied — %d matches found, expected 1", count)
	}
}

// --- Highlight ---

// Token re-exports highlight.Token for frontends that need token data.
type Token = highlight.Token

// TokenKind re-exports highlight.TokenKind for frontends that map to styles.
type TokenKind = highlight.TokenKind

// Token kind constants — re-exported for frontend use.
const (
	KindKeyword  = highlight.KindKeyword
	KindString   = highlight.KindString
	KindComment  = highlight.KindComment
	KindNumber   = highlight.KindNumber
	KindType     = highlight.KindType
	KindOperator = highlight.KindOperator
	KindNone     = highlight.KindNone
)

// MarkDirty flags the highlighter for reparse on next ReparseIfNeeded call.
func (e *Editor) MarkDirty() {
	e.needsReparse = true
}

// ReparseIfNeeded reparses the buffer for syntax highlighting.
func (e *Editor) ReparseIfNeeded() {
	if e.needsReparse && e.highlighter != nil {
		e.highlighter.Parse(e.Buf.Content())
		e.needsReparse = false
	}
}

// HighlightLine returns syntax tokens for the given line.
// Returns nil if no highlighter is configured.
// Calls ReparseIfNeeded internally so the caller doesn't have to.
func (e *Editor) HighlightLine(line int) []Token {
	e.ReparseIfNeeded()
	if e.highlighter == nil {
		return nil
	}
	return e.highlighter.HighlightLine(line)
}

// --- Display Helpers ---

// DisplayColToBufferCol converts a display column (after tab expansion) to a buffer column.
func (e *Editor) DisplayColToBufferCol(line, displayCol int) int {
	if line < 0 || line >= e.Buf.LineCount() {
		return 0
	}
	runes := []rune(e.Buf.LineText(line))
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

// IsWordSeparator returns true if the rune is a word boundary character.
func IsWordSeparator(r rune) bool {
	return r == ' ' || r == '\t' || r == '.' || r == ',' || r == ';' ||
		r == ':' || r == '(' || r == ')' || r == '[' || r == ']' ||
		r == '{' || r == '}' || r == '"' || r == '\''
}
