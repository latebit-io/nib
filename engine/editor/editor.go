// Package editor provides a frontend-agnostic code editor controller.
// It manages cursor, selection, scroll, and text operations on top of a buffer.
// Frontends map their input events to Editor methods and read Editor state to render.
package editor

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/latebit-io/junto/engine/buffer"
	"github.com/latebit-io/junto/engine/highlight"
)

// TabWidth is the display width of a tab character. Frontends must use
// this constant in their tab-expansion logic so horizontal scroll stays
// in sync with the engine's column calculations.
const TabWidth = 4

// scrollMarginCols is the horizontal lookahead margin. When the cursor
// approaches the viewport edge, we scroll early so the developer can
// see surrounding context rather than typing blind at the edge.
const scrollMarginCols = 8

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

	// Viewport scroll offset (in visual-line space when extraVisualLines > 0)
	ScrollOffset int

	// ScrollCol is the horizontal scroll offset in display-column space
	// (post tab-expansion). The TUI slices rendered content starting at
	// this column. Adjusted automatically by EnsureCursorVisible.
	ScrollCol int

	// extraVisualLines is the number of virtual lines inserted into the viewport
	// by the frontend (e.g., inline diff overlay). Set via SetExtraVisualLines;
	// scroll methods account for these so the viewport scrolls through all
	// visual content. Defaults to 0 (no overlay).
	extraVisualLines int

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

// SetSize updates the editor dimensions and clamps scroll on both axes.
func (e *Editor) SetSize(width, height int) {
	e.Width = width
	e.Height = height
	e.ClampScroll()
	e.ClampScrollCol()
}

