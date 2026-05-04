package ui

import (
	"image/color"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/latebit-io/nib/engine/editor"
	"github.com/latebit-io/nib/engine/lang"
	"github.com/latebit-io/nib/tui/sanitize"
)

// Per-line rendering for EditorModel.
//
// renderNormalLine, renderRemovedLine, and renderAddedLine produce one
// visual row each, in three flavours: plain buffer rows, the red
// "removed" rows shown above an overlay, and the green editable rows of
// the overlay itself. They share scratch buffers (charStyles, colTags,
// dispToBuf, syntaxTagMap, displayBuf) and three helpers (expandTabs,
// fillDisplay, displayColToBufCol) that handle tab expansion, fixed-
// width filling, and display↔buffer column conversion.
//
// All methods stay attached to EditorModel because they consume the
// model's pre-allocated scratch state; the file boundary makes the
// rendering subsystem visible without touching the public API.

// expandTabs converts runes to display runes and builds buffer→display column mapping.
func (m *EditorModel) expandTabs(runes []rune) (expanded []rune, bufToDisp []int) {
	if cap(m.bufToDispBuf) < len(runes)+1 {
		m.bufToDispBuf = make([]int, len(runes)+1)
	}
	bufToDisp = m.bufToDispBuf[:len(runes)+1]
	if maxDisp := len(runes) * editor.TabWidth; cap(m.expandedBuf) < maxDisp {
		m.expandedBuf = make([]rune, 0, maxDisp)
	}
	expanded = m.expandedBuf[:0]
	dispCol := 0
	for bi, r := range runes {
		bufToDisp[bi] = dispCol
		switch r {
		case '\t':
			for range editor.TabWidth {
				expanded = append(expanded, ' ')
			}
			dispCol += editor.TabWidth
		case '\uFE0F':
			// Variation Selector 16 would force emoji presentation (2 cells)
			// but terminal width measurement disagrees. Skip it in the
			// display buffer — the base character renders fine without it.
			// bufToDisp still maps this rune so cursor navigation works.
		default:
			expanded = append(expanded, r)
			dispCol++
		}
	}
	bufToDisp[len(runes)] = dispCol
	m.expandedBuf = expanded
	return
}

// formatGutterDigits returns a right-aligned line number padded with
// leading spaces to width. Replaces fmt.Sprintf("%*d", …) in the per-line
// gutter rendering paths so the conversion stays alloc-free except for
// the final string copy. Callers append the trailing suffix (' ', '-',
// or a diagnostic icon) themselves.
func (m *EditorModel) formatGutterDigits(n, width int) string {
	// Convert into a small fixed stack array so the digit conversion
	// itself stays alloc-free regardless of m.gutterBuf state. 20 covers
	// every int64.
	var tmp [20]byte
	digits := strconv.AppendInt(tmp[:0], int64(n), 10)
	pad := width - len(digits)
	if pad <= 0 {
		return string(digits)
	}
	if cap(m.gutterBuf) < width {
		m.gutterBuf = make([]byte, 0, width)
	}
	m.gutterBuf = m.gutterBuf[:0]
	for i := 0; i < pad; i++ {
		m.gutterBuf = append(m.gutterBuf, ' ')
	}
	m.gutterBuf = append(m.gutterBuf, digits...)
	return string(m.gutterBuf)
}

// formatGutterBlank returns width-1 spaces followed by suffix. Used for
// the added-line gutter where there is no source line number.
func (m *EditorModel) formatGutterBlank(width int, suffix byte) string {
	if cap(m.gutterBuf) < width {
		m.gutterBuf = make([]byte, 0, width)
	}
	m.gutterBuf = m.gutterBuf[:0]
	for i := 0; i < width-1; i++ {
		m.gutterBuf = append(m.gutterBuf, ' ')
	}
	m.gutterBuf = append(m.gutterBuf, suffix)
	return string(m.gutterBuf)
}

