package ui

import (
	"log/slog"
	"slices"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/tui/internal/sanitize"
	"github.com/latebit-io/junto/tui/internal/ui/textarea"
	"github.com/mattn/go-runewidth"
)

// InputHeight is the number of rows reserved for the input area (separator + input + status).
const InputHeight = 5

// fenceState records the active code fence after processing a raw line.
// Zero value means "not inside a code block".
type fenceState struct {
	char rune // '`' or '~', 0 when outside a block
	len  int  // fence run length, 0 when outside a block
}

// Package-level styles — allocated once, never in render paths.
var (
	userMessageStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("230")).Bold(true)
	agentDimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	agentStatusStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true)
	agentSelStyle    = lipgloss.NewStyle().Background(lipgloss.Color("24"))
	agentInputStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("230"))
	agentInputDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	agentCursorStyle = lipgloss.NewStyle().Reverse(true)
)

// AgentPaneModel is the Bubble Tea model for the agent reasoning pane.
type AgentPaneModel struct {
	Width  int
	Height int

	// RawLines stores unwrapped content; Lines is derived by wrapping to Width.
	// wrappedIndex[i] is the index into Lines where RawLines[i] starts.
	RawLines     []string
	Lines        []string
	wrappedIndex []int

	// userRawLines tracks which raw line indices are user messages.
	// Stable across rewrap — translated to wrapped indices via wrappedIndex in Render().
	userRawLines map[int]bool

	// Scroll
	ScrollOffset int

	// Status
	Status event.StatusKind

	// Selection
	SelectionActive bool
	SelectDragging  bool
	SelectStartLine int
	SelectStartCol  int
	CursorLine      int
	CursorCol       int

	// Input area
	InputActive       bool
	Input             *textarea.TextArea
	PlanningMode      bool // true when input will start a planning conversation
	inputAreaStartRow int  // first row of the input area (for mouse click detection)
	inputAreaEndRow   int  // exclusive end row

	// Shared services
	Services *Services

	// Stateful sanitizer for streamed text
	sanitizer sanitize.Sanitizer

	// inCodeAfter[i] is true when Lines[i] is inside a code block after
	// processing that line (i.e. the opening fence flips it to true, the
	// closing fence flips it back to false). Used by isCodeLine().
	inCodeAfter []bool

	// rawFenceAfter[i] records the active fence state after processing
	// RawLines[i]. This allows incremental recomputation in O(new_lines)
	// instead of rescanning the entire prefix on every token append.
	rawFenceAfter []fenceState

	// mdCache holds rendered markdown for Lines in the visible viewport only.
	// mdCacheOffset is the ScrollOffset the cache was built for; when the viewport
	// moves or content changes, the cache is invalidated. Bounded to O(VisibleLines).
	mdCache       []string
	mdCacheOffset int

	// HasAgent is true when an LLM provider is configured.
	HasAgent bool
}

// NewAgentPaneModel creates a new agent pane.
func NewAgentPaneModel(svc *Services) *AgentPaneModel {
	input := textarea.New(1) // width set properly in SetSize
	input.SetClipboard(svc.Clipboard)
	return &AgentPaneModel{
		Status:   event.StatusIdle,
		Services: svc,
		Input:    input,
	}
}

// Title returns the pane title for display in the border. Implements Titled.
func (m *AgentPaneModel) Title() string {
	return "Junto"
}

// SetSize updates the agent pane dimensions and clamps scroll. Implements Pane.
// Re-wraps content when width changes so text reflows correctly.
func (m *AgentPaneModel) SetSize(width, height int) {
	oldWidth := m.Width
	m.Width = width
	m.Height = height
	if width != oldWidth {
		m.rewrap()
	}
	m.Input.SetSize(width)
	m.clampScroll()
}

