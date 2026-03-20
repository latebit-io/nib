package ui

import (
	"log/slog"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/latebit-io/junto/tui/internal/agent"
	"github.com/latebit-io/junto/tui/internal/editor/buffer"
)

// AppModel is the top-level Bubble Tea model.
// It is a thin orchestrator: handles cross-pane actions
// (approve/reject/continue) and delegates everything else
// to the RegionManager and individual panes.
type AppModel struct {
	// Typed references for cross-pane actions
	Editor    *EditorModel
	AgentPane *AgentPaneModel

	// Layout and focus
	Regions *RegionManager

	// Agent loop (nil = editor-only mode)
	AgentLoop *agent.Agent

	// Cross-pane state
	PendingEdit *agent.PendingEdit
	Dialog      DialogModel

	// Shared
	Services *Services
	Keymap   *Keymap
	Quit     bool
	Width    int
	Height   int
	program  *tea.Program
}

// SetProgram sets the tea.Program reference for sending agent messages.
func (m *AppModel) SetProgram(p *tea.Program) {
	m.program = p
}

// NewApp creates the application model.
func NewApp(buf *buffer.Buffer, ag *agent.Agent) AppModel {
	km := DefaultKeymap()
	svc := NewServices()

	editor := NewEditorModel(buf, km, svc)
	agentPane := NewAgentPaneModel(svc)

	rm := NewRegionManager(Horizontal)
	rm.Add("editor", editor, 0.7)
	if ag != nil {
		rm.Add("agent", agentPane, 0.3)
	}

	return AppModel{
		Editor:    editor,
		AgentPane: agentPane,
		Regions:   rm,
		AgentLoop: ag,
		Services:  svc,
		Keymap:    km,
	}
}

func (m *AppModel) Init() tea.Cmd {
	return nil
}

func (m *AppModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Dialog captures all input when active
	if m.Dialog.Active {
		if keyMsg, ok := msg.(tea.KeyMsg); ok {
			cmd := m.Dialog.Update(keyMsg)
			return m, cmd
		}
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.Width = msg.Width
		m.Height = msg.Height
		m.Regions.SetSize(msg.Width, msg.Height)
		return m, nil

	// Agent messages — intercept EditProposedMsg (cross-cutting), delegate rest
	case agent.EditProposedMsg:
		m.PendingEdit = &msg.Edit
		cmd := m.AgentPane.Update(msg)
		return m, cmd

	case agent.TokenMsg, agent.StatusMsg, agent.ErrorMsg, agent.DoneMsg:
		cmd := m.AgentPane.Update(msg)
		return m, cmd

	// Goal submitted from agent pane — wire up the agent run
	case GoalSubmittedMsg:
		if m.AgentLoop != nil {
			m.AgentPane.Clear()
			m.AgentLoop.Run(m.program, m.Editor.Buf.Path, m.Editor.Buf.Content(), msg.Goal)
		}
		return m, nil

	// Dialog result — handle the user's choice
	case DialogResultMsg:
		return m.handleDialogResult(msg)

	case tea.MouseMsg:
		cmd := m.Regions.HandleMouse(msg)
		return m, cmd

	case tea.KeyMsg:
		slog.Debug("key event", "type", msg.Type, "string", msg.String(), "runes", msg.Runes)
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *AppModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Agent pane input mode — all keys go to agent pane
	if m.AgentPane.InputActive {
		cmd := m.AgentPane.Update(msg)
		return m, cmd
	}

	// Toggle focus
	if msg.Type == tea.KeyCtrlBackslash && m.AgentLoop != nil {
		m.Regions.FocusNext()
		return m, nil
	}

	action := m.Keymap.Match(msg)

	// Global / cross-pane actions
	switch action {
	case ActionQuit:
		m.Quit = true
		return m, tea.Quit

	case ActionAgentApprove:
		slog.Debug("agent approve", "pending", m.PendingEdit != nil, "agent", m.AgentLoop != nil)
		if m.PendingEdit != nil && m.AgentLoop != nil {
			ok, reason := m.Editor.ApplyEdit(m.PendingEdit.Search, m.PendingEdit.Replace)
			if ok {
				m.AgentLoop.Approve()
			} else {
				slog.Warn("agent approve: edit rejected", "reason", reason)
				m.AgentPane.AppendText("\n[" + reason + "]\n")
				m.AgentLoop.Reject()
			}
			m.PendingEdit = nil
		}
		return m, nil

	case ActionAgentReject:
		if m.PendingEdit != nil && m.AgentLoop != nil {
			m.PendingEdit = nil
			m.AgentLoop.Reject()
			return m, nil
		}
		// No pending edit — fall through to focused pane

	case ActionAgentContinue:
		if m.AgentLoop != nil && m.AgentPane.Status == "editing" {
			m.AgentLoop.Continue(m.Editor.Buf.Content())
		}
		return m, nil

	case ActionAgentStart:
		if m.AgentLoop != nil {
			m.AgentPane.InputActive = true
			m.AgentPane.InputBuffer = ""
		}
		return m, nil
	}

	// Delegate to focused pane
	pane := m.Regions.FocusedPane()
	if pane != nil {
		cmd := pane.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *AppModel) handleDialogResult(_ DialogResultMsg) (tea.Model, tea.Cmd) {
	// Placeholder — implement specific dialog responses as needed.
	// Example: save-before-quit dialog would check msg.Choice here.
	return m, nil
}

func (m *AppModel) View() string {
	if m.Width == 0 || m.Height == 0 {
		return "Initializing..."
	}

	view := m.Regions.Render()

	// Overlay dialog if active
	if m.Dialog.Active {
		view = m.Dialog.Render(m.Width, m.Height)
	}

	return view
}
