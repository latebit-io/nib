// Package ui provides the Bubble Tea TUI components for junto.
package ui

import (
	"fmt"
	"log/slog"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/latebit-io/junto/engine/editor"
	"github.com/mattn/go-runewidth"
)

// lineKind classifies a viewport row for mouse click routing.
type lineKind int

const (
	lineNormal  lineKind = iota
	lineRemoved          // buffer line within the diff's removed range
	lineAdded            // virtual overlay line (editable replacement)
	lineEmpty            // tilde row past EOF
)

// viewportEntry maps a visual row to its source.
type viewportEntry struct {
	kind        lineKind
	bufLine     int // meaningful for lineNormal and lineRemoved
	overlayLine int // meaningful for lineAdded
}

// EditorModel is the Bubble Tea view for the code editor pane.
// It wraps the engine's Editor (which owns all domain logic)
// and adds only rendering + input mapping.
type EditorModel struct {
	*editor.Editor

	// Transient status message (shown in status bar, cleared on next key)
	StatusMsg string

	// Keymap for action matching (shared with AppModel)
	Keymap *Keymap

	// Shared services (clipboard, etc.)
	Services *Services

	// Internal clipboard buffer
	Clipboard string

	// Inline diff preview and editable replacement (nil when no edit is pending)
	Overlay *DiffOverlay

	// Viewport mapping rebuilt each Render() for mouse click resolution.
	viewportMap []viewportEntry
}

