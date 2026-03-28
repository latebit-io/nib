// Package ui provides the Bubble Tea TUI components for junto.
package ui

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/latebit-io/junto/engine/buffer"
	"github.com/latebit-io/junto/engine/editor"
	"github.com/latebit-io/junto/engine/lang"
	"github.com/mattn/go-runewidth"
)

// Agent cursor styles — reused across render frames to avoid per-frame allocation.
var (
	agentCursorTypingStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("213")).
				Foreground(lipgloss.Color("0"))
	agentCursorWaitingStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("243")).
				Foreground(lipgloss.Color("0"))
	// agentLineGutterStyle renders the gutter for agent-written lines.
	agentLineGutterStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("34")) // green
)

// Hover overlay styles — floating panel for type info and documentation.
var (
	hoverStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("237")).
			Foreground(lipgloss.Color("252")).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("240")).
			Padding(0, 1)
	hoverCodeStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("114")) // green for code/signatures
	hoverDimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("245")) // dim for doc text
	hoverBoldStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252")).
			Bold(true)
)

// Diagnostic gutter styles — severity-colored icons in the gutter margin.
var (
	diagErrorGutterStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))  // red
	diagWarningGutterStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("11")) // yellow
	diagInfoGutterStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("12")) // blue
	diagUnderlineStyle     = lipgloss.NewStyle().Underline(true)
)