// Update handles messages for the agent pane. Implements Pane.
// Agent events (token, status, edit, error, done) are handled by AppModel
// and delivered via direct method calls (AppendToken, AppendText, etc.).
func (m *AgentPaneModel) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.MouseClickMsg:
		return m.handleMouseClick(msg)
	case tea.MouseMotionMsg:
		return m.handleMouseMotion(msg)
	case tea.MouseReleaseMsg:
		return m.handleMouseRelease(msg)
	case tea.MouseWheelMsg:
		return m.handleMouseWheel(msg)
	case tea.KeyPressMsg:
		if m.InputActive {
			return m.handleInput(msg)
		}
		return m.handleKey(msg)
	}
	return nil
}

// AppendToken sanitizes and appends streaming text from the agent.
// Uses the stateful sanitizer to handle escape sequences split across chunks.
func (m *AgentPaneModel) AppendToken(text string) {
	m.AppendText(m.sanitizer.Sanitize(text))
}

// AppendMeta sanitizes and appends non-stream text (edit proposals, errors, status).
// Uses a one-shot sanitizer so it doesn't interfere with the streaming sanitizer state.
func (m *AgentPaneModel) AppendMeta(text string) {
	var s sanitize.Sanitizer
	m.AppendText(s.Sanitize(text))
}

// AppendUserMessage appends the developer's follow-up message as plain text
// and marks the raw lines so Render() can style them distinctly. Tracks raw
// line indices (not wrapped) so styling survives rewrap on resize.
func (m *AgentPaneModel) AppendUserMessage(text string) {
	var s sanitize.Sanitizer
	text = s.Sanitize(text)

	firstRaw := len(m.RawLines)
	m.AppendText("\n\nYou: " + text + "\n\n")
	// Exclude the trailing empty raw line — AppendText reuses the last
	// raw line for the first chunk of the next append, so marking it
	// would misclassify the first agent token as a user message.
	if m.userRawLines == nil {
		m.userRawLines = make(map[int]bool)
	}
	endRaw := len(m.RawLines)
	if endRaw > firstRaw && m.RawLines[endRaw-1] == "" {
		endRaw--
	}
	for i := firstRaw; i < endRaw; i++ {
		m.userRawLines[i] = true
	}
}

// recomputeCodeBlock rebuilds inCodeAfter starting from raw line index fromRaw.
// Fence detection runs on RawLines (logical lines) so that wrapping can never
// split or fabricate a fence. The per-raw-line state is then projected to all
// wrapped lines belonging to that raw line via wrappedIndex.
//
// Fence state is persisted in rawFenceAfter so incremental appends seed from
// rawFenceAfter[fromRaw-1] in O(1) instead of rescanning the entire prefix.
//
// Fences follow CommonMark rules: 3+ backticks or tildes, 0–3 leading spaces,
// opener can have info string, closer must use the same char at >= opener
// length with no non-space content after. This correctly handles nested fences
// (e.g. ```“ wrapping an inner ```).
func (m *AgentPaneModel) recomputeCodeBlock(fromRaw int) {
	// Size inCodeAfter to match Lines.
	for len(m.inCodeAfter) < len(m.Lines) {
		m.inCodeAfter = append(m.inCodeAfter, false)
	}
	m.inCodeAfter = m.inCodeAfter[:len(m.Lines)]

	// Truncate rawFenceAfter to fromRaw so we rebuild from there.
	if fromRaw < len(m.rawFenceAfter) {
		m.rawFenceAfter = m.rawFenceAfter[:fromRaw]
	}

	// Seed from persisted state — O(1).
	var fence fenceState
	if fromRaw > 0 && fromRaw-1 < len(m.rawFenceAfter) {
		fence = m.rawFenceAfter[fromRaw-1]
	}

	for ri := fromRaw; ri < len(m.RawLines); ri++ {
		// User messages are rendered with userMessageStyle, not markdown.
		// Skip them so an unmatched fence in user input doesn't bleed into
		// subsequent agent output.
		if m.userRawLines[ri] {
			m.rawFenceAfter = append(m.rawFenceAfter, fence)
			wStart := m.wrappedIndex[ri]
			wEnd := len(m.Lines)
			if ri+1 < len(m.wrappedIndex) {
				wEnd = m.wrappedIndex[ri+1]
			}
			for wi := wStart; wi < wEnd; wi++ {
				m.inCodeAfter[wi] = false
			}
			continue
		}

		fenceBefore := fence
		fc, fl, closeable := parseFenceLine(m.RawLines[ri])
		if fl > 0 {
			if fence.len == 0 {
				// Not in a code block — any fence opens one (info string allowed).
				fence = fenceState{char: fc, len: fl}
			} else if closeable && fc == fence.char && fl >= fence.len {
				// In a code block — only close if no trailing non-space content.
				fence = fenceState{}
			}
		}

		// A raw line is "code" if we were inside a fence before processing it
		// (body + closer) OR if processing it opened a fence (opener). This
		// ensures all wrapped segments of a closer line are marked as code,
		// not just the first one.
		lineIsCode := fenceBefore.len > 0 || fence.len > 0

		// Persist fence state for this raw line.
		m.rawFenceAfter = append(m.rawFenceAfter, fence)

		// Fill all wrapped lines that belong to this raw line.
		wStart := m.wrappedIndex[ri]
		wEnd := len(m.Lines)
		if ri+1 < len(m.wrappedIndex) {
			wEnd = m.wrappedIndex[ri+1]
		}
		for wi := wStart; wi < wEnd; wi++ {
			m.inCodeAfter[wi] = lineIsCode
		}
	}
}

