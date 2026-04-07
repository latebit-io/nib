package textarea

import (
	"log/slog"
	"strings"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/latebit-io/junto/engine/editor"
	"github.com/mattn/go-runewidth"
)

// Clipboard abstracts system clipboard access for copy/cut/paste.
type Clipboard interface {
	Read() string
	Write(s string) error
}

// SubmitMsg is returned as a tea.Cmd when the user presses Enter.
// The parent pane should read Content() to get the submitted text.
type SubmitMsg struct{}

// CancelMsg is returned as a tea.Cmd when the user presses Escape.
type CancelMsg struct{}

// DefaultMaxBytes is the default cap on total content size.
const DefaultMaxBytes = 1 << 20 // 1 MiB

// TextArea is a multiline text editing component with cursor navigation,
// selection, word boundaries, and soft wrapping.
type TextArea struct {
	// Content — stored as logical lines (explicit newlines from Shift+Enter).
	lines [][]rune

	// Cursor — position in logical line/col coordinates.
	cursorLine int
	cursorCol  int

	// Selection — anchor point, same coordinate space as cursor.
	selActive     bool
	selAnchorLine int
	selAnchorCol  int

	// Display width for soft wrapping (excludes any prefix the parent adds).
	width int

	// Wrap cache — visual lines derived from logical lines + width.
	wrapCache []visualLine
	wrapDirty bool

	// Limits
	maxBytes int
	byteLen  int // tracked incrementally to avoid O(n) totalBytes() per edit

	// Clipboard service (injected by parent).
	clipboard Clipboard

	// Reusable render buffer — avoids allocation per frame.
	renderBuf []RenderedLine
}

// New creates a TextArea with the given display width.
func New(width int) *TextArea {
	return &TextArea{
		lines:     [][]rune{{}},
		width:     width,
		maxBytes:  DefaultMaxBytes,
		wrapDirty: true,
	}
}

// SetSize updates the display width and invalidates the wrap cache.
func (t *TextArea) SetSize(width int) {
	if width != t.width {
		t.width = width
		t.wrapDirty = true
	}
}

// SetClipboard injects the clipboard service for copy/cut/paste.
func (t *TextArea) SetClipboard(cb Clipboard) {
	t.clipboard = cb
}

// SetContent replaces all content with the given text.
func (t *TextArea) SetContent(text string) {
	t.lines = splitLines(text)
	if len(t.lines) == 0 {
		t.lines = [][]rune{{}}
	}
	t.cursorLine = len(t.lines) - 1
	t.cursorCol = len(t.lines[t.cursorLine])
	t.selActive = false
	t.wrapDirty = true
	t.recomputeByteLen()
}

// Content returns the full text with explicit newlines.
func (t *TextArea) Content() string {
	var sb strings.Builder
	for i, line := range t.lines {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(string(line))
	}
	return sb.String()
}

// Reset clears all content, cursor, and selection.
func (t *TextArea) Reset() {
	t.lines = [][]rune{{}}
	t.cursorLine = 0
	t.cursorCol = 0
	t.selActive = false
	t.wrapDirty = true
	t.byteLen = 0
}

// CursorPosition returns the visual (wrapped) row and column for cursor display.
func (t *TextArea) CursorPosition() (visualRow, visualCol int) {
	return t.logicalToVisual(t.cursorLine, t.cursorCol)
}

// VisualLineCount returns the total number of visual (wrapped) lines.
func (t *TextArea) VisualLineCount() int {
	return t.visualLineCount()
}

// RenderedLine is a single visual line for display.
type RenderedLine struct {
	Text string
}

// Render returns the visual lines for display. The parent handles
// prefixing ("> "), cursor styling, and padding.
func (t *TextArea) Render() []RenderedLine {
	t.buildWrapCache()
	if cap(t.renderBuf) < len(t.wrapCache) {
		t.renderBuf = make([]RenderedLine, len(t.wrapCache))
	} else {
		t.renderBuf = t.renderBuf[:len(t.wrapCache)]
	}
	for i, vl := range t.wrapCache {
		t.renderBuf[i] = RenderedLine{Text: string(vl.runes)}
	}
	return t.renderBuf
}

