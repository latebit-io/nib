package ui

import (
	"log/slog"

	tea "charm.land/bubbletea/v2"
	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/coding/session"
	"github.com/latebit-io/nib/engine/buffer"
	"github.com/latebit-io/nib/engine/search"
	"github.com/latebit-io/nib/engine/syntax"
	"github.com/latebit-io/nib/tui/editor"
)

// engineEventMsg wraps an engine event.Event for delivery through Bubble Tea.
type engineEventMsg struct{ event event.Event }
type initDoneMsg struct{}

// paletteFilesMsg delivers file listing results from async Walk.
type paletteFilesMsg struct{ items []PaletteItem }

// paletteErrorMsg delivers a file listing error to the UI.
type paletteErrorMsg struct{ err string }

// oauthInstructionMsg delivers an intermediate instruction from an OAuth flow
// (e.g., "Visit URL and enter code: XXXX") before the flow completes.
type oauthInstructionMsg struct {
	profile     string
	instruction string
}

// oauthConnectResultMsg delivers the result of an OAuth connection flow.
type oauthConnectResultMsg struct {
	profile string
	err     error
}

// apiKeyEnteredMsg delivers a user-entered API key for a profile.
type apiKeyEnteredMsg struct {
	profile string
	key     string
}

// setAgentCallbacksMsg installs generic agent callbacks on the
// AppModel from inside the Update goroutine. Constructed via
// [SetAgentCallbacksMsg]; sent through Program.Send so the field
// writes happen on the single-threaded Bubble Tea event loop —
// otherwise post-Run callers would race the loop's reads.
type setAgentCallbacksMsg struct {
	toggleTerse  func(enabled bool) bool
	initialTerse bool
}

// setCodingCallbacksMsg installs coding-flavored agent callbacks on
// the AppModel from inside the Update goroutine. Same race-avoidance
// rationale as [setAgentCallbacksMsg]. Currently empty — coding-
// flavored callbacks may be re-added when needed.
type setCodingCallbacksMsg struct{}

// AppModel is the top-level Bubble Tea model.
// It is a thin presentation layer: maps input to engine Session methods,
// reads Session state to render, and adapts agent events to tea.Msg.
type AppModel struct {
	// Session is the engine session that owns all domain logic.
	Session *session.Session

	// Editor is the code editor pane model.
	Editor *EditorModel
	// AgentPane is the agent conversation pane model.
	AgentPane *AgentPaneModel
	// ProjectPane is the file-tree / project sidebar model.
	ProjectPane *ProjectPaneModel

	// Regions manages layout zones and focus routing.
	Regions *RegionManager

	// Dialog is the modal confirmation dialog state.
	Dialog DialogModel
	// Palette is the command / file palette overlay state.
	Palette PaletteModel
	// Help is the keyboard-shortcut help overlay state.
	Help HelpModel
	// SearchOverlay is the project-wide search overlay state.
	SearchOverlay       SearchOverlayModel
	recentMouse         bool   // tracks leaked CSI prefix from unparsed mouse events
	terse               bool   // true when terse output mode is active
	pendingModelProfile string // profile of the in-flight ListModels request (stale detection)

	// ListModels returns available models for the given profile.
	// Set by the entry point — nil when no LLM is configured.
	ListModels func(profile string) ([]ModelSelectorItem, error)

	// LLMProfileNames returns the available profile names.
	// Set by the entry point — nil when no LLM is configured.
	LLMProfileNames func() []string

	// ConnectOAuth starts an OAuth connection flow for the given profile.
	// Returns a tea.Cmd that runs the flow asynchronously and delivers
	// oauthConnectResultMsg when complete. The flow may emit intermediate
	// messages (e.g., oauthInstructionMsg with the device code).
	// Set by the entry point — nil when OAuth is not available.
	ConnectOAuth func(profile string) tea.Cmd

	// IsOAuthProfile reports whether a profile uses OAuth authentication.
	// Returns the oauth provider ID (e.g., "openai", "copilot") or "" if not OAuth.
	IsOAuthProfile func(profile string) string

	// HasOAuthToken reports whether a valid token exists for an OAuth profile.
	HasOAuthToken func(profile string) bool

	// StoreAPIKey saves an API key for a profile to disk.
	// Set by the entry point — nil when key storage is not available.
	StoreAPIKey func(profile, key string) error

	// HasAPIKey reports whether a stored or env-based API key exists for a profile.
	HasAPIKey func(profile string) bool

	// ToggleTerse enables or disables terse output mode at runtime.
	// Returns the new state (true = enabled). Set by the entry point.
	ToggleTerse func(enabled bool) bool

	// Services holds shared runtime services (clipboard, LSP, etc.).
	Services *Services
	// Keymap holds the active key-binding configuration.
	Keymap *Keymap
	// Quit signals that the application should exit.
	Quit bool
	// Width is the current terminal width in columns.
	Width int
	// Height is the current terminal height in rows.
	Height  int
	program *tea.Program

	// fileWatcher monitors open files for external changes.
	// nil when the OS watcher is unavailable.
	fileWatcher *FileWatcher

	// blockedPaths tracks Path values that have ALREADY been
	// approved-after-Block in this session. The first Block on a
	// file surfaces normally (developer must Ctrl+O / Esc);
	// subsequent Blocks on the same file auto-apply with a
	// "snoozed" banner. Cuts the per-file repeat-prompt churn the
	// Pac-Man eval surfaced — once the developer has eyes-on with
	// a particular over-cap file, further nags don't add
	// information. Reset at process boundaries (NewApp seeds an
	// empty map); never persisted across sessions.
	blockedPaths map[string]bool

	// pendingBlockedPath is the Path of a currently-surfaced Block
	// awaiting developer decision. Set when EditProposed surfaces
	// with a Block summary; copied to blockedPaths on Ctrl+O
	// (manual approve = "yes I've seen this file's situation");
	// cleared without snoozing on Esc (reject means "this specific
	// edit is wrong, don't decide for me on the next one"). Empty
	// when no Block is currently surfaced.
	pendingBlockedPath string

	// editorPool maps canonical paths to the per-file editor
	// controllers the TUI maintains over each session OpenFile.
	// Cursor, selection, scroll, and highlighter state live on these
	// editors — Session never holds a UI controller. Editors are
	// lazily created on first need and reused across file switches so
	// each file's cursor/scroll position is preserved.
	editorPool map[string]*editor.Editor

	// highlighterFactory builds a highlighter for a given file path.
	// Set by composition root (cmd/nib-code/main.go) at startup. Nil
	// disables highlighting (e.g. tests).
	highlighterFactory syntax.HighlighterFactory
}

