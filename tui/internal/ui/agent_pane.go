package ui

import (
	"fmt"
	"log/slog"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/latebit-io/junto/tui/internal/agent"
)

// InputHeight is the number of rows reserved for the input area (separator + input + status).
const InputHeight = 5

// AgentPaneModel is the Bubble Tea model for the agent reasoning pane.
type AgentPaneModel struct {
	Width  int
	Height int

	// RawLines stores unwrapped content; Lines is derived by wrapping to Width.
	RawLines []string
	Lines    []string

	// Scroll
	ScrollOffset int

	// Status
	Status    string // "idle", "thinking", "editing", "waiting"
	StepNum   int
	StepTotal int
	StepDesc  string

	// Selection
	SelectionActive bool
	SelectStartLine int
	SelectStartCol  int
	CursorLine      int
	CursorCol       int

	// Input area
	InputActive bool
	InputBuffer string

	// Shared services
	Services *Services

	// Stateful sanitizer for streamed text
	sanitizer sanitizer
}

// NewAgentPaneModel creates a new agent pane.
func NewAgentPaneModel(svc *Services) *AgentPaneModel {
	return &AgentPaneModel{
		Status:   "idle",
		Services: svc,
	}
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
func (m *AgentPaneModel) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case agent.TokenMsg:
		m.AppendText(m.sanitizer.sanitize(msg.Text))
		return nil

	case agent.StatusMsg:
		m.Status = msg.Status
		return nil

	case agent.EditProposedMsg:
		m.Status = "waiting"
		m.AppendText("\n--- Proposed: " + msg.Edit.Reason + " ---\n")
		return nil

	case agent.ErrorMsg:
		m.AppendText("\nError: " + msg.Err + "\n")
		return nil

	case agent.DoneMsg:
		m.Status = "idle"
		m.AppendText("\n--- Done ---\n")
		return nil

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case tea.KeyMsg:
		if m.InputActive {
			return m.handleInput(msg)
		}
		return m.handleKey(msg)
	}
	return nil
}

func (m *AgentPaneModel) handleMouse(msg tea.MouseMsg) tea.Cmd {
	scrollLines := 3

	switch {
	case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonWheelUp:
		m.ScrollOffset -= scrollLines
		if m.ScrollOffset < 0 {
			m.ScrollOffset = 0
		}
		return nil

	case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonWheelDown:
		maxScroll := len(m.Lines) - m.VisibleLines()
		if maxScroll < 0 {
			maxScroll = 0
		}
		m.ScrollOffset += scrollLines
		if m.ScrollOffset > maxScroll {
			m.ScrollOffset = maxScroll
		}
		return nil
	}

	// Click/drag in content area only (skip header row 0, input area, and status)
	if msg.Button == tea.MouseButtonLeft && msg.Y > 0 && msg.Y <= m.VisibleLines() {
		col := msg.X
		if col < 0 {
			col = 0
		}
		line := m.ScrollOffset + msg.Y - 1 // -1 for header row
		if line < 0 {
			line = 0
		}
		if line >= len(m.Lines) {
			line = max(len(m.Lines)-1, 0)
		}

		switch msg.Action {
		case tea.MouseActionPress:
			m.SelectionActive = true
			m.SelectStartLine = line
			m.SelectStartCol = col
			m.CursorLine = line
			m.CursorCol = col
		case tea.MouseActionMotion:
			if m.SelectionActive {
				m.CursorLine = line
				m.CursorCol = col
			}
		case tea.MouseActionRelease:
			if m.SelectionActive &&
				m.CursorLine == m.SelectStartLine &&
				m.CursorCol == m.SelectStartCol {
				m.SelectionActive = false
			}
		}
	}

	return nil
}

func (m *AgentPaneModel) handleInput(msg tea.KeyMsg) tea.Cmd {
	// Paste from system clipboard (Ctrl+V)
	if msg.Type == tea.KeyCtrlV {
		if text := m.Services.Clipboard.Read(); text != "" {
			text = strings.ReplaceAll(text, "\r\n", " ")
			text = strings.ReplaceAll(text, "\r", " ")
			text = strings.ReplaceAll(text, "\n", " ")
			text = strings.ReplaceAll(text, "\t", " ")
			m.InputBuffer += text
		}
		return nil
	}

	switch msg.Type {
	case tea.KeyEnter:
		goal := strings.TrimSpace(m.InputBuffer)
		m.InputActive = false
		m.InputBuffer = ""
		if goal != "" {
			return func() tea.Msg { return GoalSubmittedMsg{Goal: goal} }
		}
		return nil
	case tea.KeyEscape:
		m.InputActive = false
		m.InputBuffer = ""
		return nil
	case tea.KeyBackspace:
		if len(m.InputBuffer) > 0 {
			runes := []rune(m.InputBuffer)
			m.InputBuffer = string(runes[:len(runes)-1])
		}
		return nil
	case tea.KeySpace:
		m.InputBuffer += " "
		return nil
	case tea.KeyRunes:
		if isLeakedMouseSequence(msg.Runes) {
			return nil
		}
		text := string(msg.Runes)
		text = strings.ReplaceAll(text, "\r\n", " ")
		text = strings.ReplaceAll(text, "\r", " ")
		text = strings.ReplaceAll(text, "\n", " ")
		text = strings.ReplaceAll(text, "\t", " ")
		slog.Debug("goal input runes", "len", len(msg.Runes), "text_len", len(text))
		m.InputBuffer += text
		return nil
	}
	return nil
}