// Diagnostic gutter icons by severity.
const (
	diagErrorIcon   = "✖"
	diagWarningIcon = "▲"
	diagInfoIcon    = "●"
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
//
// The engine editor is a named field (not embedded) to prevent leaking
// 30+ engine methods into the TUI surface. AppModel and other TUI code
// access it via the Engine() accessor when direct engine interaction is
// needed (e.g., BeginIncrementalEdit, cursor position for collision).
type EditorModel struct {
	// eng is the engine editor — all domain logic lives here.
	// Named (not embedded) so engine methods aren't promoted into the TUI
	// surface. Use Engine() for explicit access from outside this type.
	eng *editor.Editor

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

	// Animated agent typing (nil when no animation is active)
	Anim *animationContext

	// TypingWPM controls the agent typing speed. 0 uses the default (800).
	TypingWPM int

	// cursorMoved is set when the cursor position changes and cleared after
	// Render. Used to avoid snapping scroll on every frame — only snap when
	// the cursor actually moved, allowing free scrolling during diff review.
	cursorMoved bool

	// Viewport mapping rebuilt each Render() for mouse click resolution.
	viewportMap []viewportEntry

	// OnSave is called after a successful buffer save. Used by AppModel
	// to notify the session (and language service) of saves. nil-safe.
	OnSave func()

	// diagnostics holds the current set of diagnostics for this file.
	// Set via SetDiagnostics which also builds the per-line lookup map.
	diagnostics []lang.Diagnostic

	// diagByLine maps line number → highest-severity diagnostic on that line.
	// Precomputed by SetDiagnostics for O(1) lookup during rendering.
	diagByLine map[int]*lang.Diagnostic

	// diagUnderline is a reusable scratch buffer for underline computation
	// in renderNormalLine. Avoids per-line per-frame allocation.
	diagUnderline []bool

	// hoverText holds the content for the hover overlay (type info, docs).
	// Empty string means no hover is active.
	hoverText string
	// hoverLine/hoverCol record where the hover was triggered so the
	// overlay dismisses when the cursor moves.
	hoverLine, hoverCol int
}

// NewEditorModel creates an editor model from an engine Editor.
func NewEditorModel(e *editor.Editor, km *Keymap, svc *Services) *EditorModel {
	return &EditorModel{
		eng:      e,
		Keymap:   km,
		Services: svc,
	}
}

// Engine returns the underlying engine editor. Use this for direct engine
// access from AppModel (e.g., BeginIncrementalEdit, cursor queries).
func (m *EditorModel) Engine() *editor.Editor {
	return m.eng
}

// SetDiagnostics updates the diagnostic list and precomputes the per-line
// lookup map. Use this instead of assigning diagnostics directly.
func (m *EditorModel) SetDiagnostics(diags []lang.Diagnostic) {
	m.diagnostics = diags
	if len(diags) == 0 {
		m.diagByLine = nil
		return
	}
	m.diagByLine = make(map[int]*lang.Diagnostic, len(diags))
	lineCount := m.eng.Buf.LineCount()
	for i := range m.diagnostics {
		d := &m.diagnostics[i]
		startLine := max(d.StartLine, 0)
		endLine := min(d.EndLine, lineCount-1)
		if startLine > endLine {
			continue
		}
		for line := startLine; line <= endLine; line++ {
			if existing, ok := m.diagByLine[line]; !ok || d.Severity < existing.Severity {
				m.diagByLine[line] = d
			}
		}
	}
}

// diagnosticForLine returns the highest-severity diagnostic touching the given line.
// O(1) lookup from the precomputed map built by SetDiagnostics.
func (m *EditorModel) diagnosticForLine(line int) *lang.Diagnostic {
	return m.diagByLine[line]
}

// Title returns the filename for display in the pane border. Implements Titled.
func (m *EditorModel) Title() string {
	name := m.eng.Buf.Path
	if name == "" {
		return "[new]"
	}
	return filepath.Base(name)
}

// ShowHover displays a hover overlay with the given text at the current cursor.
// Strips markdown formatting (code fences, horizontal rules) from LSP hover output.
func (m *EditorModel) ShowHover(text string) {
	m.hoverText = renderHoverMarkdown(text)
	m.hoverLine = m.eng.CursorLine
	m.hoverCol = m.eng.CursorCol
}

// renderHoverMarkdown converts gopls markdown hover output into styled
// terminal text. Handles code fences (colored), horizontal rules (dim
// separator), bold markers, and inline code spans.
func renderHoverMarkdown(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	inCode := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Code fence toggle.
		if strings.HasPrefix(trimmed, "```") {
			inCode = !inCode
			continue
		}

		// Horizontal rule → dim separator.
		if trimmed == "---" || trimmed == "___" || trimmed == "***" {
			out = append(out, hoverDimStyle.Render("─────"))
			continue
		}

		if inCode {
			out = append(out, hoverCodeStyle.Render(line))
		} else if trimmed == "" {
			out = append(out, "")
		} else {
			// Render inline markdown: **bold** and `code`.
			rendered := renderInlineMarkdown(line)
			out = append(out, rendered)
		}
	}

	// Trim leading/trailing blank lines.
	for len(out) > 0 && strings.TrimSpace(out[0]) == "" {
		out = out[1:]
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

// renderInlineMarkdown handles **bold** and `code` spans in a single line.
func renderInlineMarkdown(s string) string {
	var b strings.Builder
	runes := []rune(s)
	i := 0
	for i < len(runes) {
		// Bold: **text**
		if i+1 < len(runes) && runes[i] == '*' && runes[i+1] == '*' {
			end := -1
			for j := i + 2; j+1 < len(runes); j++ {
				if runes[j] == '*' && runes[j+1] == '*' {
					end = j
					break
				}
			}
			if end >= 0 {
				b.WriteString(hoverBoldStyle.Render(string(runes[i+2 : end])))
				i = end + 2
				continue
			}
		}
		// Inline code: `text`
		if runes[i] == '`' {
			end := -1
			for j := i + 1; j < len(runes); j++ {
				if runes[j] == '`' {
					end = j
					break
				}
			}
			if end >= 0 {
				b.WriteString(hoverCodeStyle.Render(string(runes[i+1 : end])))
				i = end + 1
				continue
			}
		}
		b.WriteRune(runes[i])
		i++
	}
	return b.String()
}

// DismissHover clears the hover overlay.
func (m *EditorModel) DismissHover() {
	m.hoverText = ""
}

// SetSize updates the editor dimensions. Implements Pane.
func (m *EditorModel) SetSize(width, height int) {
	m.eng.SetSize(width, height)
}

// Update handles key and mouse events for the editor pane. Implements Pane.
func (m *EditorModel) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.MouseMsg:
		return m.handleMouse(msg)
	case tea.KeyMsg:
		// Dismiss hover on any key — cursor is about to move.
		m.DismissHover()
		// Any key event may move the cursor — mark for scroll adjustment.
		m.cursorMoved = true
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
		m.eng.ExtraVisualLines = m.Overlay.LineCount()
	} else {
		m.eng.ExtraVisualLines = 0
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
	if m.cursorMoved {
		m.ensureCursorVisibleVisual()
		m.cursorMoved = false
	}
	m.syncExtraVisualLines()
	m.eng.ClampScroll()

	gutterW := m.eng.GutterWidth()
	contentW := m.eng.ContentWidth()
	vis := m.eng.VisibleLines()

	gutterStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	cursorStyle := lipgloss.NewStyle().Reverse(true)
	selectionStyle := lipgloss.NewStyle().Background(lipgloss.Color("24"))

	// Compute agent cursor info for this render pass.
	var agentCursor *agentCursorInfo
	if anim := m.Anim; anim != nil && anim.state != animIdle {
		style := agentCursorWaitingStyle
		if anim.state == animTyping {
			style = agentCursorTypingStyle
		}
		line, col := anim.edit.Position()
		agentCursor = &agentCursorInfo{
			line:  line,
			col:   col,
			style: style,
		}
	}

	removedBgColor := lipgloss.Color("52") // dark red
	removedBg := lipgloss.NewStyle().Background(removedBgColor)
	removedGutterSt := lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Background(removedBgColor)

	addedBgColor := lipgloss.Color("22") // dark green
	addedBg := lipgloss.NewStyle().Background(addedBgColor)
	addedGutterSt := lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Background(addedBgColor)

	output := make([]string, m.eng.Height)
	m.viewportMap = m.viewportMap[:0]

	overlay := m.Overlay
	addedCount := 0
	if overlay != nil {
		addedCount = overlay.LineCount()
	}
	showBufferCursor := overlay == nil || !overlay.Active

	for visualRow := range vis {
		if visualRow >= m.eng.Height-1 {
			break
		}
		vLine := m.eng.ScrollOffset + visualRow

		if overlay == nil {
			// No overlay — visual line == buffer line.
			if vLine >= m.eng.Buf.LineCount() {
				output[visualRow] = gutterStyle.Render(fmt.Sprintf("%*s ", gutterW-1, "~")) + strings.Repeat(" ", contentW)
				m.viewportMap = append(m.viewportMap, viewportEntry{kind: lineEmpty})
			} else {
				output[visualRow] = m.renderNormalLine(vLine, gutterW, contentW, gutterStyle, cursorStyle, selectionStyle, showBufferCursor, agentCursor)
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
			output[visualRow] = m.renderNormalLine(vLine, gutterW, contentW, gutterStyle, cursorStyle, selectionStyle, showBufferCursor, agentCursor)
			m.viewportMap = append(m.viewportMap, viewportEntry{kind: lineNormal, bufLine: vLine})

		case vLine <= overlay.EndLine:
			// Removed (red) line.
			output[visualRow] = m.renderRemovedLine(vLine, gutterW, contentW, removedBg, removedBgColor, removedGutterSt, cursorStyle, showBufferCursor)
			m.viewportMap = append(m.viewportMap, viewportEntry{kind: lineRemoved, bufLine: vLine})

		case vLine >= addedStart && vLine <= addedEnd:
			// Added (green) overlay line.
			addedIdx := vLine - addedStart
			output[visualRow] = m.renderAddedLine(addedIdx, gutterW, contentW, cursorStyle, selectionStyle, addedBg, addedGutterSt)
			m.viewportMap = append(m.viewportMap, viewportEntry{kind: lineAdded, overlayLine: addedIdx})

		default:
			// Normal line after diff — subtract added lines to get buffer line.
			bufLine := vLine - addedCount
			if bufLine >= m.eng.Buf.LineCount() {
				output[visualRow] = gutterStyle.Render(fmt.Sprintf("%*s ", gutterW-1, "~")) + strings.Repeat(" ", contentW)
				m.viewportMap = append(m.viewportMap, viewportEntry{kind: lineEmpty})
			} else {
				output[visualRow] = m.renderNormalLine(bufLine, gutterW, contentW, gutterStyle, cursorStyle, selectionStyle, showBufferCursor, agentCursor)
				m.viewportMap = append(m.viewportMap, viewportEntry{kind: lineNormal, bufLine: bufLine})
			}
		}
	}

	// Fill remaining rows with tildes.
	for visualRow := vis; visualRow < m.eng.Height-1; visualRow++ {
		output[visualRow] = gutterStyle.Render(fmt.Sprintf("%*s ", gutterW-1, "~")) + strings.Repeat(" ", contentW)
		m.viewportMap = append(m.viewportMap, viewportEntry{kind: lineEmpty})
	}

	// Status bar (last row).
	if m.eng.Height > 0 {
		output[m.eng.Height-1] = m.renderStatusBar()
	}

	// Hover overlay — auto-dismiss if cursor moved from trigger position.
	if m.hoverText != "" {
		if m.eng.CursorLine != m.hoverLine || m.eng.CursorCol != m.hoverCol {
			m.hoverText = ""
		} else {
			m.overlayHover(output, gutterW, contentW)
		}
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
	vis := m.eng.VisibleLines()
	if vis <= 0 {
		return
	}

	var visualCursor int
	if overlay.Active {
		// Overlay cursor: added lines start at visual line EndLine+1.
		visualCursor = overlay.EndLine + 1 + overlay.Editor.CursorLine
	} else {
		// Buffer cursor: shift by addedCount if past the diff.
		visualCursor = m.eng.CursorLine
		if m.eng.CursorLine > overlay.EndLine {
			visualCursor = m.eng.CursorLine + addedCount
		}
	}

	if visualCursor < m.eng.ScrollOffset {
		m.eng.ScrollOffset = visualCursor
	}
	if visualCursor >= m.eng.ScrollOffset+vis {
		m.eng.ScrollOffset = visualCursor - vis + 1
	}
}

func (m *EditorModel) renderNormalLine(
	lineIdx, gutterW, contentW int,
	gutterStyle, cursorStyle, selectionStyle lipgloss.Style,
	showCursor bool,
	agentCursor *agentCursorInfo,
) string {
	var line strings.Builder

	numText := fmt.Sprintf("%*d", gutterW-1, lineIdx+1)
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
	} else if m.eng.Buf.LineOrigin(lineIdx) == buffer.OriginAgent {
		line.WriteString(agentLineGutterStyle.Render(numText + "j"))
	} else {
		line.WriteString(gutterStyle.Render(numText + " "))
	}

	rawRunes := []rune(m.eng.Buf.LineText(lineIdx))
	expanded, bufToDisp := expandTabs(rawRunes)
	displayed := fillDisplay(expanded, contentW)

	// Cursor position in display coords.
	displayCursorCol := -1
	if showCursor && lineIdx == m.eng.CursorLine && m.eng.CursorCol >= 0 && m.eng.CursorCol <= len(rawRunes) {
		displayCursorCol = bufToDisp[m.eng.CursorCol]
		if displayCursorCol >= contentW && contentW > 0 {
			displayCursorCol = contentW - 1
		}
	}

	// Agent cursor in display coords.
	displayAgentCol := -1
	var agentStyle lipgloss.Style
	if agentCursor != nil && lineIdx == agentCursor.line {
		agentStyle = agentCursor.style
		if agentCursor.col >= 0 && agentCursor.col <= len(rawRunes) {
			displayAgentCol = bufToDisp[agentCursor.col]
		} else if agentCursor.col > len(rawRunes) {
			// Past end of line — show at end
			displayAgentCol = bufToDisp[len(rawRunes)]
		}
		if displayAgentCol >= contentW && contentW > 0 {
			displayAgentCol = contentW - 1
		}
	}

	// Syntax highlighting.
	charStyles := make([]lipgloss.Style, contentW)
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
			style := styleForTokenKind(tok.Kind)
			for j := dStart; j < dEnd && j < contentW; j++ {
				charStyles[j] = style
			}
		}
	}

	// Selection: precompute inverse mapping display col → buffer col.
	var dispToBuf []int
	if m.eng.SelectionActive {
		dispToBuf = make([]int, contentW)
		bufCol := 0
		for j := range contentW {
			for bufCol+1 <= len(rawRunes) && bufToDisp[bufCol+1] <= j {
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
		startDisp := bufToDisp[startBufCol]
		endDisp := bufToDisp[endBufCol]
		// Zero-width diagnostics (e.g., missing token) get at least one cell.
		if endDisp <= startDisp && startDisp < contentW {
			endDisp = startDisp + 1
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
	colTags := make([]int, contentW)
	const (
		tagPlain  = 0
		tagCursor = -1
		tagAgent  = -2
		tagSel    = -3
	)
	// Syntax tokens get positive tags starting at 1, grouped by foreground color.
	syntaxTagMap := make(map[lipgloss.TerminalColor]int)
	nextTag := 1
	for j := range contentW {
		switch {
		case j == displayCursorCol:
			colTags[j] = tagCursor
		case j == displayAgentCol:
			colTags[j] = tagAgent
		case m.eng.SelectionActive && m.eng.IsSelected(lineIdx, dispToBuf[j]):
			colTags[j] = tagSel
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
		case tagAgent:
			line.WriteString(agentStyle.Render(text))
		case tagSel:
			line.WriteString(selectionStyle.Render(text))
		case tagPlain:
			if ul {
				line.WriteString(diagUnderlineStyle.Render(text))
			} else {
				line.WriteString(text)
			}
		default:
			s := charStyles[start]
			if ul {
				s = s.Underline(true)
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
	bgColor lipgloss.Color,
	gutterSt lipgloss.Style,
	cursorStyle lipgloss.Style,
	showBufferCursor bool,
) string {
	var line strings.Builder

	gutterText := fmt.Sprintf("%*d-", gutterW-1, lineIdx+1)
	line.WriteString(gutterSt.Render(gutterText))

	rawRunes := []rune(m.eng.Buf.LineText(lineIdx))
	expanded, bufToDisp := expandTabs(rawRunes)
	displayed := fillDisplay(expanded, contentW)

	// Cursor position in display coords.
	displayCursorCol := -1
	if showBufferCursor && lineIdx == m.eng.CursorLine && m.eng.CursorCol >= 0 && m.eng.CursorCol <= len(rawRunes) {
		displayCursorCol = bufToDisp[m.eng.CursorCol]
		if displayCursorCol >= contentW && contentW > 0 {
			displayCursorCol = contentW - 1
		}
	}

	// Syntax highlighting on removed lines (they are real buffer lines).
	charStyles := make([]lipgloss.Style, contentW)
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
		rColTags := make([]int, contentW)
		rSyntaxMap := make(map[lipgloss.TerminalColor]int)
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
	var dispToBuf []int
	if oe.SelectionActive {
		dispToBuf = make([]int, contentW)
		bufCol := 0
		for j := range contentW {
			for bufCol+1 <= len(rawRunes) && bufToDisp[bufCol+1] <= j {
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
		aColTags := make([]int, contentW)
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

// overlayHover renders a floating hover panel over the editor output lines.
// Positioned below the hover trigger line, capped to not exceed the editor area.
func (m *EditorModel) overlayHover(output []string, gutterW, contentW int) {
	// Determine the visual row to place the overlay below.
	visualRow := m.hoverLine - m.eng.ScrollOffset + 1
	if visualRow < 0 || visualRow >= m.eng.Height-1 {
		return // cursor scrolled off screen
	}

	// Wrap hover text to fit within the content area.
	maxWidth := contentW
	if maxWidth > 60 {
		maxWidth = 60
	}
	if maxWidth < 10 {
		return
	}

	// Split and truncate hover lines. Account for border (2 rows).
	hoverLines := strings.Split(m.hoverText, "\n")
	maxLines := m.eng.Height - 1 - visualRow - 2 // -2 for border rows
	if maxLines > 10 {
		maxLines = 10
	}
	if maxLines < 1 {
		return
	}
	if len(hoverLines) > maxLines {
		hoverLines = hoverLines[:maxLines]
	}
	if len(hoverLines) == 0 {
		return
	}

	// Render the hover box via lipgloss with fixed width for consistent borders.
	content := strings.Join(hoverLines, "\n")
	box := hoverStyle.Width(maxWidth - 4).Render(content) // -4 for border+padding
	boxLines := strings.Split(box, "\n")

	// Replace entire output rows with gutter padding + hover box line.
	// This avoids ANSI escape sequence corruption from rune-level splicing.
	gutterPad := strings.Repeat(" ", gutterW)
	for i, bl := range boxLines {
		row := visualRow + i
		if row >= m.eng.Height-1 {
			break
		}
		output[row] = gutterPad + bl
	}
}

// sanitizeStatusText strips ANSI escapes, collapses whitespace/newlines to
// single spaces, and truncates to a safe length for the status bar.
func sanitizeStatusText(s string) string {
	const maxLen = 200
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
		if r == '\n' || r == '\r' || r == '\t' {
			r = ' '
		}
		if b.Len() >= maxLen {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (m *EditorModel) renderStatusBar() string {
	statusStyle := lipgloss.NewStyle().
		Background(lipgloss.Color("62")).
		Foreground(lipgloss.Color("230")).
		Bold(true)

	name := m.eng.Buf.Path
	if name == "" {
		name = "[new]"
	}
	modified := ""
	if m.eng.Buf.Modified {
		modified = " [+]"
	}

	left := fmt.Sprintf(" %s%s", name, modified)
	if m.StatusMsg != "" {
		left += "  " + m.StatusMsg
	} else if diag := m.diagnosticForLine(m.eng.CursorLine); diag != nil {
		var prefix string
		switch diag.Severity {
		case lang.SeverityError:
			prefix = "error"
		case lang.SeverityWarning:
			prefix = "warning"
		default:
			prefix = "info"
		}
		left += "  " + prefix + ": " + sanitizeStatusText(diag.Message)
	}

	// Show cursor position: overlay, agent animation, or buffer.
	var right string
	switch {
	case m.Overlay != nil && m.Overlay.Active:
		right = fmt.Sprintf(" +%d:%d ", m.Overlay.Editor.CursorLine+1, m.Overlay.Editor.CursorCol+1)
	case m.Anim != nil && m.Anim.state == animTyping:
		al, ac := m.Anim.edit.Position()
		right = fmt.Sprintf(" %d:%d  agent:%d:%d ", m.eng.CursorLine+1, m.eng.CursorCol+1, al+1, ac+1)
	default:
		right = fmt.Sprintf(" %d:%d ", m.eng.CursorLine+1, m.eng.CursorCol+1)
	}

	leftW := runewidth.StringWidth(left)
	rightW := runewidth.StringWidth(right)
	padding := m.eng.Width - leftW - rightW
	if padding < 0 {
		padding = 0
	}

	bar := left + strings.Repeat(" ", padding) + right
	bar = runewidth.Truncate(bar, m.eng.Width, "")

	return statusStyle.Render(bar)
}

// --- Mouse Handling ---

func (m *EditorModel) handleMouse(msg tea.MouseMsg) tea.Cmd {
	scrollLines := 3

	switch {
	case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonWheelUp:
		m.syncExtraVisualLines()
		m.eng.ScrollUp(scrollLines)
		return nil

	case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonWheelDown:
		m.syncExtraVisualLines()
		m.eng.ScrollDown(scrollLines)
		return nil
	}

	if msg.Button != tea.MouseButtonLeft || msg.Y >= m.eng.VisibleLines() {
		return nil
	}

	// Mouse click moves the cursor — mark for scroll adjustment.
	m.cursorMoved = true

	gutterW := m.eng.GutterWidth()
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
				m.eng.ClearSelection()
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
		// Removed lines are real buffer lines — allow cursor positioning
		// for navigation. Edits are blocked by overlapsRemovedRange().
		m.handleNormalLineClick(entry.bufLine, displayCol, msg.Action)

	case lineEmpty:
		// Clicks past EOF: deactivate overlay and move cursor to last line.
		if m.Overlay != nil && m.Overlay.Active {
			m.Overlay.Active = false
		}
		m.handleNormalLineClick(m.eng.Buf.LineCount(), displayCol, msg.Action)
	}

	return nil
}

func (m *EditorModel) handleNormalLineClick(bufLine, displayCol int, action tea.MouseAction) {
	var bufCol int
	if bufLine >= m.eng.Buf.LineCount() {
		bufLine = m.eng.Buf.LineCount() - 1
		if bufLine < 0 {
			bufLine = 0
		}
		bufCol = m.eng.Buf.LineLen(bufLine)
	} else {
		bufCol = m.eng.DisplayColToBufferCol(bufLine, displayCol)
	}

	switch action {
	case tea.MouseActionPress:
		m.eng.ClearSelection()
		m.eng.MoveCursorTo(bufLine, bufCol)
		m.eng.SelectionActive = true
		m.eng.SelectStartLine = m.eng.CursorLine
		m.eng.SelectStartCol = m.eng.CursorCol
	case tea.MouseActionMotion:
		if m.eng.SelectionActive {
			m.eng.MoveCursorTo(bufLine, bufCol)
		}
	case tea.MouseActionRelease:
		if m.eng.SelectionActive &&
			m.eng.CursorLine == m.eng.SelectStartLine &&
			m.eng.CursorCol == m.eng.SelectStartCol {
			m.eng.ClearSelection()
		}
	}
}

// --- Key Handling ---

// overlapsRemovedRange returns true when the cursor or selection touches the
// overlay's removed line range or its immediate boundary lines. The boundary
// extension prevents newline-join operations (Backspace at col 0 of EndLine+1,
// Delete at end of StartLine-1) from merging into removed lines.
func (m *EditorModel) overlapsRemovedRange() bool {
	if m.Overlay == nil || m.Overlay.Active {
		return false
	}
	start, end := m.Overlay.StartLine, m.Overlay.EndLine

	// Cursor on a removed line → read-only.
	if m.eng.CursorLine >= start && m.eng.CursorLine <= end {
		return true
	}
	// Cursor on the line just before the removed range, at end of line:
	// Delete would join into StartLine.
	if m.eng.CursorLine == start-1 && m.eng.CursorCol >= m.eng.Buf.LineLen(m.eng.CursorLine) {
		return true
	}
	// Cursor on the line just after the removed range, at col 0:
	// Backspace would join into EndLine.
	if m.eng.CursorLine == end+1 && m.eng.CursorCol == 0 {
		return true
	}

	if m.eng.SelectionActive {
		selStart, selEnd := m.eng.SelectStartLine, m.eng.CursorLine
		if selStart > selEnd {
			selStart, selEnd = selEnd, selStart
		}
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

	// Intercept arrow keys that would cross into the removed range.
	// Enter the overlay directly so the cursor doesn't land invisibly
	// on a removed line behind the green overlay.
	if m.Overlay != nil && !m.Overlay.Active {
		if entered := m.interceptOverlayEntry(keyMsg); entered {
			return nil
		}
	}

	// Track line count so we can adjust overlay position if the user
	// inserts/deletes lines above the diff.
	linesBefore := m.eng.Buf.LineCount()

	readOnly := m.overlapsRemovedRange()
	cmd := m.handleEditorKeyFor(keyMsg, m.eng, readOnly)

	m.adjustOverlayPosition(linesBefore)
	return cmd
}

// interceptOverlayEntry checks if an arrow key would move the cursor into the
// removed range and enters the overlay instead. Returns true if intercepted.
func (m *EditorModel) interceptOverlayEntry(keyMsg tea.KeyMsg) bool {
	o := m.Overlay
	if o == nil {
		return false
	}

	switch keyMsg.Type {
	case tea.KeyDown:
		// Cursor just above removed range → enter overlay at first line.
		if m.eng.CursorLine == o.StartLine-1 {
			o.Active = true
			o.Editor.MoveCursorTo(0, m.eng.CursorCol)
			m.eng.ClearSelection()
			return true
		}
	case tea.KeyUp:
		// Cursor just below removed range → enter overlay at last line.
		if m.eng.CursorLine == o.EndLine+1 {
			o.Active = true
			lastLine := o.Editor.Buf.LineCount() - 1
			o.Editor.MoveCursorTo(lastLine, m.eng.CursorCol)
			m.eng.ClearSelection()
			return true
		}
	}
	return false
}

// adjustOverlayPosition shifts the overlay's line range when lines are
// inserted or deleted above it in the main buffer.
func (m *EditorModel) adjustOverlayPosition(linesBefore int) {
	if m.Overlay == nil {
		return
	}
	delta := m.eng.Buf.LineCount() - linesBefore
	if delta == 0 {
		return
	}
	// Edits happen at the cursor. Only adjust if the cursor is above the overlay.
	if m.eng.CursorLine <= m.Overlay.StartLine {
		m.Overlay.StartLine += delta
		m.Overlay.EndLine += delta
		slog.Debug("overlay position adjusted", "delta", delta, "newStart", m.Overlay.StartLine, "newEnd", m.Overlay.EndLine)
		// If the overlay shifted to an invalid position, remove it.
		if m.Overlay.StartLine < 0 || m.Overlay.EndLine < 0 {
			slog.Debug("overlay removed", "reason", "shifted to invalid position")
			m.Overlay = nil
			m.eng.ExtraVisualLines = 0
		}
	}
}

// handleOverlayKey handles keys when the overlay editor is active.
// Overlay-specific: boundary exit (up/down past edges), escape to deactivate,
// save always goes to main buffer. Everything else delegates to the shared handler.
func (m *EditorModel) handleOverlayKey(keyMsg tea.KeyMsg) tea.Cmd {
	o := m.Overlay
	oe := o.Editor

	// Escape — leave overlay, move cursor to nearest non-removed line
	// so subsequent navigation doesn't immediately re-enter the overlay.
	if keyMsg.Type == tea.KeyEscape {
		slog.Debug("overlay deactivated", "reason", "escape")
		oe.ClearSelection()
		o.Active = false
		if o.StartLine > 0 {
			m.eng.MoveCursorTo(o.StartLine-1, oe.CursorCol)
		} else if o.EndLine+1 < m.eng.Buf.LineCount() {
			m.eng.MoveCursorTo(o.EndLine+1, oe.CursorCol)
		}
		return nil
	}

	// Up at top of overlay — exit upward if there's a safe line above.
	if keyMsg.Type == tea.KeyUp && oe.CursorLine == 0 {
		if o.StartLine <= 0 {
			// No buffer line above the overlay — stay in overlay.
			return nil
		}
		slog.Debug("overlay deactivated", "reason", "arrow up past top", "target", o.StartLine-1)
		oe.ClearSelection()
		o.Active = false
		m.eng.MoveCursorTo(o.StartLine-1, oe.CursorCol)
		return nil
	}

	// Down at bottom of overlay — exit downward if there's a safe line below.
	if keyMsg.Type == tea.KeyDown && oe.CursorLine >= oe.Buf.LineCount()-1 {
		if o.EndLine+1 >= m.eng.Buf.LineCount() {
			// No buffer line below the overlay — stay in overlay.
			return nil
		}
		slog.Debug("overlay deactivated", "reason", "arrow down past bottom", "target", o.EndLine+1)
		oe.ClearSelection()
		o.Active = false
		m.eng.MoveCursorTo(o.EndLine+1, oe.CursorCol)
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
		if err := m.eng.Save(); err != nil {
			m.StatusMsg = "Save failed: " + err.Error()
		} else {
			m.StatusMsg = "Saved"
			if m.OnSave != nil {
				m.OnSave()
			}
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
		// Use the main viewport's visible height for page jumps, not the
		// target editor's own Height (which may differ for the overlay editor).
		e.MoveCursor(-m.eng.VisibleLines(), 0)
		return nil
	case tea.KeyPgDown:
		e.ClearSelection()
		m.syncExtraVisualLines()
		e.MoveCursor(m.eng.VisibleLines(), 0)
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