// editorForOpenFile returns the pooled editor for the given open file,
// creating and decorating one on first use. Returns nil if of is nil.
// Cursor/scroll state is preserved across calls because each path
// resolves to a single editor instance for the lifetime of the AppModel.
//
// Empty-path scratch handles (the fallback installed when Session has
// no real active file) use "" as the pool key, mirroring
// [Session.openFiles] so future iteration/lookup paths stay consistent.
// Every other path is canonicalized.
// SetProgram sets the tea.Program reference.
func (m *AppModel) SetProgram(p *tea.Program) {
	m.program = p
}

// Program returns the tea.Program reference for sending async messages.
func (m *AppModel) Program() *tea.Program { return m.program }

// OAuthInstruction creates an oauthInstructionMsg for delivery via Program.Send.
func OAuthInstruction(profile, instruction string) tea.Msg {
	return oauthInstructionMsg{profile: profile, instruction: instruction}
}

// OAuthConnectResult creates an oauthConnectResultMsg for delivery via tea.Cmd.
func OAuthConnectResult(profile string, err error) tea.Msg {
	return oauthConnectResultMsg{profile: profile, err: err}
}

// SetAgentCallbacksMsg constructs a tea.Msg that installs generic
// agent callbacks on the AppModel. Routed through Program.Send so
// the field assignment happens inside the single-threaded Update
// goroutine — calling code from outside the event loop must use this
// path (or Config-time wiring) to stay race-free.
func SetAgentCallbacksMsg(toggleTerse func(enabled bool) bool, initialTerse bool) tea.Msg {
	return setAgentCallbacksMsg{toggleTerse: toggleTerse, initialTerse: initialTerse}
}

// SetCodingCallbacksMsg constructs a tea.Msg that installs
// coding-flavored agent callbacks on the AppModel. Same race-avoidance
// rationale as [SetAgentCallbacksMsg]. Currently a no-op — kept for
// future coding-flavored callbacks.
func SetCodingCallbacksMsg() tea.Msg {
	return setCodingCallbacksMsg{}
}