// Update processes a key press. Returns a tea.Cmd for submit/cancel,
// or nil for internal edits.
func (t *TextArea) Update(msg tea.KeyPressMsg) tea.Cmd {
	// Ctrl+key shortcuts.
	if msg.Mod&tea.ModCtrl != 0 && msg.Mod&tea.ModShift == 0 && msg.Mod&tea.ModAlt == 0 {
		switch msg.Code {
		case 'a':
			t.selectAll()
			return nil
		case 'c':
			t.copySelection()
			return nil
		case 'x':
			t.cutSelection()
			return nil
		case 'v':
			t.paste()
			return nil
		}
	}

	// Word selection: Ctrl+Shift+Arrow or Alt+Shift+Arrow.
	if msg.Mod&tea.ModShift != 0 && (msg.Mod&tea.ModCtrl != 0 || msg.Mod&tea.ModAlt != 0) {
		switch msg.Code {
		case tea.KeyLeft, 'b':
			t.startSelection()
			t.wordLeft()
			return nil
		case tea.KeyRight, 'f':
			t.startSelection()
			t.wordRight()
			return nil
		}
	}

	// Word navigation: Ctrl+Arrow or Alt+Arrow (no shift).
	if msg.Mod&tea.ModShift == 0 && (msg.Mod&tea.ModCtrl != 0 || msg.Mod&tea.ModAlt != 0) {
		switch msg.Code {
		case tea.KeyLeft, 'b':
			t.clearSelection()
			t.wordLeft()
			return nil
		case tea.KeyRight, 'f':
			t.clearSelection()
			t.wordRight()
			return nil
		}
	}

	// Shift+Arrow: extend selection.
	isShift := msg.Mod&tea.ModShift != 0

	switch msg.Code {
	case tea.KeyEnter:
		if isShift {
			t.deleteSelectionIfActive()
			t.insertNewline()
			return nil
		}
		return func() tea.Msg { return SubmitMsg{} }

	case tea.KeyEscape:
		return func() tea.Msg { return CancelMsg{} }

	case tea.KeyLeft:
		if isShift {
			t.startSelection()
		} else {
			t.clearSelection()
		}
		t.moveLeft()
		return nil

	case tea.KeyRight:
		if isShift {
			t.startSelection()
		} else {
			t.clearSelection()
		}
		t.moveRight()
		return nil

	case tea.KeyUp:
		if isShift {
			t.startSelection()
		} else {
			t.clearSelection()
		}
		t.moveUp()
		return nil

	case tea.KeyDown:
		if isShift {
			t.startSelection()
		} else {
			t.clearSelection()
		}
		t.moveDown()
		return nil

	case tea.KeyHome:
		if isShift {
			t.startSelection()
		} else {
			t.clearSelection()
		}
		t.home()
		return nil

	case tea.KeyEnd:
		if isShift {
			t.startSelection()
		} else {
			t.clearSelection()
		}
		t.end()
		return nil

	case tea.KeyBackspace:
		t.collapseEmptySelection()
		if t.selActive {
			t.deleteSelection()
		} else {
			t.backspace()
		}
		return nil

	case tea.KeyDelete:
		t.collapseEmptySelection()
		if t.selActive {
			t.deleteSelection()
		} else {
			t.deleteForward()
		}
		return nil

	case tea.KeySpace:
		t.deleteSelectionIfActive()
		t.insertRune(' ')
		return nil
	}

	// Printable text (including bracket paste from terminal).
	if msg.Text != "" {
		t.deleteSelectionIfActive()
		t.insertText(msg.Text)
		return nil
	}

	return nil
}

// --- Cursor Movement ---

func (t *TextArea) moveLeft() {
	if t.cursorCol > 0 {
		t.cursorCol--
	} else if t.cursorLine > 0 {
		t.cursorLine--
		t.cursorCol = len(t.lines[t.cursorLine])
	}
}

func (t *TextArea) moveRight() {
	if t.cursorCol < len(t.lines[t.cursorLine]) {
		t.cursorCol++
	} else if t.cursorLine < len(t.lines)-1 {
		t.cursorLine++
		t.cursorCol = 0
	}
}

func (t *TextArea) moveUp() {
	visRow, visCol := t.logicalToVisual(t.cursorLine, t.cursorCol)
	if visRow == 0 {
		return
	}
	t.cursorLine, t.cursorCol = t.visualToLogical(visRow-1, visCol)
}

func (t *TextArea) moveDown() {
	visRow, visCol := t.logicalToVisual(t.cursorLine, t.cursorCol)
	if visRow >= t.visualLineCount()-1 {
		return
	}
	t.cursorLine, t.cursorCol = t.visualToLogical(visRow+1, visCol)
}

