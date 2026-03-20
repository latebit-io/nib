package ui

import (
	"fmt"
	"log/slog"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/latebit-io/junto/tui/internal/agent"
	"github.com/latebit-io/junto/tui/internal/editor/buffer"
)

// Focus indicates which pane has input focus.
type Focus int

const (
	FocusEditor Focus = iota
	FocusAgent
)

// AppModel is the top-level Bubble Tea model.
type AppModel struct {
	Editor      EditorModel
	AgentPane   AgentPaneModel
	AgentLoop   *agent.Agent // nil = editor-only mode
	Clipboard   string
	Focus       Focus
	PendingEdit *agent.PendingEdit // current proposed edit from agent
	Width       int
	Height      int
	Quit        bool

	// Agent pane width ratio (0.0-1.0)
	AgentRatio float64
	Keymap     *Keymap
	program    *tea.Program
}

// SetProgram sets the tea.Program reference for sending agent messages.
func (m *AppModel) SetProgram(p *tea.Program) {
	m.program = p
}

// NewApp creates the application model.
func NewApp(buf *buffer.Buffer, ag *agent.Agent) AppModel {
	return AppModel{
		Editor:     NewEditorModel(buf),
		AgentPane:  NewAgentPaneModel(),
		AgentLoop:  ag,
		AgentRatio: 0.3,
		Keymap:     DefaultKeymap(),
	}
}

func (m *AppModel) Init() tea.Cmd {
	return nil
}

func (m *AppModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.Width = msg.Width
		m.Height = msg.Height
		m.updateLayout()
		return m, nil

	case agent.TokenMsg:
		m.AgentPane.AppendText(msg.Text)
		return m, nil

	case agent.StatusMsg:
		m.AgentPane.Status = msg.Status
		return m, nil

	case agent.EditProposedMsg:
		m.PendingEdit = &msg.Edit
		m.AgentPane.Status = "waiting"
		m.AgentPane.AppendText("\n--- Proposed: " + msg.Edit.Reason + " ---\n")
		return m, nil

	case agent.ErrorMsg:
		m.AgentPane.AppendText("\nError: " + msg.Err + "\n")
		return m, nil

	case agent.DoneMsg:
		m.AgentPane.Status = "idle"
		m.AgentPane.AppendText("\n--- Done ---\n")
		return m, nil

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case tea.KeyMsg:
		slog.Debug("key event", "type", msg.Type, "string", msg.String(), "runes", msg.Runes)

		// Toggle focus with Ctrl+\
		if msg.Type == tea.KeyCtrlBackslash {
			if m.Focus == FocusEditor {
				m.Focus = FocusAgent
			} else {
				m.Focus = FocusEditor
			}
			return m, nil
		}
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *AppModel) updateLayout() {
	agentW := int(float64(m.Width) * m.AgentRatio)
	if agentW < 20 {
		agentW = 20
	}
	editorW := m.Width - agentW - 1 // -1 for divider
	if editorW < 20 {
		editorW = 20
		agentW = m.Width - editorW - 1
	}
	// Clamp for very narrow windows
	if agentW < 0 {
		agentW = 0
	}
	if editorW < 0 {
		editorW = 0
	}

	m.Editor.Width = editorW
	m.Editor.Height = m.Height
	m.AgentPane.Width = agentW
	m.AgentPane.Height = m.Height

	// Clamp scroll offsets for new viewport size
	maxEditorScroll := m.Editor.Buf.LineCount() - m.Editor.VisibleLines()
	if maxEditorScroll < 0 {
		maxEditorScroll = 0
	}
	if m.Editor.ScrollOffset > maxEditorScroll {
		m.Editor.ScrollOffset = maxEditorScroll
	}

	maxAgentScroll := len(m.AgentPane.Lines) - m.AgentPane.VisibleLines()
	if maxAgentScroll < 0 {
		maxAgentScroll = 0
	}
	if m.AgentPane.ScrollOffset > maxAgentScroll {
		m.AgentPane.ScrollOffset = maxAgentScroll
	}
}