// NewApp creates the application model.
func NewApp(sess *session.Session) AppModel {
	km := DefaultKeymap()
	svc := NewServices()

	m := AppModel{
		Session:      sess,
		Services:     svc,
		Keymap:       km,
		blockedPaths: map[string]bool{},
		editorPool:   make(map[string]*editor.Editor),
	}
	// Seed the editor pool from the session's initial active open file.
	// editorForOpenFile lazily creates+decorates one if needed; we resolve
	// it eagerly here so the EditorModel below holds the same instance
	// future activeEditor() calls return.
	initialEditor := m.editorForOpenFile(sess.ActiveOpenFile())
	if initialEditor == nil {
		// Defensive: Session guarantees activeOpenFile != nil, but if a
		// caller installed something unusual, fall back to a lone empty
		// editor that we don't track in the pool (no path).
		initialEditor = editor.New(buffer.New())
	}
	editorPane := NewEditorModel(initialEditor, km, svc)
	editorPane.OnSave = func() { sess.NotifySaved() }
	agentPane := NewAgentPaneModel(svc, sess.HasAgent())
	projectPane := NewProjectPaneModel(sess)

	rm := NewRegionManager(Horizontal)
	rm.Add("project", projectPane, 0.2)
	rm.Add("editor", editorPane, 0.5)
	rm.Add("agent", agentPane, 0.3)
	rm.FocusByName("editor")

	fw := NewFileWatcher(sess)
	// Watch the initial file.
	if fw != nil && sess.ActiveFile() != "" {
		fw.Watch(sess.ActiveFile())
	}

	m.Editor = editorPane
	m.AgentPane = agentPane
	m.ProjectPane = projectPane
	m.Regions = rm
	m.fileWatcher = fw
	m.SearchOverlay = SearchOverlayModel{
		SearchFunc: func(pattern string) ([]search.Result, error) {
			return sess.Search(pattern, search.Options{})
		},
	}
	return m
}

func (m *AppModel) Init() tea.Cmd {
	var cmds []tea.Cmd
	if m.Session.Events() != nil {
		cmds = append(cmds, m.listenForEvents())
	}
	if m.fileWatcher != nil {
		cmds = append(cmds, m.listenForFileChanges())
	}
	if len(cmds) == 0 {
		cmds = append(cmds, func() tea.Msg { return initDoneMsg{} })
	}
	return tea.Batch(cmds...)
}

// listenForEvents returns a tea.Cmd that blocks on the engine event channel
// and delivers the next event as a tea.Msg.
func (m *AppModel) listenForEvents() tea.Cmd {
	ch := m.Session.Events()
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return engineEventMsg{event: ev}
	}
}

func (m *AppModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Modal overlays consume input messages first; non-input messages
	// fall through so engine events / ticks / window resize keep flowing.
	if cmd, handled := m.handleModalInput(msg); handled {
		return m, cmd
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.Width = msg.Width
		m.Height = msg.Height
		// Intent bar always takes 1 row; RegionManager gets the rest.
		m.Regions.SetSize(msg.Width, m.regionHeight())
		return m, nil

	case engineEventMsg:
		cmd := m.handleEngineEvent(msg.event)
		return m, tea.Batch(m.listenForEvents(), cmd)

	case FlushDirtyBuffersMsg:
		return m, m.handleFlushDirtyBuffers(msg)

	case fileChangedMsg:
		m.handleFileChanged(msg.Path)
		return m, m.listenForFileChanges()

	case GoalSubmittedMsg:
		return m.handleGoalSubmitted(msg)
	case PlanningGoalSubmittedMsg:
		return m.handlePlanningGoalSubmitted(msg)

	case DialogResultMsg:
		return m.handleDialogResult(msg)

	case reloadWorkTreeResultMsg:
		return m.handleReloadWorkTreeResult(msg)

	case paletteErrorMsg:
		return m.handlePaletteError(msg)
	case paletteFilesMsg:
		return m.handlePaletteFiles(msg)
	case PaletteResultMsg:
		return m.handlePaletteResult(msg)

	case SearchOpenFileMsg:
		return m.handleSearchOpenFile(msg)
	case searchResultMsg:
		return m.handleSearchResult(msg)

	case modelSelSwitchProfileMsg:
		return m.handleModelSelSwitchProfile(msg)
	case oauthInstructionMsg:
		return m.handleOAuthInstruction(msg)
	case apiKeyEnteredMsg:
		return m.handleAPIKeyEntered(msg)
	case oauthConnectResultMsg:
		return m.handleOAuthConnectResult(msg)
	case setAgentCallbacksMsg:
		return m.handleSetAgentCallbacks(msg)
	case setCodingCallbacksMsg:
		return m.handleSetCodingCallbacks(msg)
	case modelListMsg:
		return m.handleModelList(msg)
	case ModelSelectorResultMsg:
		return m.handleModelSelectorResult(msg)

	case completionTriggerMsg:
		return m, m.scheduleCompletion()
	case completionTickMsg:
		return m.handleCompletionTick(msg)
	case completionResultMsg:
		return m.handleCompletionResult(msg)
	case goToDefResultMsg:
		return m.applyGoToDefinition(msg)
	case hoverResultMsg:
		return m.handleHoverResult(msg)

	case spinnerTickMsg:
		return m, m.AgentPane.Update(msg)

	case ProjectOpenFileMsg:
		return m.handleProjectOpenFile(msg)
	case ProjectSetActiveGoalMsg:
		return m.handleProjectSetActiveGoal(msg)
	case ProjectMarkGoalDoneMsg:
		return m.handleProjectMarkGoalDone(msg)
	case ProjectCreateFileMsg:
		return m.handleProjectCreateFile(msg)
	case ProjectCreateDirMsg:
		return m.handleProjectCreateDir(msg)
	case ProjectDeleteFileMsg:
		return m.handleProjectDeleteFile(msg)

	case tea.MouseWheelMsg:
		return m.handleMouseWheel(msg)
	case tea.MouseMsg:
		return m.handleMouse(msg)

	case tea.KeyPressMsg:
		slog.Debug("key event", "code", msg.Code, "mod", msg.Mod)
		return m.handleKey(msg)
	}
	return m, nil
}