// parseFenceLine checks if line is a code fence (opener or closer).
// Returns the fence character ('`' or '~'), the run length, and whether the
// line can act as a closer (no non-space content after the fence run).
// Returns 0, 0, false if the line is not a fence at all.
//
// CommonMark rules: 0–3 leading spaces, 3+ of the same fence char. An opener
// may have trailing info text (canClose=false). A closer must have only
// optional trailing spaces (canClose=true).
func parseFenceLine(line string) (ch rune, count int, canClose bool) {
	runes := []rune(line)
	i := 0

	// Skip 0–3 leading spaces.
	spaces := 0
	for i < len(runes) && runes[i] == ' ' && spaces < 3 {
		i++
		spaces++
	}
	if i >= len(runes) {
		return 0, 0, false
	}

	ch = runes[i]
	if ch != '`' && ch != '~' {
		return 0, 0, false
	}

	// Count consecutive fence chars.
	start := i
	for i < len(runes) && runes[i] == ch {
		i++
	}
	count = i - start
	if count < 3 {
		return 0, 0, false
	}

	// A closer requires only optional trailing spaces after the fence run.
	canClose = true
	for j := i; j < len(runes); j++ {
		if runes[j] != ' ' && runes[j] != '\t' {
			canClose = false
			break
		}
	}

	return ch, count, canClose
}

// isCodeLine returns true when Lines[i] should be rendered with code block styling.
// This covers the opening fence, body lines, and the closing fence — all wrapped
// segments of a raw line share the same flag.
func (m *AgentPaneModel) isCodeLine(i int) bool {
	if i < 0 || i >= len(m.inCodeAfter) {
		return false
	}
	return m.inCodeAfter[i]
}

func (m *AgentPaneModel) handleMouseWheel(msg tea.MouseWheelMsg) tea.Cmd {
	scrollLines := 3
	switch msg.Button {
	case tea.MouseWheelUp:
		m.ScrollOffset -= scrollLines
		if m.ScrollOffset < 0 {
			m.ScrollOffset = 0
		}
	case tea.MouseWheelDown:
		maxScroll := len(m.Lines) - m.VisibleLines()
		if maxScroll < 0 {
			maxScroll = 0
		}
		m.ScrollOffset += scrollLines
		if m.ScrollOffset > maxScroll {
			m.ScrollOffset = maxScroll
		}
	}
	return nil
}

func (m *AgentPaneModel) handleMouseClick(msg tea.MouseClickMsg) tea.Cmd {
	if !m.HasAgent {
		return nil
	}
	if msg.Button != tea.MouseLeft {
		return nil
	}

	// Click in the input area → focus it. Click elsewhere → unfocus.
	if msg.Y >= m.inputAreaStartRow && msg.Y < m.inputAreaEndRow {
		m.InputActive = true
		return nil
	}
	m.InputActive = false

	if msg.Y < 0 || msg.Y >= m.VisibleLines() {
		return nil
	}
	line, col := m.mouseToLineCol(msg.X, msg.Y)
	m.SelectionActive = true
	m.SelectDragging = true
	m.SelectStartLine = line
	m.SelectStartCol = col
	m.CursorLine = line
	m.CursorCol = col
	return nil
}