func (m *AppModel) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	x := msg.X
	editorW := m.Editor.Width
	dividerX := editorW
	scrollLines := 3

	// Scroll wheel
	if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonWheelUp {
		if x <= dividerX {
			m.Editor.ScrollOffset -= scrollLines
			if m.Editor.ScrollOffset < 0 {
				m.Editor.ScrollOffset = 0
			}
		} else {
			m.AgentPane.ScrollOffset -= scrollLines
			if m.AgentPane.ScrollOffset < 0 {
				m.AgentPane.ScrollOffset = 0
			}
		}
	}

	if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonWheelDown {
		if x <= dividerX {
			maxScroll := m.Editor.Buf.LineCount() - m.Editor.VisibleLines()
			if maxScroll < 0 {
				maxScroll = 0
			}
			m.Editor.ScrollOffset += scrollLines
			if m.Editor.ScrollOffset > maxScroll {
				m.Editor.ScrollOffset = maxScroll
			}
		} else {
			maxScroll := len(m.AgentPane.Lines) - m.AgentPane.VisibleLines()
			if maxScroll < 0 {
				maxScroll = 0
			}
			m.AgentPane.ScrollOffset += scrollLines
			if m.AgentPane.ScrollOffset > maxScroll {
				m.AgentPane.ScrollOffset = maxScroll
			}
		}
	}

	// Click — start selection or set cursor (ignore status bar row)
	if msg.Button == tea.MouseButtonLeft && x < dividerX && msg.Y < m.Editor.VisibleLines() {
		gutterW := m.Editor.GutterWidth()
		col := x - gutterW
		if col < 0 {
			col = 0
		}
		line := m.Editor.ScrollOffset + msg.Y

		switch msg.Action {
		case tea.MouseActionPress:
			m.Focus = FocusEditor
			m.Editor.ClearSelection()
			m.Editor.MoveCursorTo(line, col)
			// Start selection anchor for drag
			m.Editor.SelectionActive = true
			m.Editor.SelectStartLine = m.Editor.CursorLine
			m.Editor.SelectStartCol = m.Editor.CursorCol
		case tea.MouseActionMotion:
			// Drag — extend selection
			if m.Editor.SelectionActive {
				m.Editor.MoveCursorTo(line, col)
			}
		case tea.MouseActionRelease:
			// If cursor didn't move from start, it was a click — clear selection
			if m.Editor.SelectionActive &&
				m.Editor.CursorLine == m.Editor.SelectStartLine &&
				m.Editor.CursorCol == m.Editor.SelectStartCol {
				m.Editor.ClearSelection()
			}
		}
	}

	// Click/drag in agent pane — selection (only in content area, not header/status)
	if msg.Button == tea.MouseButtonLeft && x > dividerX && msg.Y > 0 && msg.Y < m.Height-1 {
		col := x - dividerX - 1 // -1 for divider
		if col < 0 {
			col = 0
		}
		line := m.AgentPane.ScrollOffset + msg.Y - 1 // -1 for header row
		if line < 0 {
			line = 0
		}
		if line >= len(m.AgentPane.Lines) {
			line = max(len(m.AgentPane.Lines)-1, 0)
		}

		switch msg.Action {
		case tea.MouseActionPress:
			m.Focus = FocusAgent
			m.AgentPane.SelectionActive = true
			m.AgentPane.SelectStartLine = line
			m.AgentPane.SelectStartCol = col
			m.AgentPane.CursorLine = line
			m.AgentPane.CursorCol = col
		case tea.MouseActionMotion:
			if m.AgentPane.SelectionActive {
				m.AgentPane.CursorLine = line
				m.AgentPane.CursorCol = col
			}
		case tea.MouseActionRelease:
			if m.AgentPane.SelectionActive &&
				m.AgentPane.CursorLine == m.AgentPane.SelectStartLine &&
				m.AgentPane.CursorCol == m.AgentPane.SelectStartCol {
				m.AgentPane.SelectionActive = false
			}
		}
	}

	return m, nil
}

func (m *AppModel) pasteText(text string) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	if m.Editor.SelectionActive {
		m.Editor.DeleteSelection()
	}
	m.Editor.Buf.Insert(m.Editor.CursorLine, m.Editor.CursorCol, text)
	cl, cc := m.Editor.CursorLine, m.Editor.CursorCol
	for _, r := range text {
		if r == '\n' {
			cl++
			cc = 0
		} else {
			cc++
		}
	}
	m.Editor.MoveCursorTo(cl, cc)
	m.Editor.MarkDirty()
}

