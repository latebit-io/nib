package ui

import (
	"log/slog"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/latebit-io/junto/engine/agent"
	"github.com/latebit-io/junto/engine/session"
)

// agentEventMsg wraps an engine agent.Event for delivery through Bubble Tea.
type agentEventMsg struct{ event agent.Event }

// AppModel is the top-level Bubble Tea model.
// It is a thin presentation layer: maps input to engine Session methods,
// reads Session state to render, and adapts agent events to tea.Msg.
type AppModel struct {
	// Engine session — owns all domain logic
	Session *session.Session

	// Typed references for rendering
	Editor    *EditorModel
	AgentPane *AgentPaneModel

	// Layout and focus
	Regions *RegionManager

	// TUI-only state
	Dialog      DialogModel
	recentMouse bool // tracks leaked CSI prefix from unparsed mouse events
	Services    *Services
	Keymap      *Keymap
	Quit        bool
	Width       int
	Height      int
	program     *tea.Program
}

// SetProgram sets the tea.Program reference.
func (m *AppModel) SetProgram(p *tea.Program) {
	m.program = p
}

// NewApp creates the application model.
func NewApp(sess *session.Session) AppModel {
	km := DefaultKeymap()
	svc := NewServices()

	editorPane := NewEditorModel(sess.Editor, km, svc)
	agentPane := NewAgentPaneModel(svc)
	agentPane.HasAgent = sess.HasAgent()

	rm := NewRegionManager(Horizontal)
	rm.Add("editor", editorPane, 0.7)
	rm.Add("agent", agentPane, 0.3)

	return AppModel{
		Session:   sess,
		Editor:    editorPane,
		AgentPane: agentPane,
		Regions:   rm,
		Services:  svc,
		Keymap:    km,
	}
}

func (m *AppModel) Init() tea.Cmd {
	if m.Session.Events != nil {
		return m.listenForAgentEvent()
	}
	return nil
}

// listenForAgentEvent returns a tea.Cmd that blocks on the engine event channel
// and delivers the next event as a tea.Msg.
func (m *AppModel) listenForAgentEvent() tea.Cmd {
	ch := m.Session.Events
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return agentEventMsg{event: ev}
	}
}

func (m *AppModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Dialog is modal — captures all input when active
	if m.Dialog.Active {
		switch typed := msg.(type) {
		case tea.KeyMsg:
			cmd := m.Dialog.Update(typed)
			return m, cmd
		case tea.MouseMsg:
			return m, nil // swallow mouse while dialog is visible
		}
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.Width = msg.Width
		m.Height = msg.Height
		// Intent bar always takes 1 row; RegionManager gets the rest
		m.Regions.SetSize(msg.Width, m.regionHeight())
		return m, nil

	// Engine agent events — adapted from channel to tea.Msg
	case agentEventMsg:
		m.handleAgentEvent(msg.event)
		// Keep listening for the next event
		return m, m.listenForAgentEvent()

	// Goal submitted from agent pane — delegate to session
	case GoalSubmittedMsg:
		m.AgentPane.Clear()
		m.Session.SubmitGoal(msg.Goal)
		return m, nil

	// Dialog result — handle the user's choice
	case DialogResultMsg:
		return m.handleDialogResult(msg)

	case tea.MouseMsg:
		// Only set recentMouse for scroll events — those are the ones that
		// produce leaked CSI sequences during rapid scrolling.
		if msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown {
			m.recentMouse = true
		}
		// Translate Y for intent bar row
		msg.Y -= 1
		cmd := m.Regions.HandleMouse(msg)
		return m, cmd

	case tea.KeyMsg:
		slog.Debug("key event", "type", msg.Type, "string", msg.String(), "runes", msg.Runes)
		return m.handleKey(msg)
	}
	return m, nil
}

// handleAgentEvent updates session state and renders the event in the agent pane.
func (m *AppModel) handleAgentEvent(ev agent.Event) {
	// Let session update domain state (intent, pending edit)
	m.Session.HandleEvent(ev)

	// Render in agent pane (presentation)
	switch e := ev.(type) {
	case agent.TokenEvent:
		m.AgentPane.AppendToken(e.Text)
	case agent.StatusEvent:
		m.AgentPane.Status = e.Status
	case agent.EditProposedEvent:
		m.AgentPane.Status = "waiting"
		m.AgentPane.AppendMeta("\n--- Proposed: " + e.Edit.Reason + " ---\n")
		// Compute inline diff and create editable overlay.
		diff := m.Editor.ComputeDiff(e.Edit.Search, e.Edit.Replace)
		if diff != nil {
			slog.Debug("overlay created", "startLine", diff.StartLine, "endLine", diff.EndLine, "newLines", len(diff.NewLines))
			m.Editor.Overlay = NewDiffOverlay(diff)
			m.Editor.Overlay.Active = true
			// Auto-scroll so the diff is visible with some context above.
			target := diff.StartLine - 3
			if target < 0 {
				target = 0
			}
			m.Editor.ScrollOffset = target
			m.Editor.syncExtraVisualLines()
			m.Editor.ClampScroll()
		}
	case agent.ErrorEvent:
		m.AgentPane.AppendMeta("\nError: " + e.Err + "\n")
		m.clearEditorOverlay()
	case agent.DoneEvent:
		m.AgentPane.Status = "idle"
		m.AgentPane.AppendText("\n--- Done ---\n")
		m.clearEditorOverlay()
	}
}