func (t *TextArea) home() {
	visRow, _ := t.logicalToVisual(t.cursorLine, t.cursorCol)
	t.cursorLine, t.cursorCol = t.visualToLogical(visRow, 0)
}

func (t *TextArea) end() {
	visRow, _ := t.logicalToVisual(t.cursorLine, t.cursorCol)
	t.buildWrapCache()
	if visRow < len(t.wrapCache) {
		vl := t.wrapCache[visRow]
		t.cursorLine = vl.logicalLine
		t.cursorCol = vl.runeOffset + len(vl.runes)
	}
}

func (t *TextArea) wordLeft() {
	// Skip separators backward.
	for t.cursorCol > 0 || t.cursorLine > 0 {
		if t.cursorCol == 0 {
			break // stop at line boundary
		}
		r := t.lines[t.cursorLine][t.cursorCol-1]
		if !editor.IsWordSeparator(r) {
			break
		}
		t.cursorCol--
	}
	// Skip word chars backward.
	for t.cursorCol > 0 {
		r := t.lines[t.cursorLine][t.cursorCol-1]
		if editor.IsWordSeparator(r) {
			break
		}
		t.cursorCol--
	}
}

func (t *TextArea) wordRight() {
	line := t.lines[t.cursorLine]
	// Skip word chars forward.
	for t.cursorCol < len(line) {
		if editor.IsWordSeparator(line[t.cursorCol]) {
			break
		}
		t.cursorCol++
	}
	// Skip separators forward.
	for t.cursorCol < len(line) {
		if !editor.IsWordSeparator(line[t.cursorCol]) {
			break
		}
		t.cursorCol++
	}
}

// --- Editing ---

func (t *TextArea) insertRune(r rune) {
	n := utf8.RuneLen(r)
	if t.byteLen+n > t.maxBytes {
		return
	}
	line := t.lines[t.cursorLine]
	newLine := make([]rune, len(line)+1)
	copy(newLine, line[:t.cursorCol])
	newLine[t.cursorCol] = r
	copy(newLine[t.cursorCol+1:], line[t.cursorCol:])
	t.lines[t.cursorLine] = newLine
	t.cursorCol++
	t.byteLen += n
	t.wrapDirty = true
}

func (t *TextArea) insertNewline() {
	if t.byteLen+1 > t.maxBytes {
		return
	}
	line := t.lines[t.cursorLine]
	before := make([]rune, t.cursorCol)
	copy(before, line[:t.cursorCol])
	after := make([]rune, len(line)-t.cursorCol)
	copy(after, line[t.cursorCol:])

	// Grow lines slice and shift everything after cursorLine.
	t.lines = append(t.lines, nil)
	copy(t.lines[t.cursorLine+2:], t.lines[t.cursorLine+1:])
	t.lines[t.cursorLine] = before
	t.lines[t.cursorLine+1] = after

	t.cursorLine++
	t.cursorCol = 0
	t.byteLen++ // newline byte
	t.wrapDirty = true
}

func (t *TextArea) backspace() {
	if t.cursorCol > 0 {
		line := t.lines[t.cursorLine]
		t.byteLen -= utf8.RuneLen(line[t.cursorCol-1])
		t.lines[t.cursorLine] = append(line[:t.cursorCol-1], line[t.cursorCol:]...)
		t.cursorCol--
		t.wrapDirty = true
	} else if t.cursorLine > 0 {
		// Merge with previous line — remove the newline byte.
		prev := t.lines[t.cursorLine-1]
		newCol := len(prev)
		t.lines[t.cursorLine-1] = append(prev, t.lines[t.cursorLine]...)
		t.lines = append(t.lines[:t.cursorLine], t.lines[t.cursorLine+1:]...)
		t.cursorLine--
		t.cursorCol = newCol
		t.byteLen-- // newline byte
		t.wrapDirty = true
	}
}

func (t *TextArea) deleteForward() {
	line := t.lines[t.cursorLine]
	if t.cursorCol < len(line) {
		t.byteLen -= utf8.RuneLen(line[t.cursorCol])
		t.lines[t.cursorLine] = append(line[:t.cursorCol], line[t.cursorCol+1:]...)
		t.wrapDirty = true
	} else if t.cursorLine < len(t.lines)-1 {
		// Merge with next line — remove the newline byte.
		t.lines[t.cursorLine] = append(line, t.lines[t.cursorLine+1]...)
		t.lines = append(t.lines[:t.cursorLine+1], t.lines[t.cursorLine+2:]...)
		t.byteLen-- // newline byte
		t.wrapDirty = true
	}
}