func (m *AppModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	action := m.Keymap.Match(msg)

	// Global actions — work from any pane
	switch action {
	case ActionQuit:
		m.Quit = true
		return m, tea.Quit

	case ActionAgentApprove:
		slog.Debug("agent approve", "pending", m.PendingEdit != nil, "agent", m.AgentLoop != nil)
		if m.PendingEdit != nil && m.AgentLoop != nil {
			content := m.Editor.Buf.Content()
			count := strings.Count(content, m.PendingEdit.Search)
			switch count {
			case 1:
				// Convert byte offset to rune-based (line, col) for buffer ops
				idx := strings.Index(content, m.PendingEdit.Search)
				line, col := 0, 0
				for _, r := range content[:idx] {
					if r == '\n' {
						line++
						col = 0
					} else {
						col++
					}
				}
				// Apply via buffer ops so undo history is preserved
				searchRunes := len([]rune(m.PendingEdit.Search))
				m.Editor.Buf.BeginGroup()
				m.Editor.Buf.Delete(line, col, searchRunes)
				m.Editor.Buf.Insert(line, col, m.PendingEdit.Replace)
				m.Editor.Buf.EndGroup()
				m.Editor.ClearSelection()
				m.Editor.MoveCursorTo(line, col)
				m.Editor.MarkDirty()
				m.PendingEdit = nil
				m.AgentLoop.Approve()
			case 0:
				slog.Warn("agent approve: search text not found, auto-rejecting")
				m.AgentPane.AppendText("\n[Edit could not be applied — text not found]\n")
				m.PendingEdit = nil
				m.AgentLoop.Reject()
			default:
				slog.Warn("agent approve: ambiguous match, auto-rejecting", "count", count)
				m.AgentPane.AppendText(fmt.Sprintf("\n[Edit could not be applied — %d matches found, expected 1]\n", count))
				m.PendingEdit = nil
				m.AgentLoop.Reject()
			}
		}
		return m, nil

	case ActionAgentReject:
		if m.PendingEdit != nil && m.AgentLoop != nil {
			m.PendingEdit = nil
			m.AgentLoop.Reject()
			return m, nil
		}

	case ActionAgentContinue:
		if m.AgentLoop != nil && m.AgentPane.Status == "editing" {
			m.AgentLoop.Continue(m.Editor.Buf.Content())
		}
		return m, nil

	case ActionAgentStart:
		if m.AgentLoop != nil {
			m.AgentPane.Clear()
			m.AgentLoop.Run(m.program, m.Editor.Buf.Path, m.Editor.Buf.Content(), "Review this code and suggest improvements")
		}
		return m, nil
	}

	// Route to focused pane
	if m.Focus == FocusAgent {
		return m.handleAgentKey(msg)
	}
	return m.handleEditorKey(msg)
}

func (m *AppModel) handleAgentKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyUp:
		if m.AgentPane.ScrollOffset > 0 {
			m.AgentPane.ScrollOffset--
		}
	case tea.KeyDown:
		if m.AgentPane.ScrollOffset < len(m.AgentPane.Lines)-m.AgentPane.VisibleLines() {
			m.AgentPane.ScrollOffset++
		}
	case tea.KeyCtrlC:
		// Copy selection or all agent output to system clipboard
		if m.AgentPane.SelectionActive {
			_ = clipboardWrite(m.AgentPane.SelectedText())
			m.AgentPane.SelectionActive = false
		} else {
			_ = clipboardWrite(strings.Join(m.AgentPane.Lines, "\n"))
		}
	}
	return m, nil
}