func (m *AgentPaneModel) handleKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyUp:
		if m.ScrollOffset > 0 {
			m.ScrollOffset--
		}
	case tea.KeyDown:
		if m.ScrollOffset < len(m.Lines)-m.VisibleLines() {
			m.ScrollOffset++
		}
	case tea.KeyCtrlC:
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
	return nil
}

// AppendText adds streaming text to the agent pane.
// Raw text is stored unwrapped; wrapped Lines are derived for the current width.
func (m *AgentPaneModel) AppendText(text string) {
	slog.Debug("agent pane append", "text_len", len(text))
	parts := strings.Split(text, "\n")
	for i, part := range parts {
		if i == 0 && len(m.RawLines) > 0 {
			m.RawLines[len(m.RawLines)-1] += part
		} else {
			m.RawLines = append(m.RawLines, part)
		}
	}
	m.rewrap()
	m.scrollToBottom()
}

// rewrap derives wrapped Lines from RawLines for the current width.
func (m *AgentPaneModel) rewrap() {
	m.Lines = nil
	for _, raw := range m.RawLines {
		if m.Width > 0 && len([]rune(raw)) > m.Width {
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
func (m *AgentPaneModel) wrapLine(line string) []string {
	if m.Width <= 0 {
		return []string{line}
	}
	var result []string
	runes := []rune(line)
	for len(runes) > m.Width {
		// Try to break at a space
		breakAt := m.Width
		for i := m.Width - 1; i > m.Width/2; i-- {
			if runes[i] == ' ' {
				breakAt = i + 1
				break
			}
		}
		result = append(result, string(runes[:breakAt]))
		runes = runes[breakAt:]
	}
	result = append(result, string(runes))
	return result
}

// SetStep updates the current step info.
func (m *AgentPaneModel) SetStep(num, total int, desc string) {
	m.StepNum = num
	m.StepTotal = total
	m.StepDesc = desc
	m.Status = "editing"
}

// Clear clears the agent pane content.
func (m *AgentPaneModel) Clear() {
	m.RawLines = nil
	m.Lines = nil
	m.ScrollOffset = 0
	m.StepNum = 0
	m.StepTotal = 0
	m.StepDesc = ""
	m.Status = "idle"
}

// VisibleLines returns the number of content lines visible.
// Layout: 1 header + content + InputHeight bottom area (separator + input + status).
func (m *AgentPaneModel) VisibleLines() int {
	h := m.Height - 1 - InputHeight // 1 header + InputHeight bottom
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
		ec = len([]rune(m.Lines[el]))
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

// padLine pads or truncates a string to exactly width characters.
func (m *AgentPaneModel) padLine(s string) string {
	runes := []rune(s)
	if len(runes) >= m.Width {
		return string(runes[:m.Width])
	}
	return s + strings.Repeat(" ", m.Width-len(runes))
}

// Render renders the agent pane as exactly m.Height lines joined by \n.
func (m *AgentPaneModel) Render() string {
	if m.Height <= 0 || m.Width <= 0 {
		return ""
	}

	output := make([]string, m.Height)
	row := 0

	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("230")).
		Background(lipgloss.Color("62"))
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	statusStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true)
	selStyle := lipgloss.NewStyle().Background(lipgloss.Color("24"))
	inputActiveStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("230"))
	inputDimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	cursorStyle := lipgloss.NewStyle().Reverse(true)

	// Row 0: Header
	output[row] = headerStyle.Render(m.padLine(" Agent"))
	row++

	// Content rows
	vis := m.VisibleLines()
	for i := range vis {
		if row >= m.Height {
			break
		}
		lineIdx := m.ScrollOffset + i
		if lineIdx < len(m.Lines) {
			runes := []rune(m.Lines[lineIdx])
			for len(runes) < m.Width {
				runes = append(runes, ' ')
			}
			if len(runes) > m.Width {
				runes = runes[:m.Width]
			}

			sl, _, el, _ := m.SelectedRange()
			if m.SelectionActive && lineIdx >= sl && lineIdx <= el {
				var line strings.Builder
				for j, r := range runes[:m.Width] {
					ch := string(r)
					if m.isSelected(lineIdx, j) {
						line.WriteString(selStyle.Render(ch))
					} else {
						line.WriteString(ch)
					}
				}
				output[row] = line.String()
			} else {
				output[row] = string(runes[:m.Width])
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
			output[row] = inputDimStyle.Render(m.padLine(" Ctrl+G to send a goal"))
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
		case "editing":
			statusText = statusStyle.Render(m.padLine(fmt.Sprintf(" Step %d/%d: %s", m.StepNum, m.StepTotal, m.StepDesc)))
		case "waiting":
			statusText = statusStyle.Render(m.padLine(" Ctrl+O approve | Esc reject"))
		default:
			statusText = m.padLine("")
		}
		output[row] = statusText
	}

	return strings.Join(output, "\n")
}