// NewEditorModel creates an editor model from an engine Editor.
func NewEditorModel(e *editor.Editor, km *Keymap, svc *Services) *EditorModel {
	return &EditorModel{
		Editor:   e,
		Keymap:   km,
		Services: svc,
	}
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

// --- Rendering ---

// expandTabs converts runes to display runes and builds buffer→display column mapping.
func expandTabs(runes []rune) (expanded []rune, bufToDisp []int) {
	bufToDisp = make([]int, len(runes)+1)
	dispCol := 0
	for bi, r := range runes {
		bufToDisp[bi] = dispCol
		if r == '\t' {
			expanded = append(expanded, ' ', ' ', ' ', ' ')
			dispCol += 4
		} else {
			expanded = append(expanded, r)
			dispCol++
		}
	}
	bufToDisp[len(runes)] = dispCol
	return
}

// fillDisplay creates a rune buffer of the given width (space-filled) and
// copies expanded runes into it.
func fillDisplay(expanded []rune, w int) []rune {
	displayed := make([]rune, w)
	for j := range displayed {
		displayed[j] = ' '
	}
	for j := 0; j < len(expanded) && j < w; j++ {
		displayed[j] = expanded[j]
	}
	return displayed
}

// displayColToBufCol converts a display column to a buffer column using the
// bufToDisp mapping produced by expandTabs.
func displayColToBufCol(bufToDisp []int, displayCol int) int {
	for i := len(bufToDisp) - 1; i >= 0; i-- {
		if bufToDisp[i] <= displayCol {
			return i
		}
	}
	return 0
}

// syncExtraVisualLines updates the engine's ExtraVisualLines field
// to match the overlay state so scroll methods work correctly.
func (m *EditorModel) syncExtraVisualLines() {
	if m.Overlay != nil {
		m.ExtraVisualLines = m.Overlay.LineCount()
	} else {
		m.ExtraVisualLines = 0
	}
}

// Render renders the editor view. Implements Pane.
//
// When an overlay exists, ScrollOffset is in visual-line space. The visual
// layout inserts the overlay's added lines between EndLine and EndLine+1:
//
//	visual 0..StartLine-1            → normal buffer lines
//	visual StartLine..EndLine        → removed buffer lines (red)
//	visual EndLine+1..EndLine+N      → added overlay lines (green, editable)
//	visual EndLine+N+1..             → normal buffer lines (starting from buffer EndLine+1)
//
// When no overlay exists, visual space == buffer space.
func (m *EditorModel) Render() string {
	m.ensureCursorVisibleVisual()
	m.syncExtraVisualLines()
	m.ClampScroll()

	gutterW := m.GutterWidth()
	contentW := m.ContentWidth()
	vis := m.VisibleLines()

	gutterStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	cursorStyle := lipgloss.NewStyle().Reverse(true)
	selectionStyle := lipgloss.NewStyle().Background(lipgloss.Color("24"))

	removedBgColor := lipgloss.Color("52") // dark red
	removedBg := lipgloss.NewStyle().Background(removedBgColor)
	removedGutterSt := lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Background(removedBgColor)

	addedBgColor := lipgloss.Color("22") // dark green
	addedBg := lipgloss.NewStyle().Background(addedBgColor)
	addedGutterSt := lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Background(addedBgColor)

	output := make([]string, m.Height)
	m.viewportMap = m.viewportMap[:0]

	overlay := m.Overlay
	addedCount := 0
	if overlay != nil {
		addedCount = overlay.LineCount()
	}
	showBufferCursor := overlay == nil || !overlay.Active

	for visualRow := range vis {
		if visualRow >= m.Height-1 {
			break
		}
		vLine := m.ScrollOffset + visualRow

		if overlay == nil {
			// No overlay — visual line == buffer line.
			if vLine >= m.Buf.LineCount() {
				output[visualRow] = gutterStyle.Render(fmt.Sprintf("%*s ", gutterW-1, "~")) + strings.Repeat(" ", contentW)
				m.viewportMap = append(m.viewportMap, viewportEntry{kind: lineEmpty})
			} else {
				output[visualRow] = m.renderNormalLine(vLine, gutterW, contentW, gutterStyle, cursorStyle, selectionStyle, showBufferCursor)
				m.viewportMap = append(m.viewportMap, viewportEntry{kind: lineNormal, bufLine: vLine})
			}
			continue
		}

		// With overlay — map visual line to content type.
		addedStart := overlay.EndLine + 1
		addedEnd := overlay.EndLine + addedCount // inclusive

		switch {
		case vLine < overlay.StartLine:
			// Normal line before diff.
			output[visualRow] = m.renderNormalLine(vLine, gutterW, contentW, gutterStyle, cursorStyle, selectionStyle, showBufferCursor)
			m.viewportMap = append(m.viewportMap, viewportEntry{kind: lineNormal, bufLine: vLine})

		case vLine <= overlay.EndLine:
			// Removed (red) line.
			output[visualRow] = m.renderRemovedLine(vLine, gutterW, contentW, removedBg, removedBgColor, removedGutterSt)
			m.viewportMap = append(m.viewportMap, viewportEntry{kind: lineRemoved, bufLine: vLine})

		case vLine >= addedStart && vLine <= addedEnd:
			// Added (green) overlay line.
			addedIdx := vLine - addedStart
			output[visualRow] = m.renderAddedLine(addedIdx, gutterW, contentW, cursorStyle, selectionStyle, addedBg, addedGutterSt)
			m.viewportMap = append(m.viewportMap, viewportEntry{kind: lineAdded, overlayLine: addedIdx})

		default:
			// Normal line after diff — subtract added lines to get buffer line.
			bufLine := vLine - addedCount
			if bufLine >= m.Buf.LineCount() {
				output[visualRow] = gutterStyle.Render(fmt.Sprintf("%*s ", gutterW-1, "~")) + strings.Repeat(" ", contentW)
				m.viewportMap = append(m.viewportMap, viewportEntry{kind: lineEmpty})
			} else {
				output[visualRow] = m.renderNormalLine(bufLine, gutterW, contentW, gutterStyle, cursorStyle, selectionStyle, showBufferCursor)
				m.viewportMap = append(m.viewportMap, viewportEntry{kind: lineNormal, bufLine: bufLine})
			}
		}
	}

	// Fill remaining rows with tildes.
	for visualRow := vis; visualRow < m.Height-1; visualRow++ {
		output[visualRow] = gutterStyle.Render(fmt.Sprintf("%*s ", gutterW-1, "~")) + strings.Repeat(" ", contentW)
		m.viewportMap = append(m.viewportMap, viewportEntry{kind: lineEmpty})
	}

	// Status bar (last row).
	if m.Height > 0 {
		output[m.Height-1] = m.renderStatusBar()
	}

	return strings.Join(output, "\n")
}

// ensureCursorVisibleVisual corrects ScrollOffset in visual-line space.
// When an overlay exists, the active cursor (buffer or overlay) must be
// mapped to its visual line before adjusting scroll.
func (m *EditorModel) ensureCursorVisibleVisual() {
	overlay := m.Overlay
	if overlay == nil {
		return
	}
	addedCount := overlay.LineCount()
	vis := m.VisibleLines()
	if vis <= 0 {
		return
	}

	var visualCursor int
	if overlay.Active {
		// Overlay cursor: added lines start at visual line EndLine+1.
		visualCursor = overlay.EndLine + 1 + overlay.Editor.CursorLine
	} else {
		// Buffer cursor: shift by addedCount if past the diff.
		visualCursor = m.CursorLine
		if m.CursorLine > overlay.EndLine {
			visualCursor = m.CursorLine + addedCount
		}
	}

	if visualCursor < m.ScrollOffset {
		m.ScrollOffset = visualCursor
	}
	if visualCursor >= m.ScrollOffset+vis {
		m.ScrollOffset = visualCursor - vis + 1
	}
}

func (m *EditorModel) renderNormalLine(
	lineIdx, gutterW, contentW int,
	gutterStyle, cursorStyle, selectionStyle lipgloss.Style,
	showCursor bool,
) string {
	var line strings.Builder

	gutterText := fmt.Sprintf("%*d ", gutterW-1, lineIdx+1)
	line.WriteString(gutterStyle.Render(gutterText))

	rawRunes := []rune(m.Buf.LineText(lineIdx))
	expanded, bufToDisp := expandTabs(rawRunes)
	displayed := fillDisplay(expanded, contentW)

	// Cursor position in display coords.
	displayCursorCol := -1
	if showCursor && lineIdx == m.CursorLine && m.CursorCol >= 0 && m.CursorCol <= len(rawRunes) {
		displayCursorCol = bufToDisp[m.CursorCol]
		if displayCursorCol >= contentW && contentW > 0 {
			displayCursorCol = contentW - 1
		}
	}

	// Syntax highlighting.
	charStyles := make([]lipgloss.Style, contentW)
	if tokens := m.HighlightLine(lineIdx); len(tokens) > 0 {
		for _, tok := range tokens {
			dStart := 0
			if tok.Col < len(bufToDisp) {
				dStart = bufToDisp[tok.Col]
			}
			dEnd := dStart + tok.Len
			tokEnd := tok.Col + tok.Len
			if tokEnd < len(bufToDisp) {
				dEnd = bufToDisp[tokEnd]
			}
			style := styleForTokenKind(tok.Kind)
			for j := dStart; j < dEnd && j < contentW; j++ {
				charStyles[j] = style
			}
		}
	}

	// Selection: precompute inverse mapping display col → buffer col.
	dispToBuf := make([]int, contentW)
	if m.SelectionActive {
		bufCol := 0
		for j := range contentW {
			for bufCol+1 <= len(rawRunes) && bufToDisp[bufCol+1] <= j {
				bufCol++
			}
			dispToBuf[j] = bufCol
		}
	}

	for j := range contentW {
		ch := string(displayed[j])
		isCursor := j == displayCursorCol
		isSel := m.SelectionActive && m.IsSelected(lineIdx, dispToBuf[j])

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

	return line.String()
}

func (m *EditorModel) renderRemovedLine(
	lineIdx, gutterW, contentW int,
	bgStyle lipgloss.Style,
	bgColor lipgloss.Color,
	gutterSt lipgloss.Style,
) string {
	var line strings.Builder

	gutterText := fmt.Sprintf("%*d-", gutterW-1, lineIdx+1)
	line.WriteString(gutterSt.Render(gutterText))

	rawRunes := []rune(m.Buf.LineText(lineIdx))
	expanded, bufToDisp := expandTabs(rawRunes)
	displayed := fillDisplay(expanded, contentW)

	// Syntax highlighting on removed lines (they are real buffer lines).
	charStyles := make([]lipgloss.Style, contentW)
	if tokens := m.HighlightLine(lineIdx); len(tokens) > 0 {
		for _, tok := range tokens {
			dStart := 0
			if tok.Col < len(bufToDisp) {
				dStart = bufToDisp[tok.Col]
			}
			dEnd := dStart + tok.Len
			tokEnd := tok.Col + tok.Len
			if tokEnd < len(bufToDisp) {
				dEnd = bufToDisp[tokEnd]
			}
			style := styleForTokenKind(tok.Kind)
			for j := dStart; j < dEnd && j < contentW; j++ {
				charStyles[j] = style
			}
		}
	}

	for j := range contentW {
		ch := string(displayed[j])
		if charStyles[j].GetForeground() != nil {
			line.WriteString(charStyles[j].Background(bgColor).Render(ch))
		} else {
			line.WriteString(bgStyle.Render(ch))
		}
	}

	return line.String()
}

func (m *EditorModel) renderAddedLine(
	overlayIdx, gutterW, contentW int,
	cursorStyle, selectionStyle, bgStyle, gutterSt lipgloss.Style,
) string {
	var line strings.Builder

	gutterText := fmt.Sprintf("%*s+", gutterW-1, "")
	line.WriteString(gutterSt.Render(gutterText))

	oe := m.Overlay.Editor
	rawRunes := []rune(oe.Buf.LineText(overlayIdx))
	expanded, bufToDisp := expandTabs(rawRunes)
	displayed := fillDisplay(expanded, contentW)

	displayCursorCol := -1
	if m.Overlay.Active && overlayIdx == oe.CursorLine && oe.CursorCol >= 0 && oe.CursorCol <= len(rawRunes) {
		displayCursorCol = bufToDisp[oe.CursorCol]
		if displayCursorCol >= contentW && contentW > 0 {
			displayCursorCol = contentW - 1
		}
	}

	// Precompute inverse mapping for selection: display col → rune col
	dispToBuf := make([]int, contentW)
	if oe.SelectionActive {
		bufCol := 0
		for j := range contentW {
			for bufCol+1 <= len(rawRunes) && bufToDisp[bufCol+1] <= j {
				bufCol++
			}
			dispToBuf[j] = bufCol
		}
	}

	for j := range contentW {
		ch := string(displayed[j])
		isCursor := j == displayCursorCol
		isSel := oe.SelectionActive && oe.IsSelected(overlayIdx, dispToBuf[j])

		if isCursor {
			line.WriteString(cursorStyle.Render(ch))
		} else if isSel {
			line.WriteString(selectionStyle.Render(ch))
		} else {
			line.WriteString(bgStyle.Render(ch))
		}
	}

	return line.String()
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

	// Show overlay cursor position when overlay is active.
	var right string
	if m.Overlay != nil && m.Overlay.Active {
		right = fmt.Sprintf(" +%d:%d ", m.Overlay.Editor.CursorLine+1, m.Overlay.Editor.CursorCol+1)
	} else {
		right = fmt.Sprintf(" %d:%d ", m.CursorLine+1, m.CursorCol+1)
	}

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

// --- Mouse Handling ---

func (m *EditorModel) handleMouse(msg tea.MouseMsg) tea.Cmd {
	scrollLines := 3

	switch {
	case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonWheelUp:
		m.syncExtraVisualLines()
		m.ScrollUp(scrollLines)
		return nil

	case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonWheelDown:
		m.syncExtraVisualLines()
		m.ScrollDown(scrollLines)
		return nil
	}

	if msg.Button != tea.MouseButtonLeft || msg.Y >= m.VisibleLines() {
		return nil
	}

	gutterW := m.GutterWidth()
	displayCol := msg.X - gutterW
	if displayCol < 0 {
		displayCol = 0
	}

	if msg.Y >= len(m.viewportMap) {
		return nil
	}
	entry := m.viewportMap[msg.Y]

	// When the overlay is active and the user is dragging (motion/release),
	// keep the selection within the overlay — don't deactivate or scroll away.
	if m.Overlay != nil && m.Overlay.Active && msg.Action != tea.MouseActionPress {
		oe := m.Overlay.Editor
		switch entry.kind {
		case lineAdded:
			_, bufToDisp := expandTabs([]rune(oe.Buf.LineText(entry.overlayLine)))
			col := displayColToBufCol(bufToDisp, displayCol)
			switch msg.Action {
			case tea.MouseActionMotion:
				oe.MoveCursorTo(entry.overlayLine, col)
			case tea.MouseActionRelease:
				if oe.SelectionActive &&
					oe.CursorLine == oe.SelectStartLine &&
					oe.CursorCol == oe.SelectStartCol {
					oe.ClearSelection()
				}
			}
		default:
			// Drag went outside the overlay — clamp to nearest boundary.
			if msg.Action == tea.MouseActionMotion {
				if entry.kind == lineNormal && entry.bufLine < m.Overlay.StartLine {
					// Above the diff — clamp to first overlay line, col 0
					oe.MoveCursorTo(0, 0)
				} else {
					// Below or on removed lines — clamp to last overlay line, end of line
					lastLine := oe.Buf.LineCount() - 1
					oe.MoveCursorTo(lastLine, oe.Buf.LineLen(lastLine))
				}
			}
			// Release outside overlay — just finalize the selection, don't deactivate
		}
		return nil
	}

	switch entry.kind {
	case lineNormal:
		if m.Overlay != nil && m.Overlay.Active {
			slog.Debug("overlay deactivated", "reason", "click on normal line", "bufLine", entry.bufLine)
			m.Overlay.Active = false
		}
		m.handleNormalLineClick(entry.bufLine, displayCol, msg.Action)

	case lineAdded:
		if m.Overlay != nil {
			oe := m.Overlay.Editor
			_, bufToDisp := expandTabs([]rune(oe.Buf.LineText(entry.overlayLine)))
			col := displayColToBufCol(bufToDisp, displayCol)
			switch msg.Action {
			case tea.MouseActionPress:
				slog.Debug("overlay click", "overlayLine", entry.overlayLine, "col", col)
				m.Overlay.Active = true
				oe.ClearSelection()
				m.ClearSelection()
				oe.MoveCursorTo(entry.overlayLine, col)
				oe.SelectionActive = true
				oe.SelectStartLine = oe.CursorLine
				oe.SelectStartCol = oe.CursorCol
			}
		}

	case lineRemoved:
		if m.Overlay != nil && m.Overlay.Active {
			slog.Debug("overlay deactivated", "reason", "click on removed line", "bufLine", entry.bufLine)
			m.Overlay.Active = false
		}
	}

	return nil
}

func (m *EditorModel) handleNormalLineClick(bufLine, displayCol int, action tea.MouseAction) {
	var bufCol int
	if bufLine >= m.Buf.LineCount() {
		bufLine = m.Buf.LineCount() - 1
		if bufLine < 0 {
			bufLine = 0
		}
		bufCol = m.Buf.LineLen(bufLine)
	} else {
		bufCol = m.DisplayColToBufferCol(bufLine, displayCol)
	}

	switch action {
	case tea.MouseActionPress:
		m.ClearSelection()
		m.MoveCursorTo(bufLine, bufCol)
		m.SelectionActive = true
		m.SelectStartLine = m.CursorLine
		m.SelectStartCol = m.CursorCol
	case tea.MouseActionMotion:
		if m.SelectionActive {
			m.MoveCursorTo(bufLine, bufCol)
		}
	case tea.MouseActionRelease:
		if m.SelectionActive &&
			m.CursorLine == m.SelectStartLine &&
			m.CursorCol == m.SelectStartCol {
			m.ClearSelection()
		}
	}
}

// --- Key Handling ---

// overlapsRemovedRange returns true when the cursor or selection touches the
// overlay's removed line range. Used to block edits that would desync the diff.
func (m *EditorModel) overlapsRemovedRange() bool {
	if m.Overlay == nil || m.Overlay.Active {
		return false
	}
	start, end := m.Overlay.StartLine, m.Overlay.EndLine
	if m.CursorLine >= start && m.CursorLine <= end {
		return true
	}
	if m.SelectionActive {
		selStart, selEnd := m.SelectStartLine, m.CursorLine
		if selStart > selEnd {
			selStart, selEnd = selEnd, selStart
		}
		// Selection overlaps if it isn't entirely before or after the range.
		if selStart <= end && selEnd >= start {
			return true
		}
	}
	return false
}

func (m *EditorModel) handleKey(keyMsg tea.KeyMsg) tea.Cmd {
	m.StatusMsg = "" // clear transient status on any key

	// Route to overlay when it owns the cursor.
	if m.Overlay != nil && m.Overlay.Active {
		return m.handleOverlayKey(keyMsg)
	}

	// Track line count so we can adjust overlay position if the user
	// inserts/deletes lines above the diff.
	linesBefore := m.Buf.LineCount()

	readOnly := m.overlapsRemovedRange()
	cmd := m.handleEditorKeyFor(keyMsg, m.Editor, readOnly)

	m.adjustOverlayPosition(linesBefore)
	return cmd
}

// adjustOverlayPosition shifts the overlay's line range when lines are
// inserted or deleted above it in the main buffer.
func (m *EditorModel) adjustOverlayPosition(linesBefore int) {
	if m.Overlay == nil {
		return
	}
	delta := m.Buf.LineCount() - linesBefore
	if delta == 0 {
		return
	}
	// Edits happen at the cursor. Only adjust if the cursor is above the overlay.
	if m.CursorLine <= m.Overlay.StartLine {
		m.Overlay.StartLine += delta
		m.Overlay.EndLine += delta
		slog.Debug("overlay position adjusted", "delta", delta, "newStart", m.Overlay.StartLine, "newEnd", m.Overlay.EndLine)
		// If the overlay shifted to an invalid position, remove it.
		if m.Overlay.StartLine < 0 || m.Overlay.EndLine < 0 {
			slog.Debug("overlay removed", "reason", "shifted to invalid position")
			m.Overlay = nil
			m.ExtraVisualLines = 0
		}
	}
}

// handleOverlayKey handles keys when the overlay editor is active.
// Overlay-specific: boundary exit (up/down past edges), escape to deactivate,
// save always goes to main buffer. Everything else delegates to the shared handler.
func (m *EditorModel) handleOverlayKey(keyMsg tea.KeyMsg) tea.Cmd {
	o := m.Overlay
	oe := o.Editor

	// Escape — leave overlay, return cursor to buffer.
	if keyMsg.Type == tea.KeyEscape {
		slog.Debug("overlay deactivated", "reason", "escape")
		oe.ClearSelection()
		o.Active = false
		return nil
	}

	// Up at top of overlay — exit upward
	if keyMsg.Type == tea.KeyUp && oe.CursorLine == 0 {
		slog.Debug("overlay deactivated", "reason", "arrow up past top", "target", o.StartLine-1)
		oe.ClearSelection()
		o.Active = false
		target := o.StartLine - 1
		if target < 0 {
			target = 0
		}
		m.MoveCursorTo(target, oe.CursorCol)
		return nil
	}

	// Down at bottom of overlay — exit downward
	if keyMsg.Type == tea.KeyDown && oe.CursorLine >= oe.Buf.LineCount()-1 {
		slog.Debug("overlay deactivated", "reason", "arrow down past bottom", "target", o.EndLine+1)
		oe.ClearSelection()
		o.Active = false
		target := o.EndLine + 1
		if target >= m.Buf.LineCount() {
			target = m.Buf.LineCount() - 1
		}
		m.MoveCursorTo(target, oe.CursorCol)
		return nil
	}

	// Everything else — same as normal editor
	return m.handleEditorKeyFor(keyMsg, oe, false)
}

// handleEditorKeyFor is the shared key handler that operates on any *editor.Editor.
// Both the main editor and the overlay editor use this — no duplication.
func (m *EditorModel) handleEditorKeyFor(keyMsg tea.KeyMsg, e *editor.Editor, readOnly bool) tea.Cmd {
	isShift := keyMsg.Type == tea.KeyShiftUp || keyMsg.Type == tea.KeyShiftDown ||
		keyMsg.Type == tea.KeyShiftLeft || keyMsg.Type == tea.KeyShiftRight ||
		keyMsg.Type == tea.KeyShiftHome || keyMsg.Type == tea.KeyShiftEnd

	action := m.Keymap.Match(keyMsg)

	switch action {
	case ActionSave:
		// Save always operates on the main buffer.
		if err := m.Save(); err != nil {
			m.StatusMsg = "Save failed: " + err.Error()
		} else {
			m.StatusMsg = "Saved"
		}
		return nil

	case ActionUndo:
		if !readOnly {
			e.Undo()
		}
		return nil

	case ActionRedo:
		if !readOnly {
			e.Redo()
		}
		return nil

	case ActionCopy:
		if e.SelectionActive {
			m.Clipboard = e.SelectedText()
			if err := m.Services.Clipboard.Write(m.Clipboard); err != nil {
				slog.Warn("system clipboard write failed", "err", err)
			}
		}
		return nil

	case ActionCut:
		if !readOnly && e.SelectionActive {
			m.Clipboard = e.SelectedText()
			if err := m.Services.Clipboard.Write(m.Clipboard); err != nil {
				slog.Warn("system clipboard write failed", "err", err)
			}
			e.DeleteSelection()
			e.MarkDirty()
		}
		return nil

	case ActionPaste:
		if !readOnly {
			if sys := m.Services.Clipboard.Read(); sys != "" {
				m.Clipboard = sys
			}
			if m.Clipboard != "" {
				if e.SelectionActive {
					e.DeleteSelection()
				}
				e.PasteText(m.Clipboard)
			}
		}
		return nil

	case ActionSelectAll:
		e.SelectAll()
		return nil
	}

	if keyMsg.Type == tea.KeyEscape {
		e.ClearSelection()
		return nil
	}

	switch keyMsg.Type {

	// Selection navigation
	case tea.KeyShiftUp:
		e.StartSelection()
		e.MoveCursor(-1, 0)
		return nil
	case tea.KeyShiftDown:
		e.StartSelection()
		e.MoveCursor(1, 0)
		return nil
	case tea.KeyShiftLeft:
		e.StartSelection()
		e.MoveCursor(0, -1)
		return nil
	case tea.KeyShiftRight:
		e.StartSelection()
		e.MoveCursor(0, 1)
		return nil
	case tea.KeyShiftHome:
		e.StartSelection()
		e.Home()
		return nil
	case tea.KeyShiftEnd:
		e.StartSelection()
		e.End()
		return nil

	// Navigation
	case tea.KeyUp:
		e.ClearSelection()
		e.MoveCursor(-1, 0)
		return nil
	case tea.KeyDown:
		e.ClearSelection()
		e.MoveCursor(1, 0)
		return nil
	case tea.KeyLeft:
		e.ClearSelection()
		e.MoveCursor(0, -1)
		return nil
	case tea.KeyRight:
		e.ClearSelection()
		e.MoveCursor(0, 1)
		return nil
	case tea.KeyHome:
		e.ClearSelection()
		e.Home()
		return nil
	case tea.KeyEnd:
		e.ClearSelection()
		e.End()
		return nil
	case tea.KeyPgUp:
		e.ClearSelection()
		m.syncExtraVisualLines()
		e.PageUp()
		return nil
	case tea.KeyPgDown:
		e.ClearSelection()
		m.syncExtraVisualLines()
		e.PageDown()
		return nil

	// Word navigation
	case tea.KeyCtrlRight:
		e.ClearSelection()
		e.WordRight()
		return nil
	case tea.KeyCtrlLeft:
		e.ClearSelection()
		e.WordLeft()
		return nil

	// Editing
	case tea.KeyEnter:
		if readOnly {
			return nil
		}
		if e.SelectionActive {
			e.DeleteSelection()
		}
		e.InsertNewline()
		return nil
	case tea.KeyTab:
		if readOnly {
			return nil
		}
		if e.SelectionActive {
			e.DeleteSelection()
		}
		e.InsertTab()
		return nil
	case tea.KeySpace:
		if readOnly {
			return nil
		}
		if e.SelectionActive {
			e.DeleteSelection()
		}
		e.InsertChar(' ')
		return nil
	case tea.KeyBackspace:
		if readOnly {
			return nil
		}
		if e.SelectionActive {
			e.DeleteSelection()
		} else {
			e.Backspace()
		}
		return nil
	case tea.KeyDelete:
		if readOnly {
			return nil
		}
		if e.SelectionActive {
			e.DeleteSelection()
		} else {
			e.DeleteChar()
		}
		return nil

	case tea.KeyRunes:
		if readOnly {
			return nil
		}
		if len(keyMsg.Runes) > 1 {
			if e.SelectionActive {
				e.DeleteSelection()
			}
			e.PasteText(string(keyMsg.Runes))
		} else {
			if e.SelectionActive {
				e.DeleteSelection()
			}
			for _, r := range keyMsg.Runes {
				e.InsertChar(r)
			}
		}
		return nil
	}

	if !isShift {
		e.ClearSelection()
	}

	return nil
}

// styleForTokenKind maps engine editor.TokenKind to lipgloss.Style for TUI rendering.
func styleForTokenKind(kind editor.TokenKind) lipgloss.Style {
	switch kind {
	case editor.KindKeyword:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("5")) // magenta
	case editor.KindString:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("2")) // green
	case editor.KindComment:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("8")) // gray
	case editor.KindNumber:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("3")) // yellow
	case editor.KindType:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("6")) // cyan
	case editor.KindOperator:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("9")) // bright red
	default:
		return lipgloss.NewStyle()
	}
}