// --- Selection ---

func (t *TextArea) startSelection() {
	if !t.selActive {
		t.selActive = true
		t.selAnchorLine = t.cursorLine
		t.selAnchorCol = t.cursorCol
	}
}

func (t *TextArea) clearSelection() {
	t.selActive = false
}

// collapseEmptySelection deactivates the selection if anchor equals cursor.
// This prevents zero-width selections from intercepting edits.
func (t *TextArea) collapseEmptySelection() {
	if t.selActive && t.selAnchorLine == t.cursorLine && t.selAnchorCol == t.cursorCol {
		t.selActive = false
	}
}

func (t *TextArea) selectAll() {
	t.selActive = true
	t.selAnchorLine = 0
	t.selAnchorCol = 0
	t.cursorLine = len(t.lines) - 1
	t.cursorCol = len(t.lines[t.cursorLine])
}

// SelectedRange returns the normalized selection range.
// Returns (-1,-1,-1,-1) if no selection is active.
func (t *TextArea) SelectedRange() (startLine, startCol, endLine, endCol int) {
	if !t.selActive {
		return -1, -1, -1, -1
	}
	sl, sc := t.selAnchorLine, t.selAnchorCol
	el, ec := t.cursorLine, t.cursorCol
	if sl > el || (sl == el && sc > ec) {
		sl, sc, el, ec = el, ec, sl, sc
	}
	return sl, sc, el, ec
}

// SelectedText returns the text within the current selection.
func (t *TextArea) SelectedText() string {
	sl, sc, el, ec := t.SelectedRange()
	if sl < 0 {
		return ""
	}

	if sl == el {
		line := t.lines[sl]
		if sc > len(line) {
			sc = len(line)
		}
		if ec > len(line) {
			ec = len(line)
		}
		return string(line[sc:ec])
	}

	var sb strings.Builder
	// First line.
	first := t.lines[sl]
	if sc > len(first) {
		sc = len(first)
	}
	sb.WriteString(string(first[sc:]))

	// Middle lines.
	for i := sl + 1; i < el; i++ {
		sb.WriteByte('\n')
		sb.WriteString(string(t.lines[i]))
	}

	// Last line.
	sb.WriteByte('\n')
	last := t.lines[el]
	if ec > len(last) {
		ec = len(last)
	}
	sb.WriteString(string(last[:ec]))

	return sb.String()
}

// HasSelection returns true if a selection is active.
func (t *TextArea) HasSelection() bool {
	return t.selActive
}

// IsSelected returns true if the given visual (row, col) is within the selection.
func (t *TextArea) IsSelected(visRow, visCol int) bool {
	if !t.selActive {
		return false
	}
	logLine, logCol := t.visualToLogical(visRow, visCol)
	sl, sc, el, ec := t.SelectedRange()
	if logLine < sl || logLine > el {
		return false
	}
	if logLine == sl && logCol < sc {
		return false
	}
	if logLine == el && logCol >= ec {
		return false
	}
	return true
}

func (t *TextArea) deleteSelection() {
	sl, sc, el, ec := t.SelectedRange()
	if sl < 0 {
		return
	}

	if sl == el {
		line := t.lines[sl]
		t.lines[sl] = append(line[:sc], line[ec:]...)
	} else {
		// Keep start of first line + end of last line.
		first := t.lines[sl][:sc]
		last := t.lines[el]
		if ec > len(last) {
			ec = len(last)
		}
		remaining := last[ec:]
		merged := make([]rune, len(first)+len(remaining))
		copy(merged, first)
		copy(merged[len(first):], remaining)

		t.lines[sl] = merged
		t.lines = append(t.lines[:sl+1], t.lines[el+1:]...)
	}

	t.cursorLine = sl
	t.cursorCol = sc
	t.selActive = false
	t.wrapDirty = true
	t.recomputeByteLen()
}

func (t *TextArea) deleteSelectionIfActive() {
	t.collapseEmptySelection()
	if t.selActive {
		t.deleteSelection()
	}
}

// --- Clipboard ---