func (m *AgentPaneModel) handleMouseMotion(msg tea.MouseMotionMsg) tea.Cmd {
	if !m.SelectDragging || msg.Y < 0 || msg.Y >= m.VisibleLines() {
		return nil
	}
	line, col := m.mouseToLineCol(msg.X, msg.Y)
	m.CursorLine = line
	m.CursorCol = col
	return nil
}

func (m *AgentPaneModel) handleMouseRelease(_ tea.MouseReleaseMsg) tea.Cmd {
	m.SelectDragging = false
	if m.CursorLine == m.SelectStartLine &&
		m.CursorCol == m.SelectStartCol {
		m.SelectionActive = false
	}
	return nil
}

// mouseToLineCol converts mouse coordinates to line and column indices.
func (m *AgentPaneModel) mouseToLineCol(x, y int) (int, int) {
	line := m.ScrollOffset + y
	if line < 0 {
		line = 0
	}
	if line >= len(m.Lines) {
		line = max(len(m.Lines)-1, 0)
	}

	col := 0
	if line < len(m.Lines) {
		cellX := x
		if cellX < 0 {
			cellX = 0
		}
		runes := []rune(m.Lines[line])
		cellsSeen := 0
		col = len(runes) // default: past end of line
		for ri, r := range runes {
			w := runewidth.RuneWidth(r)
			if cellsSeen+w > cellX {
				col = ri
				break
			}
			cellsSeen += w
		}
	}
	return line, col
}

// handleInput delegates key handling to the TextArea component.
// Submit (Enter) and Cancel (Escape) are intercepted to manage
// the agent pane's input lifecycle.
func (m *AgentPaneModel) handleInput(msg tea.KeyPressMsg) tea.Cmd {
	cmd := m.Input.Update(msg)
	if cmd == nil {
		return nil
	}

	// Inspect the command's message to handle submit/cancel.
	result := cmd()
	switch result.(type) {
	case textarea.SubmitMsg:
		goal := m.Input.Content()
		planning := m.PlanningMode
		m.InputActive = false
		m.Input.Reset()
		m.PlanningMode = false
		if strings.TrimSpace(goal) != "" {
			if planning {
				return func() tea.Msg { return PlanningGoalSubmittedMsg{Goal: goal} }
			}
			return func() tea.Msg { return GoalSubmittedMsg{Goal: goal} }
		}
		return nil
	case textarea.CancelMsg:
		// Deactivate focus but preserve content — Ctrl+G or click restores it.
		m.InputActive = false
		return nil
	default:
		// Re-wrap the command — we already consumed the thunk.
		return func() tea.Msg { return result }
	}
}

func (m *AgentPaneModel) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.Code {
	case tea.KeyUp:
		if m.ScrollOffset > 0 {
			m.ScrollOffset--
		}
	case tea.KeyDown:
		if m.ScrollOffset < len(m.Lines)-m.VisibleLines() {
			m.ScrollOffset++
		}
	case 'c':
		if msg.Mod == tea.ModCtrl {
			if m.SelectionActive {
				if err := m.Services.Clipboard.Write(m.SelectedText()); err != nil {
					slog.Warn("system clipboard write failed", "err", err)
				}
				m.SelectionActive = false
			} else {
				if err := m.Services.Clipboard.Write(strings.Join(m.Lines, "\n")); err != nil {
					slog.Warn("system clipboard write failed", "err", err)
				}
			}
		}
	}
	return nil
}