// VisibleLines returns the number of content lines visible in the viewport.
func (e *Editor) VisibleLines() int {
	if e.Height <= 0 {
		return 0
	}
	return e.Height
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

// SetExtraVisualLines sets the number of virtual lines the frontend has
// inserted into the viewport (e.g., inline diff overlay). Scroll methods
// account for these so the viewport scrolls through all visual content.
func (e *Editor) SetExtraVisualLines(n int) {
	e.extraVisualLines = max(n, 0)
}

// ExtraVisualLines returns the current number of virtual lines.
func (e *Editor) ExtraVisualLines() int { return e.extraVisualLines }

// totalVisualLines returns the total number of visual lines in the viewport,
// including any extra virtual lines set by the frontend.
func (e *Editor) totalVisualLines() int {
	return e.Buf.LineCount() + e.extraVisualLines
}

// ClampScroll clamps the vertical scroll offset to valid range.
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

// ClampScrollCol ensures the horizontal scroll offset is non-negative.
// No upper bound is enforced — lines have varying lengths.
func (e *Editor) ClampScrollCol() {
	if e.ScrollCol < 0 {
		e.ScrollCol = 0
	}
}

// BufferColToDisplayCol converts a buffer column on the given line to a
// display column, accounting for tab expansion and wide characters.
// This is the engine-side equivalent of the TUI's expandTabs mapping.
func (e *Editor) BufferColToDisplayCol(line, bufCol int) int {
	if line < 0 || line >= e.Buf.LineCount() {
		return bufCol
	}
	text := e.Buf.LineText(line)
	runes := []rune(text)
	dispCol := 0
	for i := 0; i < bufCol && i < len(runes); i++ {
		switch runes[i] {
		case '\t':
			dispCol += TabWidth
		case '\uFE0F':
			// VS16 is skipped in the display buffer (see TUI expandTabs),
			// so it contributes 0 display columns.
		default:
			dispCol++
		}
	}
	return dispCol
}

// ScrollLeft scrolls the viewport left by the given number of columns.
func (e *Editor) ScrollLeft(cols int) {
	e.ScrollCol -= cols
	e.ClampScrollCol()
}

// ScrollRight scrolls the viewport right by the given number of columns.
func (e *Editor) ScrollRight(cols int) {
	e.ScrollCol += cols
}

// CollapseOverlay adjusts ScrollOffset when an overlay (inline diff) is
// removed from the viewport. The overlay injects virtual lines between
// startLine and endLine; removing them requires converting from visual-line
// space back to buffer-line space.
//
// Parameters:
//   - startLine: first buffer line of the overlay's removed range
//   - endLine: last buffer line of the overlay's removed range (inclusive)
//   - addedCount: number of virtual lines the overlay injected
//   - bufferMutated: true when the buffer already contains the replacement
//     (approve path); false when the buffer is unchanged (reject/error/done)
//
// CollapseOverlay also resets extraVisualLines to 0.
func (e *Editor) CollapseOverlay(startLine, endLine, addedCount int, bufferMutated bool) {
	addedEnd := endLine + addedCount

	if bufferMutated {
		// After approve: removed lines are gone, added lines are now real
		// buffer lines.
		removedCount := endLine - startLine + 1
		if e.ScrollOffset > addedEnd {
			// Past the overlay: subtract removedCount (virtual removed lines gone).
			e.ScrollOffset -= removedCount
		} else if e.ScrollOffset > endLine {
			// In the added-lines zone: map to replacement position.
			e.ScrollOffset = startLine + (e.ScrollOffset - endLine - 1)
		} else if e.ScrollOffset >= startLine {
			// In the removed range: those lines no longer exist.
			// Clamp to startLine (start of the replacement content).
			e.ScrollOffset = startLine
		}
	} else {
		// Reject/error/done: buffer unchanged. Subtract addedCount
		// (the virtual overlay lines that are being removed).
		if e.ScrollOffset > addedEnd {
			e.ScrollOffset -= addedCount
		} else if e.ScrollOffset > endLine {
			e.ScrollOffset = endLine + 1
		}
	}
	e.extraVisualLines = 0
}

// EnsureCursorVisible scrolls the viewport on both axes to keep the cursor visible.
func (e *Editor) EnsureCursorVisible() {
	// Vertical
	vis := e.VisibleLines()
	if vis > 0 {
		if e.CursorLine < e.ScrollOffset {
			e.ScrollOffset = e.CursorLine
		}
		if e.CursorLine >= e.ScrollOffset+vis {
			e.ScrollOffset = e.CursorLine - vis + 1
		}
	}

	// Horizontal
	cw := e.ContentWidth()
	dispCol := e.BufferColToDisplayCol(e.CursorLine, e.CursorCol)
	margin := scrollMarginCols
	if margin >= cw {
		margin = 0 // terminal too narrow for margin
	}

	if dispCol < e.ScrollCol {
		e.ScrollCol = dispCol
	}
	if dispCol >= e.ScrollCol+cw-margin {
		e.ScrollCol = dispCol - cw + margin + 1
	}
	e.ClampScrollCol()
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

// --- File Navigation ---

// FileStart moves the cursor to the beginning of the file.
func (e *Editor) FileStart() {
	e.MoveCursorTo(0, 0)
}

// FileEnd moves the cursor to the end of the file.
func (e *Editor) FileEnd() {
	lastLine := e.Buf.LineCount() - 1
	e.MoveCursorTo(lastLine, e.Buf.LineLen(lastLine))
}

// --- Line Operations ---

// selectedLineRange returns the inclusive line range for the active selection,
// normalizing half-open selections. SelectLine() places the cursor at (nextLine, 0)
// which makes SelectedRange() return endLine as the next line. When endCol is 0
// and the selection spans multiple lines, the cursor is at the start of a line
// it doesn't actually select — so we decrement endLine.
func (e *Editor) selectedLineRange() (int, int) {
	startLine, _, endLine, endCol := e.SelectedRange()
	if endCol == 0 && endLine > startLine {
		endLine--
	}
	return startLine, endLine
}

// DeleteLine deletes the current line (or all selected lines).
// The operation is grouped for undo.
func (e *Editor) DeleteLine() {
	startLine, endLine := e.CursorLine, e.CursorLine
	if e.SelectionActive {
		startLine, endLine = e.selectedLineRange()
		e.SelectionActive = false
	}

	e.Buf.BeginGroup()
	for line := endLine; line >= startLine; line-- {
		lineText := e.Buf.LineText(line)
		lineLen := utf8.RuneCountInString(lineText)
		if line < e.Buf.LineCount()-1 {
			// Not the last line: delete text + newline
			e.Buf.Delete(line, 0, lineLen+1)
		} else if line > 0 {
			// Last line: delete newline from previous line + text
			e.Buf.Delete(line-1, e.Buf.LineLen(line-1), lineLen+1)
		} else {
			// Only line: clear it
			e.Buf.Delete(0, 0, lineLen)
		}
	}
	e.Buf.EndGroup()

	if startLine >= e.Buf.LineCount() {
		startLine = e.Buf.LineCount() - 1
	}
	e.CursorLine = startLine
	lineLen := e.Buf.LineLen(e.CursorLine)
	if e.CursorCol > lineLen {
		e.CursorCol = lineLen
	}
	e.MarkDirty()
	e.EnsureCursorVisible()
}

// SwapLineUp swaps the current line (or selected lines) with the line above.
func (e *Editor) SwapLineUp() {
	startLine, endLine := e.CursorLine, e.CursorLine
	if e.SelectionActive {
		startLine, endLine = e.selectedLineRange()
	}
	if startLine == 0 {
		return
	}

	aboveText := e.Buf.LineText(startLine - 1)
	aboveLen := utf8.RuneCountInString(aboveText)

	e.Buf.BeginGroup()
	// Delete the line above (text + newline)
	e.Buf.Delete(startLine-1, 0, aboveLen+1)
	// Insert it after the (now shifted) block
	insertLine := endLine - 1
	insertCol := e.Buf.LineLen(insertLine)
	e.Buf.Insert(insertLine, insertCol, "\n"+aboveText)
	e.Buf.EndGroup()

	e.CursorLine--
	if e.SelectionActive {
		e.SelectStartLine--
	}
	e.MarkDirty()
	e.EnsureCursorVisible()
}

// SwapLineDown swaps the current line (or selected lines) with the line below.
func (e *Editor) SwapLineDown() {
	startLine, endLine := e.CursorLine, e.CursorLine
	if e.SelectionActive {
		startLine, endLine = e.selectedLineRange()
	}
	if endLine >= e.Buf.LineCount()-1 {
		return
	}

	belowText := e.Buf.LineText(endLine + 1)
	belowLen := utf8.RuneCountInString(belowText)

	e.Buf.BeginGroup()
	// Delete the line below (newline + text)
	e.Buf.Delete(endLine, e.Buf.LineLen(endLine), belowLen+1)
	// Insert it before the block
	e.Buf.Insert(startLine, 0, belowText+"\n")
	e.Buf.EndGroup()

	e.CursorLine++
	if e.SelectionActive {
		e.SelectStartLine++
	}
	e.MarkDirty()
	e.EnsureCursorVisible()
}

// DuplicateLine duplicates the current line (or selected lines) below.
func (e *Editor) DuplicateLine() {
	startLine, endLine := e.CursorLine, e.CursorLine
	if e.SelectionActive {
		startLine, endLine = e.selectedLineRange()
	}

	var sb strings.Builder
	for line := startLine; line <= endLine; line++ {
		sb.WriteString(e.Buf.LineText(line))
		if line < endLine {
			sb.WriteRune('\n')
		}
	}
	dupText := sb.String()
	lineCount := endLine - startLine + 1

	e.Buf.BeginGroup()
	endCol := e.Buf.LineLen(endLine)
	e.Buf.Insert(endLine, endCol, "\n"+dupText)
	e.Buf.EndGroup()

	e.CursorLine += lineCount
	if e.SelectionActive {
		e.SelectStartLine += lineCount
	}
	e.MarkDirty()
	e.EnsureCursorVisible()
}

// ToggleLineComment toggles a line comment prefix on the current line or selection.
// If all lines in the range are commented, the prefix is removed; otherwise it is added.
func (e *Editor) ToggleLineComment(prefix string) {
	startLine, endLine := e.CursorLine, e.CursorLine
	if e.SelectionActive {
		startLine, endLine = e.selectedLineRange()
	}

	prefixWithSpace := prefix + " "

	// Check if all lines are commented.
	allCommented := true
	for line := startLine; line <= endLine; line++ {
		text := e.Buf.LineText(line)
		trimmed := strings.TrimLeft(text, " \t")
		if !strings.HasPrefix(trimmed, prefix) {
			allCommented = false
			break
		}
	}

	e.Buf.BeginGroup()
	if allCommented {
		// Remove comment prefix from each line.
		for line := startLine; line <= endLine; line++ {
			text := e.Buf.LineText(line)
			indent := len(text) - len(strings.TrimLeft(text, " \t"))
			runeIndent := utf8.RuneCountInString(text[:indent])
			if strings.HasPrefix(text[indent:], prefixWithSpace) {
				e.Buf.Delete(line, runeIndent, utf8.RuneCountInString(prefixWithSpace))
			} else if strings.HasPrefix(text[indent:], prefix) {
				e.Buf.Delete(line, runeIndent, utf8.RuneCountInString(prefix))
			}
		}
	} else {
		// Add comment prefix to each line.
		for line := endLine; line >= startLine; line-- {
			text := e.Buf.LineText(line)
			indent := len(text) - len(strings.TrimLeft(text, " \t"))
			runeIndent := utf8.RuneCountInString(text[:indent])
			e.Buf.Insert(line, runeIndent, prefixWithSpace)
		}
	}
	e.Buf.EndGroup()

	e.MarkDirty()
}

// IndentSelection indents all lines in the selection by inserting tabStr at column 0.
func (e *Editor) IndentSelection(tabStr string) {
	if !e.SelectionActive {
		return
	}
	startLine, endLine := e.selectedLineRange()
	tabRunes := utf8.RuneCountInString(tabStr)

	e.Buf.BeginGroup()
	for line := startLine; line <= endLine; line++ {
		e.Buf.Insert(line, 0, tabStr)
	}
	e.Buf.EndGroup()

	// Shift both anchor and cursor columns by the indent width,
	// preserving the original selection direction.
	e.SelectStartCol += tabRunes
	e.CursorCol += tabRunes
	e.MarkDirty()
}

// OutdentSelection removes one level of indentation from all selected lines.
func (e *Editor) OutdentSelection(tabStr string) {
	if !e.SelectionActive {
		return
	}
	startLine, endLine := e.selectedLineRange()
	tabRunes := utf8.RuneCountInString(tabStr)

	// Track actual columns removed for anchor and cursor lines
	// so we shift their columns by the right amount.
	anchorShift := 0
	cursorShift := 0

	e.Buf.BeginGroup()
	for line := startLine; line <= endLine; line++ {
		text := e.Buf.LineText(line)
		removed := 0
		if strings.HasPrefix(text, tabStr) {
			e.Buf.Delete(line, 0, tabRunes)
			removed = tabRunes
		} else {
			// Remove as many leading spaces as possible (up to tabRunes).
			for _, ch := range text {
				if ch == ' ' && removed < tabRunes {
					removed++
				} else {
					break
				}
			}
			if removed > 0 {
				e.Buf.Delete(line, 0, removed)
			}
		}
		if line == e.SelectStartLine {
			anchorShift = removed
		}
		if line == e.CursorLine {
			cursorShift = removed
		}
	}
	e.Buf.EndGroup()

	// Shift columns back by what was actually removed. Preserve selection direction.
	e.SelectStartCol -= anchorShift
	if e.SelectStartCol < 0 {
		e.SelectStartCol = 0
	}
	e.CursorCol -= cursorShift
	if e.CursorCol < 0 {
		e.CursorCol = 0
	}
	e.MarkDirty()
}

// SelectLine selects the current line. Repeated calls extend the selection down.
func (e *Editor) SelectLine() {
	lastLine := e.Buf.LineCount() - 1

	// If already selecting full lines and cursor is at col 0, extend down.
	if e.SelectionActive && e.SelectStartCol == 0 &&
		e.CursorCol == 0 && e.CursorLine > e.SelectStartLine {
		if e.CursorLine <= lastLine {
			if e.CursorLine < lastLine {
				e.CursorLine++
			} else {
				e.CursorCol = e.Buf.LineLen(lastLine)
			}
		}
		return
	}

	e.SelectionActive = true
	e.SelectStartLine = e.CursorLine
	e.SelectStartCol = 0
	if e.CursorLine < lastLine {
		e.CursorLine++
		e.CursorCol = 0
	} else {
		e.CursorCol = e.Buf.LineLen(lastLine)
	}
}

// SelectNextOccurrence selects the next occurrence of the current selection.
// If nothing is selected, selects the word under the cursor.
func (e *Editor) SelectNextOccurrence() {
	if !e.SelectionActive {
		e.selectWordUnderCursor()
		return
	}

	needle := e.SelectedText()
	if needle == "" {
		return
	}
	needleRunes := utf8.RuneCountInString(needle)

	_, _, endLine, endCol := e.SelectedRange()

	// Search forward from end of current selection.
	for line := endLine; line < e.Buf.LineCount(); line++ {
		runes := []rune(e.Buf.LineText(line))
		startCol := 0
		if line == endLine {
			startCol = endCol
		}
		for col := startCol; col <= len(runes)-needleRunes; col++ {
			if string(runes[col:col+needleRunes]) == needle {
				e.SelectStartLine = line
				e.SelectStartCol = col
				e.CursorLine = line
				e.CursorCol = col + needleRunes
				e.EnsureCursorVisible()
				return
			}
		}
	}

	// Wrap: search from beginning up to original selection start.
	sl, sc, _, _ := e.SelectedRange()
	for line := 0; line <= sl; line++ {
		runes := []rune(e.Buf.LineText(line))
		maxCol := len(runes) - needleRunes
		if line == sl {
			maxCol = sc - 1
		}
		for col := 0; col <= maxCol; col++ {
			if string(runes[col:col+needleRunes]) == needle {
				e.SelectStartLine = line
				e.SelectStartCol = col
				e.CursorLine = line
				e.CursorCol = col + needleRunes
				e.EnsureCursorVisible()
				return
			}
		}
	}
}

// selectWordUnderCursor selects the word at the cursor position.
func (e *Editor) selectWordUnderCursor() {
	runes := []rune(e.Buf.LineText(e.CursorLine))
	if len(runes) == 0 {
		return
	}
	col := e.CursorCol
	if col >= len(runes) {
		col = len(runes) - 1
	}
	if IsWordSeparator(runes[col]) {
		return
	}

	// Find word boundaries.
	start := col
	for start > 0 && !IsWordSeparator(runes[start-1]) {
		start--
	}
	end := col
	for end < len(runes) && !IsWordSeparator(runes[end]) {
		end++
	}

	e.SelectionActive = true
	e.SelectStartLine = e.CursorLine
	e.SelectStartCol = start
	e.CursorCol = end
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
	e.Buf.Delete(sl, sc, utf8.RuneCountInString(text))
	e.Buf.ResetOriginToDeveloper(sl)
	e.CursorLine = sl
	e.CursorCol = sc
	e.SelectionActive = false
	e.MarkDirty()
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
	e.Buf.ResetOriginToDeveloper(e.CursorLine)
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
	e.Buf.ResetOriginToDeveloper(e.CursorLine)
	e.CursorLine++
	e.Buf.ResetOriginToDeveloper(e.CursorLine)
	e.CursorCol = utf8.RuneCountInString(indent)
	e.MarkDirty()
	e.EnsureCursorVisible()
}

// InsertTab inserts a tab (4 spaces) at the cursor.
func (e *Editor) InsertTab() {
	e.Buf.Insert(e.CursorLine, e.CursorCol, "    ")
	e.Buf.ResetOriginToDeveloper(e.CursorLine)
	e.CursorCol += 4
	e.MarkDirty()
}

// Backspace deletes the character before the cursor.
func (e *Editor) Backspace() {
	if e.CursorCol > 0 {
		e.Buf.Delete(e.CursorLine, e.CursorCol-1, 1)
		e.Buf.ResetOriginToDeveloper(e.CursorLine)
		e.CursorCol--
		e.MarkDirty()
	} else if e.CursorLine > 0 {
		prevLen := e.Buf.LineLen(e.CursorLine - 1)
		e.Buf.Delete(e.CursorLine-1, prevLen, 1)
		e.CursorLine--
		e.Buf.ResetOriginToDeveloper(e.CursorLine)
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
		e.Buf.ResetOriginToDeveloper(e.CursorLine)
		e.MarkDirty()
	} else if e.CursorLine < e.Buf.LineCount()-1 {
		e.Buf.Delete(e.CursorLine, e.CursorCol, 1)
		e.Buf.ResetOriginToDeveloper(e.CursorLine)
		e.MarkDirty()
	}
}

// PasteText inserts text at the cursor, replacing any active selection.
// MaxPasteBytes caps a single paste operation to prevent unbounded buffer
// growth from large terminal pastes or clipboard content.
const MaxPasteBytes = 10 << 20 // 10 MiB

func (e *Editor) PasteText(text string) {
	if len(text) > MaxPasteBytes {
		// Truncate on a valid UTF-8 boundary.
		text = text[:MaxPasteBytes]
		for len(text) > 0 && !utf8.Valid([]byte(text)) {
			text = text[:len(text)-1]
		}
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	if e.SelectionActive {
		e.DeleteSelection()
	}
	startLine := e.CursorLine
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
	// All pasted lines become developer-owned
	for i := startLine; i <= cl; i++ {
		e.Buf.ResetOriginToDeveloper(i)
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

// EditLocation describes where a search string was found in the buffer.
type EditLocation struct {
	Line int // 0-indexed line
	Col  int // 0-indexed rune column
}

// LocateEdit finds the unique occurrence of search in the buffer.
// Returns the location and "" on success, or nil and a reason on failure.
func (e *Editor) LocateEdit(search string) (*EditLocation, string) {
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
		return &EditLocation{Line: line, Col: col}, ""
	case 0:
		return nil, "Edit could not be applied — text not found"
	default:
		return nil, fmt.Sprintf("Edit could not be applied — %d matches found, expected 1", count)
	}
}

// ApplyEdit applies a search-and-replace edit to the buffer.
// Returns (true, "") on success, or (false, reason) on failure.
// Edits are grouped for undo and the cursor is moved to the edit location.
// lineOrigins is a per-line origin slice for the replacement text (index 0 =
// first replacement line). Nil slice means no origin changes. A nil entry
// within the slice means "don't change this line's origin" (unchanged line).
func (e *Editor) ApplyEdit(search, replace string, lineOrigins []*buffer.Origin) (bool, string) {
	loc, reason := e.LocateEdit(search)
	if loc == nil {
		return false, reason
	}
	searchRunes := utf8.RuneCountInString(search)
	e.Buf.BeginGroup()
	e.Buf.Delete(loc.Line, loc.Col, searchRunes)
	e.Buf.Insert(loc.Line, loc.Col, replace)
	for i, origin := range lineOrigins {
		if origin != nil {
			e.Buf.SetLineOrigin(loc.Line+i, *origin)
		}
	}
	e.Buf.EndGroup()
	e.ClearSelection()
	e.MoveCursorTo(loc.Line, loc.Col)
	e.MarkDirty()
	return true, ""
}

// --- Spatial Queries ---

// CursorInRegion reports whether a cursor at (cursorLine, cursorCol) falls
// within the region bounded by (startLine, startCol) to (endLine, endCol),
// inclusive. This is a pure geometric check — frontends use it for collision
// detection between the developer cursor and agent-active regions.
func CursorInRegion(cursorLine, cursorCol, startLine, startCol, endLine, endCol int) bool {
	if cursorLine < startLine || cursorLine > endLine {
		return false
	}
	// Single-line region: both bounds on the same line.
	if startLine == endLine {
		return cursorCol >= startCol && cursorCol <= endCol
	}
	// Multi-line region: check boundary columns on first/last lines,
	// interior lines are fully within.
	if cursorLine == startLine {
		return cursorCol >= startCol
	}
	if cursorLine == endLine {
		return cursorCol <= endCol
	}
	return true
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
		var width int
		switch r {
		case '\t':
			width = TabWidth
		case '\uFE0F':
			width = 0 // VS16 is stripped from display (see expandTabs)
		default:
			width = 1
		}
		if width > 0 && displayCol < dc+width {
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