// clearEditorOverlay converts ScrollOffset from visual-line space back to
// buffer-line space and removes the overlay.
func (m *AppModel) clearEditorOverlay() {
	o := m.Editor.Overlay
	if o == nil {
		return
	}
	addedCount := o.LineCount()
	if m.Editor.ScrollOffset > o.EndLine+addedCount {
		m.Editor.ScrollOffset -= addedCount
	} else if m.Editor.ScrollOffset > o.EndLine {
		m.Editor.ScrollOffset = o.EndLine + 1
	}
	m.Editor.Overlay = nil
	m.Editor.ExtraVisualLines = 0
}

// regionHeight returns the height available for the RegionManager
// (total height minus the intent bar which is always present).
func (m *AppModel) regionHeight() int {
	h := m.Height - 1 // 1 row for intent bar
	if h < 1 {
		h = 1
	}
	return h
}

func (m *AppModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Drop leaked mouse escape sequence fragments.
	// During rapid scrolling, Bubble Tea's parser can fail to consume full SGR
	// sequences. The fragments leak as KeyRunes — either a lone '[' (CSI prefix)
	// or a full SGR body like '<65;14;32M'. Gate behind recentMouse so we never
	// silently drop legitimate typed/pasted text.
	if m.recentMouse && msg.Type == tea.KeyRunes {
		if (len(msg.Runes) == 1 && msg.Runes[0] == '[') || isLeakedMouseSequence(msg.Runes) {
			// Keep recentMouse=true so consecutive leaked sequences from
			// rapid scrolling are all caught, not just the first one.
			return m, nil
		}
	}
	m.recentMouse = false

	// Agent pane input mode — all keys go to agent pane
	if m.AgentPane.InputActive {
		cmd := m.AgentPane.Update(msg)
		return m, cmd
	}

	// Toggle focus
	if msg.Type == tea.KeyCtrlBackslash && m.Session.HasAgent() {
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
		slog.Debug("agent approve", "pending", m.Session.PendingEdit != nil, "agent", m.Session.HasAgent())
		if m.Session.PendingEdit != nil {
			// If the user edited the overlay, update the search/replace to use
			// full affected lines so prefix/suffix edits are captured.
			if m.Editor.Overlay != nil {
				o := m.Editor.Overlay
				var oldLines []string
				for i := o.StartLine; i <= o.EndLine; i++ {
					oldLines = append(oldLines, m.Editor.Buf.LineText(i))
				}
				m.Session.PendingEdit.Search = strings.Join(oldLines, "\n")
				m.Session.PendingEdit.Replace = o.Content()
			}
			slog.Debug("overlay cleared", "reason", "approve",
				"searchLen", len(m.Session.PendingEdit.Search),
				"replaceLen", len(m.Session.PendingEdit.Replace))
			m.clearEditorOverlay()
			ok, reason := m.Session.ApproveEdit()
			if !ok {
				slog.Warn("agent approve: edit rejected", "reason", reason)
				m.AgentPane.AppendText("\n[" + reason + "]\n")
			}
		}
		return m, nil

	case ActionAgentReject:
		if m.Session.PendingEdit != nil {
			slog.Debug("overlay cleared", "reason", "reject")
			m.clearEditorOverlay()
			m.Session.RejectEdit()
			return m, nil
		}
		// No pending edit — cancel agent if active
		if m.Session.CurrentIntent != "" && m.Session.HasAgent() {
			m.Session.CancelAgent()
			m.AgentPane.Status = "idle"
			return m, nil
		}
		// No intent either — fall through to focused pane

	case ActionAgentContinue:
		if m.Session.HasAgent() && m.AgentPane.Status == "editing" {
			m.Session.Continue()
		}
		return m, nil

	case ActionAgentStart:
		if m.Session.HasAgent() {
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
	return m, nil
}

func (m *AppModel) View() string {
	if m.Width == 0 || m.Height == 0 {
		return "Initializing..."
	}

	// Replace view with dialog when active
	if m.Dialog.Active {
		return m.Dialog.Render(m.Width, m.Height)
	}

	return m.renderIntentBar() + "\n" + m.Regions.Render()
}

func (m *AppModel) renderIntentBar() string {
	var text string
	var style lipgloss.Style

	idleStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("240")).
		Background(lipgloss.Color("236"))
	activeStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("230")).
		Background(lipgloss.Color("235"))
	doneStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("2")).
		Background(lipgloss.Color("236"))

	switch {
	case !m.Session.HasAgent():
		text = " Editor"
		style = idleStyle
	case m.Session.CurrentIntent == "":
		text = " Ctrl+G to set intent"
		style = idleStyle
	case m.Session.IntentDone:
		text = " done: " + m.Session.CurrentIntent
		style = doneStyle
	default:
		text = " " + m.Session.CurrentIntent
		style = activeStyle
	}

	// Truncate to fit width (one line, never wraps)
	runes := []rune(text)
	if len(runes) > m.Width {
		runes = runes[:m.Width-1]
		text = string(runes) + "…"
	}

	// Pad to full width
	padding := m.Width - len([]rune(text))
	if padding > 0 {
		text += strings.Repeat(" ", padding)
	}

	return style.Render(text)
}
