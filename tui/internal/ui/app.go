package ui

import (
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/latebit-io/junto/engine/editor"
	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/filelist"
	"github.com/latebit-io/junto/engine/session"
)

// engineEventMsg wraps an engine event.Event for delivery through Bubble Tea.
type engineEventMsg struct{ event event.Event }

// paletteFilesMsg delivers file listing results from async Walk.
type paletteFilesMsg struct{ items []PaletteItem }

// paletteErrorMsg delivers a file listing error to the UI.
type paletteErrorMsg struct{ err string }

// hoverResultMsg delivers hover information from an async LSP request.
type hoverResultMsg struct{ text string }

// AppModel is the top-level Bubble Tea model.
// It is a thin presentation layer: maps input to engine Session methods,
// reads Session state to render, and adapts agent events to tea.Msg.
type AppModel struct {
	// Engine session — owns all domain logic
	Session *session.Session

	// Typed references for rendering
	Editor      *EditorModel
	AgentPane   *AgentPaneModel
	ProjectPane *ProjectPaneModel

	// Layout and focus
	Regions *RegionManager

	// TUI-only state
	Dialog      DialogModel
	Palette     PaletteModel
	ProjectRoot string
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
	editorPane.OnSave = func() { sess.NotifySaved() }
	agentPane := NewAgentPaneModel(svc)
	agentPane.HasAgent = sess.HasAgent()
	projectPane := NewProjectPaneModel(sess)

	rm := NewRegionManager(Horizontal)
	rm.Add("project", projectPane, 0.2)
	rm.Add("editor", editorPane, 0.5)
	rm.Add("agent", agentPane, 0.3)
	rm.FocusByName("editor")

	return AppModel{
		Session:     sess,
		Editor:      editorPane,
		AgentPane:   agentPane,
		ProjectPane: projectPane,
		Regions:     rm,
		Services:    svc,
		Keymap:      km,
	}
}

func (m *AppModel) Init() tea.Cmd {
	if m.Session.Events != nil {
		return m.listenForEvents()
	}
	return nil
}

// listenForEvents returns a tea.Cmd that blocks on the engine event channel
// and delivers the next event as a tea.Msg.
func (m *AppModel) listenForEvents() tea.Cmd {
	ch := m.Session.Events
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return engineEventMsg{event: ev}
	}
}

