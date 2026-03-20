package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// AgentPaneModel is the Bubble Tea model for the agent reasoning pane.
type AgentPaneModel struct {
	Width  int
	Height int

	// Content lines (streamed from agent, already wrapped)
	Lines []string

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

	// Input mode (for sending messages to agent)
	InputActive bool
	Input       string
}

// NewAgentPaneModel creates a new agent pane.
func NewAgentPaneModel() AgentPaneModel {
	return AgentPaneModel{
		Status: "idle",
	}
}

// AppendText adds streaming text to the agent pane.
func (m *AgentPaneModel) AppendText(text string) {
	parts := strings.Split(text, "\n")
	for i, part := range parts {
		if i == 0 && len(m.Lines) > 0 {
			m.Lines[len(m.Lines)-1] += part
		} else {
			m.Lines = append(m.Lines, part)
		}
	}
	m.scrollToBottom()
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
	m.Lines = nil
	m.ScrollOffset = 0
	m.StepNum = 0
	m.StepTotal = 0
	m.StepDesc = ""
	m.Status = "idle"
}

// VisibleLines returns the number of content lines visible (total height - header - status).
func (m *AgentPaneModel) VisibleLines() int {
	h := m.Height - 2 // 1 header + 1 status
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

	// Row 0: Header
	output[row] = headerStyle.Render(m.padLine(" Agent"))
	row++

	// Content rows
	vis := m.VisibleLines()
	selStyle := lipgloss.NewStyle().Background(lipgloss.Color("24"))

	for i := range vis {
		if row >= m.Height {
			break
		}
		lineIdx := m.ScrollOffset + i
		if lineIdx < len(m.Lines) {
			runes := []rune(m.Lines[lineIdx])
			// Pad to width
			for len(runes) < m.Width {
				runes = append(runes, ' ')
			}
			if len(runes) > m.Width {
				runes = runes[:m.Width]
			}

			// Render char by char if selection active on this line
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

	// Status line (last row)
	for row < m.Height-1 {
		output[row] = strings.Repeat(" ", m.Width)
		row++
	}

	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	statusStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true)
	var statusText string
	switch m.Status {
	case "idle":
		statusText = dimStyle.Render(m.padLine(" Ready"))
	case "thinking":
		statusText = statusStyle.Render(m.padLine(" Thinking..."))
	case "editing":
		statusText = statusStyle.Render(m.padLine(fmt.Sprintf(" Step %d/%d: %s", m.StepNum, m.StepTotal, m.StepDesc)))
	case "waiting":
		statusText = statusStyle.Render(m.padLine(" Waiting for approval..."))
	default:
		statusText = m.padLine("")
	}
	if row < m.Height {
		output[row] = statusText
	}

	return strings.Join(output, "\n")
}