// AppendText adds streaming text to the agent pane.
// Raw text is stored unwrapped; wrapped Lines are derived incrementally
// for only the affected lines to avoid O(total_text) per token during streaming.
func (m *AgentPaneModel) AppendText(text string) {
	slog.Debug("agent pane append", "text_len", len(text))

	wasAtBottom := m.isAtBottom()

	parts := strings.Split(text, "\n")

	// Track which raw line index was modified vs appended
	firstAffected := len(m.RawLines) - 1
	if firstAffected < 0 {
		firstAffected = 0
	}

	for i, part := range parts {
		if i == 0 && len(m.RawLines) > 0 {
			m.RawLines[len(m.RawLines)-1] += part
		} else {
			m.RawLines = append(m.RawLines, part)
		}
	}

	// Incremental rewrap: truncate Lines to before firstAffected (O(1) via index),
	// then wrap only the affected raw lines.
	truncateTo := 0
	if firstAffected < len(m.wrappedIndex) {
		truncateTo = m.wrappedIndex[firstAffected]
	}
	m.Lines = m.Lines[:truncateTo]
	m.wrappedIndex = m.wrappedIndex[:firstAffected]
	if truncateTo < len(m.inCodeAfter) {
		m.inCodeAfter = m.inCodeAfter[:truncateTo]
	}
	m.invalidateMdCache()

	for i := firstAffected; i < len(m.RawLines); i++ {
		m.wrappedIndex = append(m.wrappedIndex, len(m.Lines))
		if m.Width > 0 && runewidth.StringWidth(m.RawLines[i]) > m.Width {
			m.Lines = append(m.Lines, m.wrapLine(m.RawLines[i])...)
		} else {
			m.Lines = append(m.Lines, m.RawLines[i])
		}
	}
	m.recomputeCodeBlock(firstAffected)

	if wasAtBottom {
		m.scrollToBottom()
	}
}

// isAtBottom returns true if the view is scrolled to (or near) the bottom.
func (m *AgentPaneModel) isAtBottom() bool {
	vis := m.VisibleLines()
	maxScroll := len(m.Lines) - vis
	if maxScroll <= 0 {
		return true
	}
	return m.ScrollOffset >= maxScroll-1
}

// rewrap derives wrapped Lines from RawLines for the current width.
func (m *AgentPaneModel) rewrap() {
	m.Lines = nil
	m.wrappedIndex = nil
	m.inCodeAfter = nil
	m.rawFenceAfter = nil
	m.invalidateMdCache()
	for _, raw := range m.RawLines {
		m.wrappedIndex = append(m.wrappedIndex, len(m.Lines))
		if m.Width > 0 && runewidth.StringWidth(raw) > m.Width {
			m.Lines = append(m.Lines, m.wrapLine(raw)...)
		} else {
			m.Lines = append(m.Lines, raw)
		}
	}
	m.recomputeCodeBlock(0)
}

func (m *AgentPaneModel) clampScroll() {
	maxScroll := len(m.Lines) - m.VisibleLines()
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.ScrollOffset > maxScroll {
		m.ScrollOffset = maxScroll
	}
}

// wrapLine wraps a single long line into multiple lines at word boundaries.
// Uses cell-width measurement to handle wide characters (CJK, emoji).
// Tracks remaining width incrementally to stay O(n) in line length.
func (m *AgentPaneModel) wrapLine(line string) []string {
	if m.Width <= 0 {
		return []string{line}
	}

	// Precompute per-rune widths once
	runes := []rune(line)
	widths := make([]int, len(runes))
	for i, r := range runes {
		widths[i] = runewidth.RuneWidth(r)
	}

	var result []string
	start := 0

	for start < len(runes) {
		// Find how many runes fit within m.Width cells
		cellW := 0
		fitEnd := start
		for i := start; i < len(runes); i++ {
			if cellW+widths[i] > m.Width {
				break
			}
			cellW += widths[i]
			fitEnd = i + 1
		}

		// If everything remaining fits, we're done
		if fitEnd == len(runes) {
			break
		}

		if fitEnd == start {
			fitEnd = start + 1 // always make progress
		}

		// Try to break at a space within the second half
		breakAt := fitEnd
		for i := fitEnd - 1; i > start+(fitEnd-start)/2; i-- {
			if runes[i] == ' ' {
				breakAt = i + 1
				break
			}
		}

		result = append(result, string(runes[start:breakAt]))
		start = breakAt
	}

	result = append(result, string(runes[start:]))
	return result
}