// clearEditorOverlay delegates scroll correction to the engine and clears
// the TUI overlay state.
//
// bufferMutated should be true when called after a successful apply
// (the buffer already has the replacement content). When false (reject,
// error, done), the buffer is unchanged and the conversion differs.
func (m *AppModel) clearEditorOverlay(bufferMutated bool) {
	o := m.Editor.Overlay
	if o == nil {
		return
	}
	m.Editor.CollapseOverlay(o.StartLine, o.EndLine, o.LineCount(), bufferMutated)
	m.Editor.Overlay = nil
}

// regionHeight returns the height available for the RegionManager
// (total height minus the intent bar and status bar).
func (m *AppModel) regionHeight() int {
	h := m.Height - 2 // 1 intent bar + 1 status bar
	if h < 1 {
		h = 1
	}
	return h
}

func (m *AppModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Drop leaked mouse escape sequence fragments. During rapid scrolling,
	// Bubble Tea's parser can fail to consume full SGR sequences; the
	// fragments leak as printable text. Gate behind recentMouse so we
	// never silently drop legitimate typed/pasted text. Keep
	// recentMouse=true while consecutive leaked sequences flow.
	if m.recentMouse && msg.Text != "" {
		if isLeakedMouseSequence([]rune(msg.Text)) {
			return m, nil
		}
	}
	m.recentMouse = false

	// Agent input mode short-circuits to a small allow-list of global
	// shortcuts; everything else flows into the pane as text input.
	if m.AgentPane.IsInputActive() {
		return m, m.handleAgentInputKey(msg)
	}

	// Project pane inline input — all keys go to the pane.
	if m.ProjectPane != nil && m.ProjectPane.IsInputActive() {
		return m, m.ProjectPane.Update(msg)
	}

	// Cross-pane focus toggle.
	if msg.Code == '\\' && msg.Mod == tea.ModCtrl {
		m.Regions.FocusNext()
		return m, nil
	}

	if cmd, handled := m.handleGlobalAction(m.Keymap.Match(msg)); handled {
		return m, cmd
	}

	// Action unmatched (or AgentReject's no-overlay-no-intent branch) —
	// delegate to the focused pane.
	if pane := m.Regions.FocusedPane(); pane != nil {
		return m, pane.Update(msg)
	}
	return m, nil
}

func (m *AppModel) handleDialogResult(_ DialogResultMsg) (tea.Model, tea.Cmd) {
	// Placeholder — implement specific dialog responses as needed.
	return m, nil
}

// handleMouseWheel translates the wheel event for the intent-bar row
// offset and forwards to the region manager. Sets recentMouse so the
// keypress dispatcher can drop leaked SGR fragments from rapid scrolls.
func (m *AppModel) handleMouseWheel(msg tea.MouseWheelMsg) (tea.Model, tea.Cmd) {
	m.recentMouse = true
	translated := tea.Mouse(msg)
	translated.Y -= 1 // translate Y for intent bar row
	cmd := m.Regions.HandleMouse(tea.MouseWheelMsg(translated))
	return m, cmd
}

// handleMouse translates a mouse event for the intent-bar row offset and
// forwards to the region manager. Any click also unfocuses the agent
// input; the agent pane's own click handler re-focuses if the click
// landed in the input area.
func (m *AppModel) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(tea.MouseClickMsg); ok {
		m.AgentPane.SetInputActive(false)
	}
	mouse := msg.Mouse()
	mouse.Y -= 1 // translate Y for intent bar row
	localMsg := translateMouseMsg(msg, mouse)
	cmd := m.Regions.HandleMouse(localMsg)
	return m, cmd
}

// translateMouseMsg creates a new mouse message with translated coordinates.
func translateMouseMsg(msg tea.MouseMsg, m tea.Mouse) tea.MouseMsg {
	switch msg.(type) {
	case tea.MouseClickMsg:
		return tea.MouseClickMsg(m)
	case tea.MouseReleaseMsg:
		return tea.MouseReleaseMsg(m)
	case tea.MouseMotionMsg:
		return tea.MouseMotionMsg(m)
	case tea.MouseWheelMsg:
		return tea.MouseWheelMsg(m)
	}
	return msg
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

// --- Edit Approval ---
//
// Implementation lives in app_approval.go.