// fillDisplay returns a rune buffer of the given width (space-filled),
// starting from scrollCol in the expanded rune slice. Content before
// scrollCol is not included, enabling horizontal scrolling.
// The buffer is reused across calls to avoid per-line allocation.
func (m *EditorModel) fillDisplay(expanded []rune, w, scrollCol int) []rune {
	if cap(m.displayBuf) < w {
		m.displayBuf = make([]rune, w)
	}
	displayed := m.displayBuf[:w]
	for j := range displayed {
		displayed[j] = ' '
	}
	start := scrollCol
	if start > len(expanded) {
		start = len(expanded)
	}
	visible := expanded[start:]
	for j := 0; j < len(visible) && j < w; j++ {
		displayed[j] = visible[j]
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

func (m *EditorModel) renderNormalLine(
	lineIdx, gutterW, contentW int,
	gutterStyle, cursorStyle, selectionStyle lipgloss.Style,
	showCursor bool,
) string {
	var line strings.Builder

	isAgentLine := m.eng.LineOrigin(lineIdx) == editor.OriginAgent

	// Sanitize agent-origin lines to prevent ANSI injection from LLM output.
	lineText := m.eng.LineText(lineIdx)
	if isAgentLine {
		var san sanitize.Sanitizer
		lineText = san.Sanitize(lineText)
	}

	numText := m.formatGutterDigits(lineIdx+1, gutterW-1)
	if diag := m.diagnosticForLine(lineIdx); diag != nil {
		// Diagnostic icon takes priority in the gutter suffix.
		line.WriteString(gutterStyle.Render(numText))
		var icon string
		var iconStyle lipgloss.Style
		switch diag.Severity {
		case lang.SeverityError:
			icon, iconStyle = diagErrorIcon, diagErrorGutterStyle
		case lang.SeverityWarning:
			icon, iconStyle = diagWarningIcon, diagWarningGutterStyle
		default:
			icon, iconStyle = diagInfoIcon, diagInfoGutterStyle
		}
		line.WriteString(iconStyle.Render(icon))
	} else if isAgentLine {
		line.WriteString(agentLineGutterStyle.Render(numText + " "))
	} else {
		line.WriteString(gutterStyle.Render(numText + " "))
	}

	rawRunes := []rune(lineText)
	expanded, bufToDisp := m.expandTabs(rawRunes)
	scrollCol := m.eng.ScrollCol
	displayed := m.fillDisplay(expanded, contentW, scrollCol)

	// Cursor position in display coords, offset by horizontal scroll.
	displayCursorCol := -1
	if showCursor && lineIdx == m.eng.CursorLine && m.eng.CursorCol >= 0 && m.eng.CursorCol <= len(rawRunes) {
		displayCursorCol = bufToDisp[m.eng.CursorCol] - scrollCol
		if displayCursorCol < 0 || displayCursorCol >= contentW {
			displayCursorCol = -1 // off-screen
		}
	}

	// Syntax highlighting (reuse scratch buffer).
	if cap(m.charStyles) < contentW {
		m.charStyles = make([]lipgloss.Style, contentW)
	}
	charStyles := m.charStyles[:contentW]
	clear(charStyles)
	if tokens := m.eng.HighlightLine(lineIdx); len(tokens) > 0 {
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
			dStart -= scrollCol
			dEnd -= scrollCol
			if dEnd <= 0 || dStart >= contentW {
				continue
			}
			if dStart < 0 {
				dStart = 0
			}
			style := styleForTokenKind(tok.Kind)
			for j := dStart; j < dEnd && j < contentW; j++ {
				charStyles[j] = style
			}
		}
	}

	// Selection: precompute inverse mapping viewport col → buffer col.
	// Viewport col j corresponds to absolute display col j + scrollCol.
	if cap(m.dispToBuf) < contentW {
		m.dispToBuf = make([]int, contentW)
	}
	dispToBuf := m.dispToBuf[:contentW]
	clear(dispToBuf)
	if m.eng.SelectionActive {
		bufCol := 0
		for j := range contentW {
			absDispCol := j + scrollCol
			for bufCol+1 <= len(rawRunes) && bufToDisp[bufCol+1] <= absDispCol {
				bufCol++
			}
			dispToBuf[j] = bufCol
		}
	}

	// Diagnostic underlines: mark display columns within diagnostic ranges.
	// Reuse scratch buffer to avoid per-line allocation.
	if cap(m.diagUnderline) < contentW {
		m.diagUnderline = make([]bool, contentW)
	}
	diagUnderline := m.diagUnderline[:contentW]
	clear(diagUnderline)
	for i := range m.diagnostics {
		d := &m.diagnostics[i]
		if d.StartLine > lineIdx || d.EndLine < lineIdx {
			continue
		}
		startBufCol := 0
		if lineIdx == d.StartLine {
			startBufCol = d.StartCol
		}
		endBufCol := len(rawRunes)
		if lineIdx == d.EndLine {
			endBufCol = d.EndCol
		}
		startBufCol = min(max(startBufCol, 0), len(rawRunes))
		endBufCol = min(max(endBufCol, 0), len(rawRunes))
		startDisp := bufToDisp[startBufCol] - scrollCol
		endDisp := bufToDisp[endBufCol] - scrollCol
		if endDisp <= 0 || startDisp >= contentW {
			continue
		}
		// Zero-width diagnostics (e.g., missing token) get at least one cell.
		if endDisp <= startDisp && startDisp < contentW {
			endDisp = startDisp + 1
		}
		if startDisp < 0 {
			startDisp = 0
		}
		for j := startDisp; j < endDisp && j < contentW; j++ {
			diagUnderline[j] = true
		}
	}

	// Span-based rendering: batch consecutive characters that share the same
	// effective style into a single lipgloss.Render call. This reduces overhead
	// from O(columns) to O(style-transitions) — typically 5-20x fewer calls.
	//
	// styleTag classifies each column. Cursors and selection get unique tags;
	// syntax tokens share a tag when they have the same foreground color.
	// Plain text (no style) is tag 0.
	if cap(m.colTags) < contentW {
		m.colTags = make([]int, contentW)
	}
	colTags := m.colTags[:contentW]
	clear(colTags)
	const (
		tagPlain       = 0
		tagCursor      = -1
		tagSel         = -3
		tagFindMatch   = -4
		tagFindCurrent = -5
	)
	// Syntax tokens get positive tags starting at 1, grouped by foreground color.
	if m.syntaxTagMap == nil {
		m.syntaxTagMap = make(map[color.Color]int)
	}
	syntaxTagMap := m.syntaxTagMap
	clear(syntaxTagMap)
	nextTag := 1
	// Find match highlights: precompute per-display-column state.
	// 0 = no match, 1 = match, 2 = current match.
	if cap(m.findHighlight) < contentW {
		m.findHighlight = make([]byte, contentW)
	}
	findHL := m.findHighlight[:contentW]
	clear(findHL)
	if m.Find.Active && len(m.Find.Matches) > 0 {
		for i, match := range m.Find.Matches {
			if match.Line != lineIdx {
				if match.Line > lineIdx {
					break
				}
				continue
			}
			hlVal := byte(1)
			if i == m.Find.CurrentMatch {
				hlVal = 2
			}
			startBuf := match.Col
			endBuf := match.Col + match.Len
			if startBuf < len(bufToDisp) && endBuf < len(bufToDisp) {
				dStart := bufToDisp[startBuf] - scrollCol
				dEnd := bufToDisp[endBuf] - scrollCol
				if dStart < 0 {
					dStart = 0
				}
				for d := dStart; d < dEnd && d < contentW; d++ {
					findHL[d] = hlVal
				}
			}
		}
	}

	for j := range contentW {
		switch {
		case j == displayCursorCol:
			colTags[j] = tagCursor
		case m.eng.SelectionActive && m.eng.IsSelected(lineIdx, dispToBuf[j]):
			colTags[j] = tagSel
		case findHL[j] == 2:
			colTags[j] = tagFindCurrent
		case findHL[j] == 1:
			colTags[j] = tagFindMatch
		case charStyles[j].GetForeground() != nil:
			fg := charStyles[j].GetForeground()
			tag, ok := syntaxTagMap[fg]
			if !ok {
				tag = nextTag
				syntaxTagMap[fg] = tag
				nextTag++
			}
			colTags[j] = tag
		default:
			colTags[j] = tagPlain
		}
	}

	flushSpan := func(start, end int) {
		text := string(displayed[start:end])
		ul := diagUnderline[start]
		switch colTags[start] {
		case tagCursor:
			line.WriteString(cursorStyle.Render(text))
		case tagSel:
			line.WriteString(selectionStyle.Render(text))
		case tagFindCurrent:
			line.WriteString(findCurrentMatchStyle.Render(text))
		case tagFindMatch:
			line.WriteString(findMatchStyle.Render(text))
		case tagPlain:
			if isAgentLine {
				s := agentLineBgStyle
				if ul {
					s = s.Underline(true)
				}
				line.WriteString(s.Render(text))
			} else if ul {
				line.WriteString(diagUnderlineStyle.Render(text))
			} else {
				line.WriteString(text)
			}
		default:
			s := charStyles[start]
			if ul {
				s = s.Underline(true)
			}
			if isAgentLine {
				s = s.Background(agentLineBgColor)
			}
			line.WriteString(s.Render(text))
		}
	}

	if contentW > 0 {
		spanStart := 0
		for j := 1; j < contentW; j++ {
			if colTags[j] != colTags[spanStart] || diagUnderline[j] != diagUnderline[spanStart] {
				flushSpan(spanStart, j)
				spanStart = j
			}
		}
		flushSpan(spanStart, contentW)
	}

	return line.String()
}

func (m *EditorModel) renderRemovedLine(
	lineIdx, gutterW, contentW int,
	bgStyle lipgloss.Style,
	bgColor color.Color,
	gutterSt lipgloss.Style,
	cursorStyle lipgloss.Style,
	showBufferCursor bool,
) string {
	var line strings.Builder

	line.WriteString(gutterSt.Render(m.formatGutterDigits(lineIdx+1, gutterW-1) + "-"))

	// Sanitize agent-origin lines to prevent ANSI injection from LLM output.
	removedText := m.eng.LineText(lineIdx)
	if m.eng.LineOrigin(lineIdx) == editor.OriginAgent {
		var san sanitize.Sanitizer
		removedText = san.Sanitize(removedText)
	}
	rawRunes := []rune(removedText)
	expanded, bufToDisp := m.expandTabs(rawRunes)
	scrollCol := m.eng.ScrollCol
	displayed := m.fillDisplay(expanded, contentW, scrollCol)

	// Cursor position in display coords, offset by horizontal scroll.
	displayCursorCol := -1
	if showBufferCursor && lineIdx == m.eng.CursorLine && m.eng.CursorCol >= 0 && m.eng.CursorCol <= len(rawRunes) {
		displayCursorCol = bufToDisp[m.eng.CursorCol] - scrollCol
		if displayCursorCol < 0 || displayCursorCol >= contentW {
			displayCursorCol = -1
		}
	}

	// Syntax highlighting on removed lines (reuse scratch buffer).
	if cap(m.charStyles) < contentW {
		m.charStyles = make([]lipgloss.Style, contentW)
	}
	charStyles := m.charStyles[:contentW]
	clear(charStyles)
	if tokens := m.eng.HighlightLine(lineIdx); len(tokens) > 0 {
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
			dStart -= scrollCol
			dEnd -= scrollCol
			if dEnd <= 0 || dStart >= contentW {
				continue
			}
			if dStart < 0 {
				dStart = 0
			}
			style := styleForTokenKind(tok.Kind)
			for j := dStart; j < dEnd && j < contentW; j++ {
				charStyles[j] = style
			}
		}
	}

	// Span-based rendering for removed lines.
	if contentW > 0 {
		const (
			rTagBg     = 0
			rTagCursor = -1
		)
		if cap(m.colTags) < contentW {
			m.colTags = make([]int, contentW)
		}
		rColTags := m.colTags[:contentW]
		clear(rColTags)
		if m.syntaxTagMap == nil {
			m.syntaxTagMap = make(map[color.Color]int)
		}
		rSyntaxMap := m.syntaxTagMap
		clear(rSyntaxMap)
		rNextTag := 1
		for j := range contentW {
			switch {
			case j == displayCursorCol:
				rColTags[j] = rTagCursor
			case charStyles[j].GetForeground() != nil:
				fg := charStyles[j].GetForeground()
				tag, ok := rSyntaxMap[fg]
				if !ok {
					tag = rNextTag
					rSyntaxMap[fg] = tag
					rNextTag++
				}
				rColTags[j] = tag
			default:
				rColTags[j] = rTagBg
			}
		}
		rFlush := func(start, end int) {
			text := string(displayed[start:end])
			switch rColTags[start] {
			case rTagCursor:
				line.WriteString(cursorStyle.Render(text))
			case rTagBg:
				line.WriteString(bgStyle.Render(text))
			default:
				line.WriteString(charStyles[start].Background(bgColor).Render(text))
			}
		}
		spanStart := 0
		for j := 1; j < contentW; j++ {
			if rColTags[j] != rColTags[spanStart] {
				rFlush(spanStart, j)
				spanStart = j
			}
		}
		rFlush(spanStart, contentW)
	}

	return line.String()
}

