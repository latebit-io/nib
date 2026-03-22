package ui

import (
	"log/slog"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/latebit-io/junto/engine/agent"
	"github.com/latebit-io/junto/engine/editor"
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

	// Animation tick — advance agent typing by one character
	case animTickMsg:
		return m, m.handleAnimTick()

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
		// ReviewEdit computes the diff AND marks the edit as reviewed.
		// ApproveEdit will fail if this step is skipped — the engine enforces
		// that every frontend shows the developer what the agent proposes.
		diff := m.Session.ReviewEdit()
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
		} else {
			slog.Warn("ReviewEdit returned nil — search text not found or not unique")
			m.AgentPane.AppendMeta("[edit could not be matched — auto-rejecting]\n")
			m.Session.RejectEdit()
		}
	case agent.ErrorEvent:
		m.AgentPane.AppendMeta("\nError: " + e.Err + "\n")
		m.cancelAnimation()
		m.clearEditorOverlay(false)
	case agent.DoneEvent:
		m.AgentPane.Status = "idle"
		m.AgentPane.AppendText("\n--- Done ---\n")
		m.cancelAnimation()
		m.clearEditorOverlay(false)
	}
}

// clearEditorOverlay converts ScrollOffset from visual-line space back to
// buffer-line space and removes the overlay.
//
// bufferMutated should be true when called after a successful ApproveEdit
// (the buffer already has the replacement content). When false (reject,
// error, done), the buffer is unchanged and the conversion differs.
func (m *AppModel) clearEditorOverlay(bufferMutated bool) {
	o := m.Editor.Overlay
	if o == nil {
		return
	}
	addedCount := o.LineCount()
	addedEnd := o.EndLine + addedCount

	if bufferMutated {
		// After approve: removed lines are gone, added lines are now real
		// buffer lines.
		removedCount := o.EndLine - o.StartLine + 1
		if m.Editor.ScrollOffset > addedEnd {
			// Past the overlay: subtract removedCount (virtual removed lines gone).
			m.Editor.ScrollOffset -= removedCount
		} else if m.Editor.ScrollOffset > o.EndLine {
			// In the added-lines zone: map to replacement position.
			m.Editor.ScrollOffset = o.StartLine + (m.Editor.ScrollOffset - o.EndLine - 1)
		} else if m.Editor.ScrollOffset >= o.StartLine {
			// In the removed range: those lines no longer exist.
			// Clamp to StartLine (start of the replacement content).
			m.Editor.ScrollOffset = o.StartLine
		}
	} else {
		// Reject/error/done: buffer unchanged. Subtract addedCount
		// (the virtual overlay lines that are being removed).
		if m.Editor.ScrollOffset > addedEnd {
			m.Editor.ScrollOffset -= addedCount
		} else if m.Editor.ScrollOffset > o.EndLine {
			m.Editor.ScrollOffset = o.EndLine + 1
		}
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
		if m.Session.PendingEdit != nil && m.Editor.Overlay != nil {
			cmd := m.startAnimatedApproval()
			return m, cmd
		}
		return m, nil

	case ActionAgentReject:
		// Cancel running animation (partial edit stays, undoable)
		if m.Editor.Anim != nil && m.Editor.Anim.state == animTyping {
			slog.Debug("animation cancelled", "reason", "escape")
			m.cancelAnimation()
			return m, nil
		}
		if m.Session.PendingEdit != nil {
			slog.Debug("overlay cleared", "reason", "reject")
			m.clearEditorOverlay(false)
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
			m.cancelAnimation()
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

// --- Animated Approval ---

// startAnimatedApproval initiates the animated typing flow.
// Validates via Session.PrepareApproval, clears the overlay, and starts
// the delete+type animation via an engine IncrementalEdit.
func (m *AppModel) startAnimatedApproval() tea.Cmd {
	o := m.Editor.Overlay
	var oldLines []string
	for i := o.StartLine; i <= o.EndLine; i++ {
		oldLines = append(oldLines, m.Editor.Buf.LineText(i))
	}
	search := strings.Join(oldLines, "\n")
	replace := o.Content()

	plan, err := m.Session.PrepareApproval(search, replace)
	if err != nil {
		slog.Warn("agent approve: preparation failed", "err", err)
		m.AgentPane.AppendText("\n[" + err.Error() + "]\n")
		// Only clear the overlay if the session gave up on the edit
		// (PendingEdit cleared, agent rejected). If PendingEdit is still
		// set (e.g. "not reviewed"), keep the overlay so the user can retry.
		if m.Session.PendingEdit == nil {
			m.clearEditorOverlay(false)
		}
		return nil
	}

	slog.Debug("animation starting",
		"line", plan.Line, "col", plan.Col,
		"searchLen", len(plan.Search), "replaceLen", len(plan.Replace))

	// Clear the overlay — we're taking over with direct buffer mutations.
	// Neither clearEditorOverlay(true) nor (false) maps to our state here:
	// the buffer hasn't been mutated yet but is about to be incrementally.
	// Clear with false (no mutation), then reset scroll to the edit location
	// so the user watches the animation from the right place.
	m.clearEditorOverlay(false)
	target := plan.Line - 3
	if target < 0 {
		target = 0
	}
	m.Editor.ScrollOffset = target
	m.Editor.ClampScroll()

	// Engine handles undo group + delete + position tracking.
	searchRunes := len([]rune(plan.Search))
	ie := m.Editor.BeginIncrementalEdit(plan.Line, plan.Col, searchRunes)

	// TUI wraps it with timing and visual state.
	anim := newAnimationContext(ie, plan.Replace, m.Editor.TypingWPM)
	m.Editor.Anim = anim
	m.AgentPane.Status = "typing"

	if anim.done() {
		return m.finishAnimation()
	}

	return scheduleNextTick(anim.charDelay)
}

// handleAnimTick advances the animation by one character.
func (m *AppModel) handleAnimTick() tea.Cmd {
	anim := m.Editor.Anim
	if anim == nil || anim.state != animTyping {
		return nil
	}

	// Collision check: dev cursor is sovereign.
	if m.animCollision() {
		return m.yieldAnimation()
	}

	r := anim.nextChar()
	if r == 0 {
		return m.finishAnimation()
	}

	// Delegate the buffer mutation to the engine.
	anim.edit.InsertChar(r)

	// Auto-scroll to keep agent cursor visible.
	line, _ := anim.position()
	vis := m.Editor.VisibleLines()
	if vis > 0 && line >= m.Editor.ScrollOffset+vis {
		m.Editor.ScrollOffset = line - vis + 1
	}

	if anim.done() {
		return m.finishAnimation()
	}

	return scheduleNextTick(anim.delayForChar(r))
}

// animCollision returns true if the dev cursor is within the span the agent
// is actively typing into.
func (m *AppModel) animCollision() bool {
	anim := m.Editor.Anim
	if anim == nil {
		return false
	}
	startLine, startCol := anim.startPosition()
	endLine, endCol := anim.position()
	return editor.CursorInRegion(
		m.Editor.CursorLine, m.Editor.CursorCol,
		startLine, startCol,
		endLine, endCol,
	)
}

// yieldAnimation pauses the animation because the dev cursor entered the
// agent's region. Finishes typing through the end of the current line
// (clean boundary), then transitions to waiting.
func (m *AppModel) yieldAnimation() tea.Cmd {
	anim := m.Editor.Anim
	if anim == nil {
		return nil
	}

	// Finish the current line via the engine (clean boundary).
	remaining := anim.replaceRunes[anim.typed:]
	consumed := anim.edit.FinishLine(remaining)
	anim.typed += consumed

	anim.edit.Complete()
	anim.state = animWaiting
	anim.yielded = true
	m.AgentPane.Status = "editing"
	m.AgentPane.AppendText("\n[yielded — your line]\n")
	m.Session.CompleteApproval()

	line, col := anim.position()
	slog.Debug("animation yielded", "line", line, "col", col,
		"typed", anim.typed, "total", len(anim.replaceRunes))
	return nil
}

// finishAnimation closes the undo group and signals the agent.
func (m *AppModel) finishAnimation() tea.Cmd {
	anim := m.Editor.Anim
	if anim == nil {
		return nil
	}

	anim.edit.Complete()
	anim.state = animWaiting
	m.AgentPane.Status = "editing"
	m.Session.CompleteApproval()

	line, col := anim.position()
	slog.Debug("animation complete", "line", line, "col", col)
	return nil
}

// cancelAnimation aborts a running animation, closing the undo group
// and signaling the agent to reject (so it can try a different approach).
// The partial edit remains in the buffer (undoable via Ctrl+Z).
func (m *AppModel) cancelAnimation() {
	anim := m.Editor.Anim
	if anim == nil {
		return
	}
	// Signal rejection so the agent isn't left blocked on approveCh.
	// PrepareApproval already cleared PendingEdit, so RejectEdit won't
	// work here — use AbortApproval which sends the reject directly.
	if anim.state == animTyping {
		anim.edit.Abort()
		m.Session.AbortApproval()
	}
	m.Editor.Anim = nil
	m.AgentPane.Status = "idle"
}
