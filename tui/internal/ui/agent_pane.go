package ui

import (
	"log/slog"
	"slices"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/latebit-io/junto/tui/internal/sanitize"
	"github.com/mattn/go-runewidth"
)

// InputHeight is the number of rows reserved for the input area (separator + input + status).
const InputHeight = 5

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

// MaxInputBufferBytes caps the goal input buffer to prevent unbounded memory
// growth from large pastes or rapid key input. 1 MiB is generous for any
// reasonable prompt while still protecting against accidental megabyte pastes.
const MaxInputBufferBytes = 1 << 20 // 1 MiB

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
	Status string // "idle", "thinking", "typing", "editing", "waiting"

	// Selection
	SelectionActive bool
	SelectDragging  bool
	SelectStartLine int
	SelectStartCol  int
	CursorLine      int
	CursorCol       int

	// Input area
	InputActive  bool
	InputBuffer  string
	PlanningMode bool // true when input will start a planning conversation

	// Shared services
	Services *Services

	// Stateful sanitizer for streamed text
	sanitizer sanitize.Sanitizer

	// HasAgent is true when an LLM provider is configured.
	HasAgent bool
}

// NewAgentPaneModel creates a new agent pane.
func NewAgentPaneModel(svc *Services) *AgentPaneModel {
	return &AgentPaneModel{
		Status:   "idle",
		Services: svc,
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
	if msg.Button != tea.MouseLeft || msg.Y < 0 || msg.Y >= m.VisibleLines() {
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

// appendInput appends text to InputBuffer without exceeding MaxInputBufferBytes.
// It truncates on a rune boundary so partial runes are never stored.
func (m *AgentPaneModel) appendInput(text string) {
	remaining := MaxInputBufferBytes - len(m.InputBuffer)
	if remaining <= 0 {
		return
	}
	if len(text) > remaining {
		// Truncate to remaining bytes on a valid rune boundary.
		text = text[:remaining]
		for len(text) > 0 && !utf8.Valid([]byte(text)) {
			text = text[:len(text)-1]
		}
		slog.Warn("goal input truncated to cap", "cap", MaxInputBufferBytes)
	}
	m.InputBuffer += text
}

func (m *AgentPaneModel) handleInput(msg tea.KeyPressMsg) tea.Cmd {
	// Paste from system clipboard (Ctrl+V)
	if msg.Code == 'v' && msg.Mod == tea.ModCtrl {
		if text := m.Services.Clipboard.Read(); text != "" {
			text = strings.ReplaceAll(text, "\r\n", " ")
			text = strings.ReplaceAll(text, "\r", " ")
			text = strings.ReplaceAll(text, "\n", " ")
			text = strings.ReplaceAll(text, "\t", " ")
			m.appendInput(text)
		}
		return nil
	}

	switch msg.Code {
	case tea.KeyEnter:
		goal := strings.TrimSpace(m.InputBuffer)
		planning := m.PlanningMode
		m.InputActive = false
		m.InputBuffer = ""
		m.PlanningMode = false
		if goal != "" {
			if planning {
				return func() tea.Msg { return PlanningGoalSubmittedMsg{Goal: goal} }
			}
			return func() tea.Msg { return GoalSubmittedMsg{Goal: goal} }
		}
		return nil
	case tea.KeyEscape:
		m.InputActive = false
		m.InputBuffer = ""
		m.PlanningMode = false
		return nil
	case tea.KeyBackspace:
		if len(m.InputBuffer) > 0 {
			runes := []rune(m.InputBuffer)
			m.InputBuffer = string(runes[:len(runes)-1])
		}
		return nil
	case tea.KeySpace:
		m.appendInput(" ")
		return nil
	}

	// Printable text
	if msg.Text != "" {
		text := msg.Text
		text = strings.ReplaceAll(text, "\r\n", " ")
		text = strings.ReplaceAll(text, "\r", " ")
		text = strings.ReplaceAll(text, "\n", " ")
		text = strings.ReplaceAll(text, "\t", " ")
		slog.Debug("goal input text", "text_len", len(text))
		m.appendInput(text)
		return nil
	}
	return nil
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

	for i := firstAffected; i < len(m.RawLines); i++ {
		m.wrappedIndex = append(m.wrappedIndex, len(m.Lines))
		if m.Width > 0 && runewidth.StringWidth(m.RawLines[i]) > m.Width {
			m.Lines = append(m.Lines, m.wrapLine(m.RawLines[i])...)
		} else {
			m.Lines = append(m.Lines, m.RawLines[i])
		}
	}

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
	for _, raw := range m.RawLines {
		m.wrappedIndex = append(m.wrappedIndex, len(m.Lines))
		if m.Width > 0 && runewidth.StringWidth(raw) > m.Width {
			m.Lines = append(m.Lines, m.wrapLine(raw)...)
		} else {
			m.Lines = append(m.Lines, raw)
		}
	}
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
	m.ScrollOffset = 0
	m.Status = "idle"
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

	dimStyle := agentDimStyle
	statusStyle := agentStatusStyle
	selStyle := agentSelStyle
	inputActiveStyle := agentInputStyle
	inputDimStyle := agentInputDim
	cursorStyle := agentCursorStyle

	// No LLM configured — show message and fill remaining rows
	if !m.HasAgent {
		if row < m.Height {
			output[row] = dimStyle.Render(m.padLine(""))
			row++
		}
		if row < m.Height {
			output[row] = dimStyle.Render(m.padLine(" No LLM configured"))
			row++
		}
		if row < m.Height {
			output[row] = dimStyle.Render(m.padLine(" Set LLM_API_KEY to enable"))
			row++
		}
		for row < m.Height {
			output[row] = dimStyle.Render(m.padLine(""))
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
						line.WriteString(selStyle.Render(ch))
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
				output[row] = m.padLine(lineText)
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
		output[row] = dimStyle.Render(m.padLine(strings.Repeat("─", m.Width)))
		row++
	}

	// Input area: fill rows between separator and status
	inputRows := m.Height - row - 1 // -1 for status line
	if inputRows < 0 {
		inputRows = 0
	}
	var inputLines []string
	if m.InputActive {
		// Wrap input text to width-2 (1 for "> " prefix on first line)
		inputW := m.Width - 2
		if inputW < 1 {
			inputW = 1
		}
		buf := m.InputBuffer
		if len(buf) == 0 {
			inputLines = []string{""}
		} else {
			runes := []rune(buf)
			for len(runes) > inputW {
				inputLines = append(inputLines, string(runes[:inputW]))
				runes = runes[inputW:]
			}
			inputLines = append(inputLines, string(runes))
		}
	}

	for i := range inputRows {
		if row >= m.Height {
			break
		}
		if m.InputActive && i < len(inputLines) {
			prefix := "  "
			if i == 0 {
				prefix = "> "
			}
			lineText := prefix + inputLines[i]
			// Add cursor at the end of the last input line
			if i == len(inputLines)-1 {
				lineText += cursorStyle.Render(" ")
			}
			runes := []rune(lineText)
			if len(runes) < m.Width {
				lineText += strings.Repeat(" ", m.Width-len(runes))
			}
			output[row] = inputActiveStyle.Render(lineText)
		} else if !m.InputActive && i == 0 {
			output[row] = inputDimStyle.Render(m.padLine(" Ctrl+G code | Alt+G plan"))
		} else {
			output[row] = strings.Repeat(" ", m.Width)
		}
		row++
	}

	// Status line (last row)
	if row < m.Height {
		var statusText string
		switch m.Status {
		case "idle":
			statusText = dimStyle.Render(m.padLine(" Ready"))
		case "thinking":
			statusText = statusStyle.Render(m.padLine(" Thinking..."))
		case "planning":
			statusText = statusStyle.Render(m.padLine(" Planning..."))
		case "planning-waiting":
			statusText = statusStyle.Render(m.padLine(" Planning | :done to execute | :skip"))
		case "reviewing":
			statusText = statusStyle.Render(m.padLine(" Ctrl+O approve | Esc reject"))
		case "editing":
			statusText = statusStyle.Render(m.padLine(" Ctrl+N to continue"))
		case "waiting":
			statusText = statusStyle.Render(m.padLine(" Type to reply | Esc cancel"))
		case "typing":
			statusText = statusStyle.Render(m.padLine(" Agent typing... | Esc cancel"))
		default:
			statusText = m.padLine("")
		}
		output[row] = statusText
	}

	return strings.Join(output, "\n")
}