func (m *AppModel) handleEditorKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.Editor.StatusMsg = "" // clear transient status on any key

	isShift := msg.Type == tea.KeyShiftUp || msg.Type == tea.KeyShiftDown ||
		msg.Type == tea.KeyShiftLeft || msg.Type == tea.KeyShiftRight ||
		msg.Type == tea.KeyShiftHome || msg.Type == tea.KeyShiftEnd

	// Match keybinding actions
	action := m.Keymap.Match(msg)

	switch action {
	case ActionQuit:
		m.Quit = true
		return m, tea.Quit

	case ActionSave:
		if err := m.Editor.Buf.Save(); err != nil {
			m.Editor.StatusMsg = "Save failed: " + err.Error()
		} else {
			m.Editor.StatusMsg = "Saved"
		}
		return m, nil

	case ActionUndo:
		l, c, ok := m.Editor.Buf.Undo()
		if ok {
			m.Editor.ClearSelection()
			m.Editor.MoveCursorTo(l, c)
			m.Editor.MarkDirty()
		}
		return m, nil

	case ActionRedo:
		l, c, ok := m.Editor.Buf.Redo()
		if ok {
			m.Editor.ClearSelection()
			m.Editor.MoveCursorTo(l, c)
			m.Editor.MarkDirty()
		}
		return m, nil

	case ActionCopy:
		if m.Editor.SelectionActive {
			m.Clipboard = m.Editor.SelectedText()
			_ = clipboardWrite(m.Clipboard)
		}
		return m, nil

	case ActionCut:
		if m.Editor.SelectionActive {
			m.Clipboard = m.Editor.SelectedText()
			_ = clipboardWrite(m.Clipboard)
			m.Editor.DeleteSelection()
			m.Editor.MarkDirty()
		}
		return m, nil

	case ActionPaste:
		if sys := clipboardRead(); sys != "" {
			m.Clipboard = sys
		}
		if m.Clipboard != "" {
			m.pasteText(m.Clipboard)
		}
		return m, nil

	case ActionSelectAll:
		m.Editor.SelectionActive = true
		m.Editor.SelectStartLine = 0
		m.Editor.SelectStartCol = 0
		lastLine := m.Editor.Buf.LineCount() - 1
		m.Editor.CursorLine = lastLine
		m.Editor.CursorCol = m.Editor.Buf.LineLen(lastLine)
		return m, nil

	case ActionAgentReject:
		// Escape with no pending edit = clear selection
		m.Editor.ClearSelection()
		return m, nil
	}

	// Navigation and editing (not remappable yet)
	switch msg.Type {

	// Selection navigation
	case tea.KeyShiftUp:
		m.Editor.StartSelection()
		m.Editor.MoveCursor(-1, 0)
		return m, nil
	case tea.KeyShiftDown:
		m.Editor.StartSelection()
		m.Editor.MoveCursor(1, 0)
		return m, nil
	case tea.KeyShiftLeft:
		m.Editor.StartSelection()
		m.Editor.MoveCursor(0, -1)
		return m, nil
	case tea.KeyShiftRight:
		m.Editor.StartSelection()
		m.Editor.MoveCursor(0, 1)
		return m, nil
	case tea.KeyShiftHome:
		m.Editor.StartSelection()
		m.Editor.Home()
		return m, nil
	case tea.KeyShiftEnd:
		m.Editor.StartSelection()
		m.Editor.End()
		return m, nil

	// Navigation
	case tea.KeyUp:
		m.Editor.ClearSelection()
		m.Editor.MoveCursor(-1, 0)
		return m, nil
	case tea.KeyDown:
		m.Editor.ClearSelection()
		m.Editor.MoveCursor(1, 0)
		return m, nil
	case tea.KeyLeft:
		m.Editor.ClearSelection()
		m.Editor.MoveCursor(0, -1)
		return m, nil
	case tea.KeyRight:
		m.Editor.ClearSelection()
		m.Editor.MoveCursor(0, 1)
		return m, nil
	case tea.KeyHome:
		m.Editor.ClearSelection()
		m.Editor.Home()
		return m, nil
	case tea.KeyEnd:
		m.Editor.ClearSelection()
		m.Editor.End()
		return m, nil
	case tea.KeyPgUp:
		m.Editor.ClearSelection()
		m.Editor.PageUp()
		return m, nil
	case tea.KeyPgDown:
		m.Editor.ClearSelection()
		m.Editor.PageDown()
		return m, nil

	// Word navigation
	case tea.KeyCtrlRight:
		m.Editor.ClearSelection()
		m.Editor.WordRight()
		return m, nil
	case tea.KeyCtrlLeft:
		m.Editor.ClearSelection()
		m.Editor.WordLeft()
		return m, nil

	// Editing
	case tea.KeyEnter:
		if m.Editor.SelectionActive {
			m.Editor.DeleteSelection()
		}
		m.Editor.InsertNewline()
		return m, nil
	case tea.KeyTab:
		if m.Editor.SelectionActive {
			m.Editor.DeleteSelection()
		}
		m.Editor.InsertTab()
		return m, nil
	case tea.KeyBackspace:
		if m.Editor.SelectionActive {
			m.Editor.DeleteSelection()
			m.Editor.MarkDirty()
		} else {
			m.Editor.Backspace()
		}
		return m, nil
	case tea.KeyDelete:
		if m.Editor.SelectionActive {
			m.Editor.DeleteSelection()
			m.Editor.MarkDirty()
		} else {
			m.Editor.DeleteChar()
		}
		return m, nil

	// Character input (also handles Cmd+V paste on macOS — arrives as multi-char KeyRunes)
	case tea.KeyRunes:
		if len(msg.Runes) > 1 {
			// Multi-char = paste from terminal
			m.pasteText(string(msg.Runes))
		} else {
			if m.Editor.SelectionActive {
				m.Editor.DeleteSelection()
			}
			for _, r := range msg.Runes {
				m.Editor.InsertChar(r)
			}
		}
		return m, nil
	}

	if !isShift {
		m.Editor.ClearSelection()
	}

	return m, nil
}

func (m *AppModel) View() string {
	if m.Width == 0 || m.Height == 0 {
		return "Initializing..."
	}

	editorView := m.Editor.Render()
	agentView := m.AgentPane.Render()

	// Divider: exactly m.Height lines
	dividerStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("240")).
		Background(lipgloss.Color("235"))

	divLines := make([]string, m.Height)
	for i := range m.Height {
		divLines[i] = dividerStyle.Render("│")
	}
	divider := strings.Join(divLines, "\n")

	return lipgloss.JoinHorizontal(lipgloss.Top, editorView, divider, agentView)
}