func (m *AppModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Palette is modal — captures all input when active
	if m.Palette.Active {
		switch typed := msg.(type) {
		case tea.KeyMsg:
			cmd := m.Palette.Update(typed)
			return m, cmd
		case tea.MouseMsg:
			return m, nil
		}
	}

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

	// Engine events — adapted from channel to tea.Msg
	case engineEventMsg:
		m.handleEngineEvent(msg.event)
		// Keep listening for the next event
		return m, m.listenForEvents()

	// Goal submitted from agent pane — delegate to session
	case GoalSubmittedMsg:
		m.AgentPane.Clear()
		m.Session.SubmitGoal(msg.Goal)
		return m, nil

	// Dialog result — handle the user's choice
	case DialogResultMsg:
		return m.handleDialogResult(msg)

	// File listing error — surface in agent pane
	case paletteErrorMsg:
		slog.Error("failed to list files", "err", msg.err)
		m.AgentPane.AppendMeta("\n[file listing failed: " + msg.err + "]\n")
		return m, nil

	// File listing completed — open the palette with results
	case paletteFilesMsg:
		m.Palette.Open(msg.items)
		return m, nil

	// Palette result — user selected a file or cancelled
	case PaletteResultMsg:
		if !msg.Cancelled && msg.Category == "file" {
			return m.openFile(msg.Item.Value)
		}
		return m, nil

	// Hover result — display or dismiss
	case hoverResultMsg:
		if msg.text != "" {
			m.Editor.ShowHover(msg.text)
		}
		return m, nil

	// Project pane — user selected a file to open
	case ProjectOpenFileMsg:
		return m.openFile(msg.Path)

	// Project pane — context set mutations (all session writes go through AppModel)
	case ProjectAddContextMsg:
		m.Session.AddContext(msg.Path)
		m.refreshProjectPane()
		return m, nil

	case ProjectRemoveContextMsg:
		m.Session.RemoveContext(msg.Path)
		m.refreshProjectPane()
		return m, nil

	// Animation tick — advance the agent typing animation
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

// handleEngineEvent updates session state and renders the event.
func (m *AppModel) handleEngineEvent(ev event.Event) {
	// Let session update domain state (intent, pending edit)
	m.Session.HandleEvent(ev)

	// Render in agent pane (presentation)
	switch e := ev.(type) {
	case event.AgentToken:
		m.AgentPane.AppendToken(e.Text)
	case event.AgentStatus:
		m.AgentPane.Status = e.Status
	case event.AgentEditProposed:
		// Clean up any running animation before creating a new overlay.
		m.cancelAnimation()
		m.clearEditorOverlay(false)

		m.AgentPane.Status = "waiting"
		m.AgentPane.AppendMeta("\n--- Proposed: " + e.Edit.Reason + " ---\n")
		// ReviewEdit computes the diff AND marks the edit as reviewed.
		// If the edit targets a different file, the session auto-switches
		// and we rebuild the EditorModel to render the correct buffer.
		diff, switched := m.Session.ReviewEdit()
		if switched {
			wpm := m.Editor.TypingWPM
			m.Editor = NewEditorModel(m.Session.Editor, m.Keymap, m.Services)
			m.Editor.TypingWPM = wpm
			m.Editor.OnSave = func() { m.Session.NotifySaved() }
			m.Regions.ReplacePane("editor", m.Editor)
			m.refreshDiagnostics(m.Session.ActiveFile())
		}
		if diff != nil {
			slog.Debug("overlay created", "startLine", diff.StartLine, "endLine", diff.EndLine, "newLines", len(diff.NewLines))
			m.Editor.Overlay = NewDiffOverlay(diff)
			m.Editor.Overlay.Active = true
			// Auto-scroll so the diff is visible with some context above.
			target := diff.StartLine - 3
			if target < 0 {
				target = 0
			}
			m.Editor.eng.ScrollOffset = target
			m.Editor.syncExtraVisualLines()
			m.Editor.eng.ClampScroll()
		} else {
			slog.Warn("ReviewEdit returned nil — search text not found or not unique")
			m.AgentPane.AppendMeta("[edit could not be matched — auto-rejecting]\n")
			m.Session.RejectEdit()
		}
	case event.AgentFileCreated:
		m.AgentPane.AppendMeta("\n[Created: " + e.Path + "]\n")
		m.refreshProjectPane()
	case event.AgentError:
		m.AgentPane.AppendMeta("\nError: " + e.Err + "\n")
		m.cancelAnimation()
		m.clearEditorOverlay(false)
	case event.AgentDone:
		m.AgentPane.Status = "idle"
		m.AgentPane.AppendText("\n--- Done ---\n")
		m.cancelAnimation()
		m.clearEditorOverlay(false)
		m.refreshProjectPane()
	case event.DiagnosticsUpdated:
		m.refreshDiagnostics(e.Path)
	}
}

// clearEditorOverlay delegates scroll correction to the engine and clears
// the TUI overlay state.
//
// bufferMutated should be true when called after a successful ApproveEdit
// (the buffer already has the replacement content). When false (reject,
// error, done), the buffer is unchanged and the conversion differs.
func (m *AppModel) clearEditorOverlay(bufferMutated bool) {
	o := m.Editor.Overlay
	if o == nil {
		return
	}
	m.Editor.eng.CollapseOverlay(o.StartLine, o.EndLine, o.LineCount(), bufferMutated)
	m.Editor.Overlay = nil
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

	// Toggle focus between visible panes
	if msg.Type == tea.KeyCtrlBackslash {
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
		// Block approve during active animation.
		if m.Editor.Anim != nil && m.Editor.Anim.state == animTyping {
			return m, nil
		}
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
		// Block continue during active animation — wait for it to finish.
		if m.Editor.Anim != nil && m.Editor.Anim.state == animTyping {
			return m, nil
		}
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

	case ActionToggleProject:
		return m.handleToggleProject()

	case ActionOpenPalette:
		if m.ProjectRoot != "" {
			root := m.ProjectRoot
			return m, func() tea.Msg {
				files, err := filelist.Walk(root)
				if err != nil && !errors.Is(err, filelist.ErrCapped) {
					return paletteErrorMsg{err: err.Error()}
				}
				items := make([]PaletteItem, len(files))
				for i, f := range files {
					items[i] = PaletteItem{
						Label:    f,
						Category: "file",
						Value:    filepath.Join(root, f),
					}
				}
				return paletteFilesMsg{items: items}
			}
		}
		return m, nil

	case ActionGoToDefinition:
		return m.handleGoToDefinition()

	case ActionGoBack:
		return m.handleGoBack()

	case ActionHover:
		return m.handleHover()
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

	// Replace view with dialog when active.
	if m.Dialog.Active {
		return m.Dialog.Render(m.Width, m.Height)
	}

	base := m.renderIntentBar() + "\n" + m.Regions.Render()

	// Palette floats on top of the editor — editor stays visible.
	if m.Palette.Active {
		return m.Palette.RenderOverlay(base, m.Width, m.Height)
	}

	return base
}

// openFile switches the editor to a different file. Uses session.SwitchTo
// which keeps editors alive in the multi-buffer map. This method rebuilds
// the EditorModel and updates the region manager.
func (m *AppModel) openFile(path string) (tea.Model, tea.Cmd) {
	if err := m.Session.SwitchTo(path); err != nil {
		if errors.Is(err, session.ErrEditPending) {
			m.AgentPane.AppendMeta("[" + err.Error() + "]\n")
		} else {
			slog.Error("failed to open file", "path", path, "err", err)
			m.AgentPane.AppendMeta("[error: " + err.Error() + "]\n")
		}
		return m, nil
	}

	// Cancel any running animation.
	m.cancelAnimation()

	// Rebuild EditorModel with the new active editor from session.
	wpm := m.Editor.TypingWPM
	m.Editor = NewEditorModel(m.Session.Editor, m.Keymap, m.Services)
	m.Editor.TypingWPM = wpm
	m.Editor.OnSave = func() { m.Session.NotifySaved() }

	// Update region manager's pane reference and apply size.
	m.Regions.ReplacePane("editor", m.Editor)

	slog.Debug("file opened", "path", path)
	m.refreshDiagnostics(m.Session.ActiveFile())
	m.refreshProjectPane()
	return m, nil
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
	padding := m.Width - utf8.RuneCountInString(text)
	if padding > 0 {
		text += strings.Repeat(" ", padding)
	}

	return style.Render(text)
}

// refreshDiagnostics queries the session for diagnostics on the given path
// and updates the editor model. Only updates if path matches the active file.
func (m *AppModel) refreshDiagnostics(path string) {
	if !m.Session.HasLanguageService() {
		return
	}
	canon := m.Session.CanonPath(path)
	if m.Session.ActiveFile() != canon {
		return
	}
	m.Editor.SetDiagnostics(m.Session.Diagnostics(canon))
}

// --- Go-to-Definition / Hover / Go-Back ---

// handleGoToDefinition navigates to the definition of the symbol under the cursor.
func (m *AppModel) handleGoToDefinition() (tea.Model, tea.Cmd) {
	if !m.Session.HasLanguageService() {
		return m, nil
	}
	loc, err := m.Session.GoToDefinition(m.Editor.eng.CursorLine, m.Editor.eng.CursorCol)
	if err != nil {
		slog.Debug("go-to-definition failed", "err", err)
		m.Editor.StatusMsg = err.Error()
		return m, nil
	}

	// Session may have switched files — rebuild EditorModel if needed.
	if m.Session.Editor != m.Editor.eng {
		wpm := m.Editor.TypingWPM
		m.Editor = NewEditorModel(m.Session.Editor, m.Keymap, m.Services)
		m.Editor.TypingWPM = wpm
		m.Editor.OnSave = func() { m.Session.NotifySaved() }
		m.Regions.ReplacePane("editor", m.Editor)
		m.refreshDiagnostics(m.Session.ActiveFile())
	}

	slog.Debug("go-to-definition", "path", loc.Path, "line", loc.Line, "col", loc.Col)
	return m, nil
}

// handleGoBack returns to the previous location in the navigation stack.
func (m *AppModel) handleGoBack() (tea.Model, tea.Cmd) {
	loc := m.Session.GoBack()
	if loc == nil {
		return m, nil
	}

	// Session may have switched files — rebuild EditorModel if needed.
	if m.Session.Editor != m.Editor.eng {
		wpm := m.Editor.TypingWPM
		m.Editor = NewEditorModel(m.Session.Editor, m.Keymap, m.Services)
		m.Editor.TypingWPM = wpm
		m.Editor.OnSave = func() { m.Session.NotifySaved() }
		m.Regions.ReplacePane("editor", m.Editor)
		m.refreshDiagnostics(m.Session.ActiveFile())
	}

	slog.Debug("go-back", "path", loc.Path, "line", loc.Line, "col", loc.Col)
	return m, nil
}

// handleHover requests hover info for the symbol under the cursor.
// The LSP request runs asynchronously via a tea.Cmd.
func (m *AppModel) handleHover() (tea.Model, tea.Cmd) {
	if !m.Session.HasLanguageService() {
		return m, nil
	}
	// Dismiss any existing hover.
	m.Editor.DismissHover()

	line, col := m.Editor.eng.CursorLine, m.Editor.eng.CursorCol
	return m, func() tea.Msg {
		text, err := m.Session.HoverInfo(line, col)
		if err != nil {
			slog.Debug("hover failed", "err", err)
			return hoverResultMsg{}
		}
		return hoverResultMsg{text: text}
	}
}

// refreshProjectPane rebuilds the project pane if visible, or marks it
// dirty for deferred rebuild when the pane is next shown.
func (m *AppModel) refreshProjectPane() {
	if m.ProjectPane == nil {
		return
	}
	r := m.Regions.regionByName("project")
	if r != nil && r.Visible {
		m.ProjectPane.rebuild()
	} else {
		m.ProjectPane.dirty = true
	}
}

// handleToggleProject implements the Ctrl+B toggle:
// hidden → show + focus, visible but not focused → focus, focused → hide.
func (m *AppModel) handleToggleProject() (tea.Model, tea.Cmd) {
	r := m.Regions.regionByName("project")
	if r == nil {
		return m, nil
	}
	focused := m.Regions.FocusedRegion()
	if !r.Visible {
		// Hidden → show + focus (rebuild if stale)
		if m.ProjectPane.dirty {
			m.ProjectPane.rebuild()
			m.ProjectPane.dirty = false
		}
		m.Regions.Show("project")
		m.Regions.FocusByName("project")
	} else if focused == nil || focused.Name != "project" {
		// Visible but not focused → focus
		m.Regions.FocusByName("project")
	} else {
		// Focused → hide, move focus to editor
		m.Regions.Hide("project")
		m.Regions.FocusByName("editor")
	}
	return m, nil
}

// --- Animated Approval ---

// startAnimatedApproval initiates the animated typing flow.
// Validates via Session.PrepareApproval, clears the overlay, and starts
// the delete+type animation via an engine IncrementalEdit.
func (m *AppModel) startAnimatedApproval() tea.Cmd {
	o := m.Editor.Overlay
	var oldLines []string
	for i := o.StartLine; i <= o.EndLine; i++ {
		oldLines = append(oldLines, m.Editor.eng.Buf.LineText(i))
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
	scrollTo := plan.Line
	if plan.IsSurgical() {
		scrollTo = plan.Narrowed.Line
	}
	target := scrollTo - 3
	if target < 0 {
		target = 0
	}
	m.Editor.eng.ScrollOffset = target
	m.Editor.eng.ClampScroll()

	// Engine handles undo group, deletion, position tracking, and per-tick
	// advancement. TUI only owns the tick schedule and visual state.
	cpt := charsPerTick(m.Editor.TypingWPM)
	devStartLine, devStartCol := m.Editor.eng.CursorLine, m.Editor.eng.CursorCol

	// Surgical path: narrow the edit to only the changed lines.
	// Unchanged prefix and suffix lines stay in the buffer — they never
	// disappear, making the animation look like a real pair programmer.
	var edit editor.AnimatedEdit
	if plan.IsSurgical() {
		ne := plan.Narrowed
		searchRunes := utf8.RuneCountInString(ne.Search)
		edit = m.Editor.eng.BeginIncrementalEdit(ne.Line, ne.Col, searchRunes, cpt, ne.Replace, ne.LineOrigins)
		slog.Debug("surgical animation",
			"prefix", ne.PrefixLines, "suffix", ne.SuffixLines,
			"narrowSearch", len(ne.Search), "narrowReplace", len(ne.Replace))
	} else {
		searchRunes := utf8.RuneCountInString(plan.Search)
		edit = m.Editor.eng.BeginIncrementalEdit(plan.Line, plan.Col, searchRunes, cpt, plan.Replace, plan.LineOrigins)
	}

	m.Editor.Anim = &animationContext{
		state:        animTyping,
		edit:         edit,
		devStartLine: devStartLine,
		devStartCol:  devStartCol,
	}
	m.AgentPane.Status = "typing"

	if edit.Remaining() == 0 {
		return m.finishAnimation()
	}

	return scheduleNextTick()
}

// handleAnimTick advances the animation by one frame. The engine's
// IncrementalEdit.Advance handles char insertion and pacing.
func (m *AppModel) handleAnimTick() tea.Cmd {
	anim := m.Editor.Anim
	if anim == nil || anim.state != animTyping {
		return nil
	}

	// Collision check: dev cursor is sovereign.
	if m.animCollision() {
		return m.yieldAnimation()
	}

	// Engine advances the edit by charsPerTick characters.
	result := anim.edit.Advance()

	// Scroll down to follow the agent cursor as it advances. Only scrolls
	// downward — if the user scrolled past the agent, we don't pull them back.
	line, _ := anim.edit.Position()
	vis := m.Editor.eng.VisibleLines()
	if vis > 0 && line >= m.Editor.eng.ScrollOffset+vis {
		m.Editor.eng.ScrollOffset = line - vis + 1
	}

	if result.Done {
		return m.finishAnimation()
	}

	return scheduleNextTick()
}

// animCollision returns true if the dev cursor is within the span the agent
// is actively typing into. Only triggers after the dev has moved their cursor
// at least once since animation started — a stationary cursor is passive.
// The moved flag is sticky: once the dev moves, collision detection stays
// active even if they return to the original position.
func (m *AppModel) animCollision() bool {
	anim := m.Editor.Anim
	if anim == nil {
		return false
	}
	// Detect first move — sticky once set.
	if !anim.devMoved {
		if m.Editor.eng.CursorLine != anim.devStartLine || m.Editor.eng.CursorCol != anim.devStartCol {
			anim.devMoved = true
		} else {
			return false
		}
	}
	startLine, startCol := anim.edit.StartPosition()
	endLine, endCol := anim.edit.Position()
	return editor.CursorInRegion(
		m.Editor.eng.CursorLine, m.Editor.eng.CursorCol,
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
	anim.edit.FinishLine()
	anim.edit.Complete()
	anim.state = animWaiting
	anim.yielded = true
	m.AgentPane.Status = "editing"
	m.AgentPane.AppendText("\n[yielded — your line]\n")
	m.Session.CompleteApproval()

	line, col := anim.edit.Position()
	remaining := anim.edit.Remaining()
	slog.Debug("animation yielded", "line", line, "col", col, "remaining", remaining)
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
	m.refreshProjectPane()

	line, col := anim.edit.Position()
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
		m.AgentPane.Status = "idle"
	}
	m.Editor.Anim = nil
}