func (t *TextArea) copySelection() {
	if t.clipboard == nil || !t.selActive {
		return
	}
	text := t.SelectedText()
	if text != "" {
		if err := t.clipboard.Write(text); err != nil {
			slog.Warn("clipboard write failed", "err", err)
		}
	}
}

func (t *TextArea) cutSelection() {
	if t.clipboard == nil || !t.selActive {
		return
	}
	text := t.SelectedText()
	if text == "" {
		t.deleteSelection()
		return
	}
	if err := t.clipboard.Write(text); err != nil {
		slog.Warn("clipboard write failed, selection not deleted", "err", err)
		return
	}
	t.deleteSelection()
}

func (t *TextArea) paste() {
	if t.clipboard == nil {
		return
	}
	text := t.clipboard.Read()
	if text == "" {
		return
	}
	// Delete selection first so remaining capacity accounts for freed bytes.
	t.deleteSelectionIfActive()

	// Cap pasted text to remaining capacity before processing to avoid
	// allocating unbounded memory from a large clipboard payload.
	remaining := t.maxBytes - t.byteLen
	if remaining <= 0 {
		return
	}
	if len(text) > remaining {
		// Truncate on a valid UTF-8 boundary.
		text = text[:remaining]
		for len(text) > 0 && !utf8.Valid([]byte(text)) {
			text = text[:len(text)-1]
		}
		slog.Warn("paste truncated to capacity", "cap", t.maxBytes)
	}
	t.insertText(text)
}

func (t *TextArea) insertText(text string) {
	for _, r := range text {
		if t.byteLen >= t.maxBytes {
			return
		}
		if r == '\r' {
			continue
		}
		if r == '\n' {
			t.insertNewline()
			continue
		}
		if r == '\t' {
			t.insertRune(' ')
			continue
		}
		if unicode.IsControl(r) {
			continue
		}
		t.insertRune(r)
	}
}

// --- Mouse ---

// HandleClick positions the cursor at the given visual row and cell column,
// clearing any active selection. Call this on mouse-down in the input area.
func (t *TextArea) HandleClick(visRow, cellCol int) {
	t.clearSelection()
	visRow = t.clampVisualRow(visRow)
	runeCol := t.cellToRuneCol(visRow, cellCol)
	t.cursorLine, t.cursorCol = t.visualToLogical(visRow, runeCol)
}

// HandleDrag extends the selection to the given visual row and cell column.
// If no selection is active, one is started from the current cursor position.
func (t *TextArea) HandleDrag(visRow, cellCol int) {
	t.startSelection()
	visRow = t.clampVisualRow(visRow)
	runeCol := t.cellToRuneCol(visRow, cellCol)
	t.cursorLine, t.cursorCol = t.visualToLogical(visRow, runeCol)
}

// clampVisualRow constrains visRow to the valid wrap cache range so that
// clicks on blank padding rows below content map to the last wrapped line.
func (t *TextArea) clampVisualRow(visRow int) int {
	t.buildWrapCache()
	if len(t.wrapCache) == 0 || visRow < 0 {
		return 0
	}
	if visRow >= len(t.wrapCache) {
		return len(t.wrapCache) - 1
	}
	return visRow
}

// cellToRuneCol converts a cell (display) column to a rune index within
// the given visual row, accounting for wide characters.
func (t *TextArea) cellToRuneCol(visRow, cellCol int) int {
	t.buildWrapCache()
	if visRow < 0 || visRow >= len(t.wrapCache) {
		return 0
	}
	vl := t.wrapCache[visRow]
	cellsSeen := 0
	for i, r := range vl.runes {
		w := runewidth.RuneWidth(r)
		if cellsSeen+w > cellCol {
			return i
		}
		cellsSeen += w
	}
	return len(vl.runes)
}

// --- Helpers ---

// recomputeByteLen recalculates byteLen from scratch. Called after bulk
// mutations (SetContent, deleteSelection) where incremental tracking
// would be more complex than a single pass.
func (t *TextArea) recomputeByteLen() {
	n := 0
	for i, line := range t.lines {
		for _, r := range line {
			n += utf8.RuneLen(r)
		}
		if i > 0 {
			n++ // newline
		}
	}
	t.byteLen = n
}

// splitLines splits text into logical lines as rune slices.
func splitLines(text string) [][]rune {
	raw := strings.Split(text, "\n")
	result := make([][]rune, len(raw))
	for i, s := range raw {
		s = strings.ReplaceAll(s, "\r", "")
		result[i] = []rune(s)
	}
	return result
}