// Clear clears the agent pane content.
func (m *AgentPaneModel) Clear() {
	m.RawLines = nil
	m.Lines = nil
	m.wrappedIndex = nil
	m.userRawLines = nil
	m.inCodeAfter = nil
	m.rawFenceAfter = nil
	m.invalidateMdCache()
	m.ScrollOffset = 0
	m.Status = event.StatusIdle
	m.sanitizer = sanitize.Sanitizer{}
}

// VisibleLines returns the number of content lines visible.
// Layout: content + InputHeight bottom area (separator + input + status).
func (m *AgentPaneModel) VisibleLines() int {
	h := m.Height - InputHeight
	if h < 1 {
		h = 1
	}
	return h
}

func (m *AgentPaneModel) scrollToBottom() {
	vis := m.VisibleLines()
	if len(m.Lines) > vis {
		m.ScrollOffset = len(m.Lines) - vis
	} else {
		m.ScrollOffset = 0
	}
}

// SelectedRange returns the normalized (start, end) of the selection.
func (m *AgentPaneModel) SelectedRange() (int, int, int, int) {
	sl, sc := m.SelectStartLine, m.SelectStartCol
	el, ec := m.CursorLine, m.CursorCol
	if sl > el || (sl == el && sc > ec) {
		sl, sc, el, ec = el, ec, sl, sc
	}
	return sl, sc, el, ec
}

// SelectedText returns the text in the current selection.
func (m *AgentPaneModel) SelectedText() string {
	if !m.SelectionActive || len(m.Lines) == 0 {
		return ""
	}
	sl, sc, el, ec := m.SelectedRange()
	if sl < 0 {
		sl = 0
		sc = 0
	}
	if el >= len(m.Lines) {
		el = len(m.Lines) - 1
		ec = utf8.RuneCountInString(m.Lines[el])
	}
	if sl == el {
		if sl >= len(m.Lines) {
			return ""
		}
		runes := []rune(m.Lines[sl])
		if sc > len(runes) {
			sc = len(runes)
		}
		if ec > len(runes) {
			ec = len(runes)
		}
		return string(runes[sc:ec])
	}

	var sb strings.Builder
	if sl < len(m.Lines) {
		runes := []rune(m.Lines[sl])
		if sc > len(runes) {
			sc = len(runes)
		}
		sb.WriteString(string(runes[sc:]))
		sb.WriteRune('\n')
	}
	for i := sl + 1; i < el && i < len(m.Lines); i++ {
		sb.WriteString(m.Lines[i])
		sb.WriteRune('\n')
	}
	if el < len(m.Lines) {
		runes := []rune(m.Lines[el])
		if ec > len(runes) {
			ec = len(runes)
		}
		sb.WriteString(string(runes[:ec]))
	}
	return sb.String()
}

// isUserLine returns true if the wrapped line index corresponds to a user
// message. Uses binary search on wrappedIndex (which is sorted by construction)
// so cost is O(log n) per call instead of O(n).
func (m *AgentPaneModel) isUserLine(wrappedIdx int) bool {
	if len(m.userRawLines) == 0 || len(m.wrappedIndex) == 0 {
		return false
	}
	// BinarySearch finds the insertion point for wrappedIdx+1.
	// The owning raw line is one before that.
	rawIdx, _ := slices.BinarySearch(m.wrappedIndex, wrappedIdx+1)
	rawIdx--
	if rawIdx < 0 {
		return false
	}
	return m.userRawLines[rawIdx]
}