func (m *EditorModel) renderAddedLine(
	overlayIdx, gutterW, contentW int,
	cursorStyle, selectionStyle, bgStyle, gutterSt lipgloss.Style,
) string {
	var line strings.Builder

	line.WriteString(gutterSt.Render(m.formatGutterBlank(gutterW, '+')))

	oe := m.Overlay.Editor
	// Sanitize overlay text — the overlay holds the agent's pending
	// replacement, so it must pass through the ANSI sanitizer before
	// reaching the terminal (parity with renderNormalLine/renderRemovedLine
	// agent-line paths).
	overlayText := oe.LineText(overlayIdx)
	var san sanitize.Sanitizer
	overlayText = san.Sanitize(overlayText)
	rawRunes := []rune(overlayText)
	expanded, bufToDisp := m.expandTabs(rawRunes)
	scrollCol := m.eng.ScrollCol
	displayed := m.fillDisplay(expanded, contentW, scrollCol)

	displayCursorCol := -1
	if m.Overlay.Active && overlayIdx == oe.CursorLine && oe.CursorCol >= 0 && oe.CursorCol <= len(rawRunes) {
		displayCursorCol = bufToDisp[oe.CursorCol] - scrollCol
		if displayCursorCol < 0 || displayCursorCol >= contentW {
			displayCursorCol = -1
		}
	}

	// Precompute inverse mapping for selection: viewport col → buffer col.
	// Reuse the shared scratch buffer to avoid per-call allocation.
	var dispToBuf []int
	if oe.SelectionActive {
		if cap(m.dispToBuf) < contentW {
			m.dispToBuf = make([]int, contentW)
		}
		dispToBuf = m.dispToBuf[:contentW]
		clear(dispToBuf)
		bufCol := 0
		for j := range contentW {
			absDispCol := j + scrollCol
			for bufCol+1 <= len(rawRunes) && bufToDisp[bufCol+1] <= absDispCol {
				bufCol++
			}
			dispToBuf[j] = bufCol
		}
	}

	// Span-based rendering for added lines.
	if contentW > 0 {
		const (
			aTagBg     = 0
			aTagCursor = -1
			aTagSel    = -2
		)
		if cap(m.colTags) < contentW {
			m.colTags = make([]int, contentW)
		}
		aColTags := m.colTags[:contentW]
		clear(aColTags)
		for j := range contentW {
			switch {
			case j == displayCursorCol:
				aColTags[j] = aTagCursor
			case oe.SelectionActive && oe.IsSelected(overlayIdx, dispToBuf[j]):
				aColTags[j] = aTagSel
			default:
				aColTags[j] = aTagBg
			}
		}
		aFlush := func(start, end int) {
			text := string(displayed[start:end])
			switch aColTags[start] {
			case aTagCursor:
				line.WriteString(cursorStyle.Render(text))
			case aTagSel:
				line.WriteString(selectionStyle.Render(text))
			default:
				line.WriteString(bgStyle.Render(text))
			}
		}
		spanStart := 0
		for j := 1; j < contentW; j++ {
			if aColTags[j] != aColTags[spanStart] {
				aFlush(spanStart, j)
				spanStart = j
			}
		}
		aFlush(spanStart, contentW)
	}

	return line.String()
}