func (m *AgentPaneModel) isSelected(line, col int) bool {
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

// invalidateMdCache clears the viewport markdown cache so the next Render
// recomputes it. Called when Lines content changes (AppendText, rewrap, Clear).
func (m *AgentPaneModel) invalidateMdCache() {
	m.mdCache = m.mdCache[:0]
	m.mdCacheOffset = -1
}

// cachedMarkdown returns the rendered markdown for Lines[lineIdx]. The cache
// is viewport-scoped: it holds at most VisibleLines() entries starting at
// m.ScrollOffset, so memory is O(visible) regardless of transcript length.
func (m *AgentPaneModel) cachedMarkdown(lineIdx int) string {
	vis := m.VisibleLines()

	// Rebuild cache if viewport moved or was invalidated.
	if m.mdCacheOffset != m.ScrollOffset || len(m.mdCache) != vis {
		if cap(m.mdCache) >= vis {
			m.mdCache = m.mdCache[:vis]
		} else {
			m.mdCache = make([]string, vis)
		}
		// Zero out entries (reused slice may have stale data).
		for j := range m.mdCache {
			m.mdCache[j] = ""
		}
		m.mdCacheOffset = m.ScrollOffset
	}

	slot := lineIdx - m.mdCacheOffset
	if slot < 0 || slot >= len(m.mdCache) {
		// Outside viewport — compute without caching.
		return renderMarkdownLine(m.Lines[lineIdx], m.isCodeLine(lineIdx), m.Width)
	}
	if m.mdCache[slot] == "" {
		m.mdCache[slot] = renderMarkdownLine(m.Lines[lineIdx], m.isCodeLine(lineIdx), m.Width)
	}
	return m.mdCache[slot]
}

// padLine pads or truncates a string to exactly width display cells.
// Uses cell-width measurement to handle wide characters (CJK, emoji).
func (m *AgentPaneModel) padLine(s string) string {
	w := runewidth.StringWidth(s)
	if w >= m.Width {
		return runewidth.Truncate(s, m.Width, "")
	}
	return s + strings.Repeat(" ", m.Width-w)
}

// Render renders the agent pane as exactly m.Height lines joined by \n.
func (m *AgentPaneModel) Render() string {
	if m.Height <= 0 || m.Width <= 0 {
		return ""
	}

	output := make([]string, m.Height)
	row := 0

	// Use package-level style vars directly — no local copies needed
	// since lipgloss styles are immutable value types.

	// No LLM configured — show message and fill remaining rows
	if !m.HasAgent {
		if row < m.Height {
			output[row] = agentDimStyle.Render(m.padLine(""))
			row++
		}
		if row < m.Height {
			output[row] = agentDimStyle.Render(m.padLine(" No LLM configured"))
			row++
		}
		if row < m.Height {
			output[row] = agentDimStyle.Render(m.padLine(" Set LLM_API_KEY to enable"))
			row++
		}
		for row < m.Height {
			output[row] = agentDimStyle.Render(m.padLine(""))
			row++
		}
		return strings.Join(output, "\n")
	}

	// Content rows
	vis := m.VisibleLines()
	for i := range vis {
		if row >= m.Height {
			break
		}
		lineIdx := m.ScrollOffset + i
		if lineIdx < len(m.Lines) {
			lineText := m.Lines[lineIdx]

			sl, _, el, _ := m.SelectedRange()
			if m.SelectionActive && lineIdx >= sl && lineIdx <= el {
				// Render char-by-char with selection highlighting
				var line strings.Builder
				runes := []rune(lineText)
				cellsUsed := 0
				for j, r := range runes {
					w := runewidth.RuneWidth(r)
					if cellsUsed+w > m.Width {
						break
					}
					ch := string(r)
					if m.isSelected(lineIdx, j) {
						line.WriteString(agentSelStyle.Render(ch))
					} else {
						line.WriteString(ch)
					}
					cellsUsed += w
				}
				// Pad remaining cells
				if cellsUsed < m.Width {
					line.WriteString(strings.Repeat(" ", m.Width-cellsUsed))
				}
				output[row] = line.String()
			} else if m.isUserLine(lineIdx) {
				output[row] = userMessageStyle.Render(m.padLine(lineText))
			} else {
				output[row] = m.cachedMarkdown(lineIdx)
			}
		} else {
			output[row] = strings.Repeat(" ", m.Width)
		}
		row++
	}

	// Fill remaining content area
	contentEnd := m.Height - InputHeight
	for row < contentEnd {
		output[row] = strings.Repeat(" ", m.Width)
		row++
	}

	// Separator line
	if row < m.Height-1 { // -1 to leave room for status
		output[row] = agentDimStyle.Render(m.padLine(strings.Repeat("─", m.Width)))
		row++
	}

	// Input area: fill rows between separator and status
	inputRows := m.Height - row - 1 // -1 for status line
	if inputRows < 0 {
		inputRows = 0
	}
	// Always render the TextArea content — visible even when unfocused.
	renderedInput := m.Input.Render()
	hasContent := m.InputActive || (len(renderedInput) > 0 && (len(renderedInput) > 1 || renderedInput[0].Text != ""))
	var cursorVisRow, cursorVisCol int
	if m.InputActive {
		cursorVisRow, cursorVisCol = m.Input.CursorPosition()
	}

	// Window the rendered input so the cursor row is always visible.
	// inputScrollOffset is the first visual row to display.
	inputScrollOffset := 0
	if len(renderedInput) > inputRows && inputRows > 0 {
		// Ensure the cursor row is on screen.
		if cursorVisRow >= inputRows {
			inputScrollOffset = cursorVisRow - inputRows + 1
		}
		maxScroll := len(renderedInput) - inputRows
		if inputScrollOffset > maxScroll {
			inputScrollOffset = maxScroll
		}
	}

	// Track where the input area starts for mouse click detection.
	m.inputAreaStartRow = row
	m.inputAreaEndRow = row + inputRows

	for i := range inputRows {
		if row >= m.Height {
			break
		}
		visIdx := i + inputScrollOffset
		if hasContent && visIdx < len(renderedInput) {
			lineRunes := []rune(renderedInput[visIdx].Text)
			var lineBuilder strings.Builder
			cellsUsed := 0

			for j, r := range lineRunes {
				rw := runewidth.RuneWidth(r)
				if cellsUsed+rw > m.Width {
					break
				}
				ch := string(r)
				if m.InputActive && visIdx == cursorVisRow && j == cursorVisCol {
					lineBuilder.WriteString(agentCursorStyle.Render(ch))
				} else if m.InputActive && m.Input.IsSelected(visIdx, j) {
					lineBuilder.WriteString(agentSelStyle.Render(ch))
				} else {
					lineBuilder.WriteString(ch)
				}
				cellsUsed += rw
			}
			// Cursor at end of line (past last char) — only when focused.
			if m.InputActive && visIdx == cursorVisRow && cursorVisCol >= len(lineRunes) && cellsUsed < m.Width {
				lineBuilder.WriteString(agentCursorStyle.Render(" "))
				cellsUsed++
			}
			// Pad to full width.
			if cellsUsed < m.Width {
				lineBuilder.WriteString(strings.Repeat(" ", m.Width-cellsUsed))
			}
			style := agentInputStyle
			if !m.InputActive {
				style = agentInputDim
			}
			output[row] = style.Render(lineBuilder.String())
		} else if !hasContent && i == 0 {
			output[row] = agentInputDim.Render(m.padLine(" Ctrl+G code | Alt+G plan"))
		} else {
			output[row] = strings.Repeat(" ", m.Width)
		}
		row++
	}

	// Status line (last row)
	if row < m.Height {
		var statusText string
		switch m.Status {
		case event.StatusIdle:
			statusText = agentDimStyle.Render(m.padLine(" Ready"))
		case event.StatusThinking:
			statusText = agentStatusStyle.Render(m.padLine(" Thinking..."))
		case event.StatusPlanning:
			statusText = agentStatusStyle.Render(m.padLine(" Planning..."))
		case event.StatusPlanningWaiting:
			statusText = agentStatusStyle.Render(m.padLine(" Planning | :done to execute | :skip"))
		case event.StatusReviewing:
			statusText = agentStatusStyle.Render(m.padLine(" Ctrl+O approve | Esc reject"))
		case event.StatusEditing:
			statusText = agentStatusStyle.Render(m.padLine(" Ctrl+N to continue"))
		case event.StatusWaiting:
			statusText = agentStatusStyle.Render(m.padLine(" Type to reply | Enter send"))
		case event.StatusTyping:
			statusText = agentStatusStyle.Render(m.padLine(" Agent typing... | Esc cancel"))
		default:
			statusText = m.padLine("")
		}
		output[row] = statusText
	}

	return strings.Join(output, "\n")
}
