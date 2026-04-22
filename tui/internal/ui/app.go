package ui

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/filelist"
	"github.com/latebit-io/junto/engine/lang"
	"github.com/latebit-io/junto/engine/llmconfig"
	"github.com/latebit-io/junto/engine/search"
	"github.com/latebit-io/junto/engine/session"
)

// engineEventMsg wraps an engine event.Event for delivery through Bubble Tea.
type engineEventMsg struct{ event event.Event }
type initDoneMsg struct{}

// paletteFilesMsg delivers file listing results from async Walk.
type paletteFilesMsg struct{ items []PaletteItem }

// paletteErrorMsg delivers a file listing error to the UI.
type paletteErrorMsg struct{ err string }

// goToDefResultMsg delivers go-to-definition results from an async LSP request.
type goToDefResultMsg struct {
	// Result from LSP — target location.
	path      string
	line, col int
	err       error
	// Origin cursor for nav stack push.
	originPath            string
	originLine, originCol int
}

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

// completionResultMsg delivers completion results from an async LSP request.
type completionResultMsg struct {
	items        []lang.CompletionItem
	isIncomplete bool
	path         string
	line, col    int
}

// completionTriggerMsg is emitted by the editor after typing a trigger character.
type completionTriggerMsg struct{}

// completionTickMsg fires after a debounce delay to trigger a completion request.
type completionTickMsg struct {
	path      string
	line, col int
}

// hoverResultMsg delivers hover information from an async LSP request.
// Carries the origin path+cursor so stale results are dropped on mismatch.
type hoverResultMsg struct {
	text      string
	path      string
	line, col int
}

type reloadWorkTreeResultMsg struct {
	snap session.WorkTreeSnapshot
}

// Package-level styles for the intent bar — allocated once, not per frame.
var (
	intentIdleStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240")).
			Background(lipgloss.Color("236"))
	intentActiveStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("230")).
				Background(lipgloss.Color("235"))
)

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
	recentMouse         bool                  // tracks leaked CSI prefix from unparsed mouse events
	dial                session.AutonomyLevel // current autonomy level; defaults to session.LevelTrusted
	styleName           string                // current coding style display name; empty when disabled
	evaluatorEnabled    bool                  // true when the style evaluator is active
	terse               bool                  // true when terse output mode is active
	pendingModelProfile string                // profile of the in-flight ListModels request (stale detection)

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

	// CycleStyle advances to the next available coding style and returns its
	// display name (or "" if styles are exhausted and cycling disables enforcement).
	// Set by the entry point — nil when no styles are configured.
	CycleStyle func() string

	// ToggleEvaluator enables or disables the style evaluator at runtime.
	// Returns the new state (true = enabled). Set by the entry point — nil
	// when no style or provider is configured.
	ToggleEvaluator func(enabled bool) bool

	// ToggleTerse enables or disables terse output mode at runtime.
	// Returns the new state (true = enabled). Set by the entry point.
	ToggleTerse func(enabled bool) bool

	// OnDialChange is called when the autonomy dial changes.
	// Set by the entry point — nil when no agent is configured.
	OnDialChange func(level session.AutonomyLevel)

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
}

// CloseWatcher shuts down the file watcher. Safe to call if the watcher is nil.
func (m *AppModel) CloseWatcher() {
	if m.fileWatcher != nil {
		m.fileWatcher.Close()
	}
}

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

// SetStyleName sets the current coding style display name for the status bar.
// Pass empty string to clear the indicator.
func (m *AppModel) SetStyleName(name string) {
	m.styleName = name
}

// SetEvaluatorEnabled sets the evaluator status bar indicator.
func (m *AppModel) SetEvaluatorEnabled(enabled bool) {
	m.evaluatorEnabled = enabled
}

// cycleStyle advances to the next coding style via the CycleStyle callback.
// Does nothing if no styles are configured.
func (m *AppModel) cycleStyle() {
	if m.CycleStyle == nil {
		return
	}
	m.styleName = m.CycleStyle()
}

// toggleEvaluator flips the style evaluator on/off via the ToggleEvaluator callback.
// Does nothing if no callback is wired (no agent or no provider).
func (m *AppModel) toggleEvaluator() {
	if m.ToggleEvaluator == nil {
		return
	}
	m.evaluatorEnabled = m.ToggleEvaluator(!m.evaluatorEnabled)
}

// SetTerse sets the terse mode indicator. Use this at startup to sync
// the UI with the agent's initial state.
func (m *AppModel) SetTerse(on bool) {
	m.terse = on
}

// toggleTerse flips terse output mode on/off via the ToggleTerse callback.
// Does nothing if no callback is wired.
func (m *AppModel) toggleTerse() {
	if m.ToggleTerse == nil {
		return
	}
	m.terse = m.ToggleTerse(!m.terse)
}

func (m *AppModel) cycleDial() {
	m.dial = m.dial.Cycle()
	if m.OnDialChange != nil {
		m.OnDialChange(m.dial)
	}
}

// NewApp creates the application model.
func NewApp(sess *session.Session) AppModel {
	km := DefaultKeymap()
	svc := NewServices()

	editorPane := NewEditorModel(sess.ActiveEditor(), km, svc)
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

	return AppModel{
		Session:     sess,
		Editor:      editorPane,
		AgentPane:   agentPane,
		ProjectPane: projectPane,
		Regions:     rm,
		Services:    svc,
		Keymap:      km,
		dial:        session.LevelTrusted,
		fileWatcher: fw,
		SearchOverlay: SearchOverlayModel{
			SearchFunc: func(pattern string) ([]search.Result, error) {
				return sess.Search(pattern, search.Options{})
			},
		},
	}
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

// listenForFileChanges returns a tea.Cmd that blocks on the file watcher
// channel and delivers the next change as a tea.Msg.
func (m *AppModel) listenForFileChanges() tea.Cmd {
	ch := m.fileWatcher.Changes()
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
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
	// Help overlay is modal for user input only — non-input messages
	// (engine events, window resize, ticks) must still be processed.
	if m.Help.Active {
		switch typed := msg.(type) {
		case tea.KeyPressMsg:
			m.Help.Update(typed, m.Height-2)
			return m, nil
		case tea.MouseMsg:
			return m, nil
		}
	}

	// Model selector is modal — captures most input when active.
	// Ctrl+Q always quits regardless of modal state.
	if m.AgentPane.IsModelSelectorActive() {
		switch typed := msg.(type) {
		case tea.KeyPressMsg:
			if m.Keymap.Match(typed) == ActionQuit {
				m.Quit = true
				return m, tea.Quit
			}
			cmd := m.AgentPane.UpdateModelSelector(typed)
			return m, cmd
		case tea.MouseMsg:
			return m, nil
		}
	}

	// Palette is modal — captures all input when active
	if m.Palette.Active {
		switch typed := msg.(type) {
		case tea.KeyPressMsg:
			cmd := m.Palette.Update(typed)
			return m, cmd
		case tea.MouseMsg:
			return m, nil
		}
	}

	// Search overlay is modal — captures all input when active
	if m.SearchOverlay.Active {
		switch typed := msg.(type) {
		case tea.KeyPressMsg:
			cmd := m.SearchOverlay.Update(typed)
			return m, cmd
		case searchResultMsg:
			cmd := m.SearchOverlay.Update(typed)
			return m, cmd
		case SearchOpenFileMsg:
			m.SearchOverlay.Close()
			model, cmd := m.openFile(filepath.Join(m.Session.ProjectRoot(), typed.Path))
			if cmd == nil {
				// Navigate to the specific line.
				m.Editor.MoveCursorTo(typed.Line-1, 0)
				m.Editor.EnsureCursorVisible()
			}
			return model, cmd
		case tea.MouseMsg:
			return m, nil
		}
	}

	// Dialog is modal — captures all input when active
	if m.Dialog.Active {
		switch typed := msg.(type) {
		case tea.KeyPressMsg:
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
		cmd := m.handleEngineEvent(msg.event)
		// Keep listening for the next event
		return m, tea.Batch(m.listenForEvents(), cmd)

	// File watcher — external change detected
	case fileChangedMsg:
		m.handleFileChanged(msg.Path)
		return m, m.listenForFileChanges()

	// Goal submitted from agent pane — delegate to session.
	// Only clear the pane when starting a new conversation, not on follow-ups.
	case GoalSubmittedMsg:
		continued := m.Session.SubmitGoal(msg.Goal)
		if continued {
			m.AgentPane.AppendUserMessage(msg.Goal)
		} else {
			m.AgentPane.Clear()
		}
		return m, nil

	case PlanningGoalSubmittedMsg:
		m.Session.SubmitPlanningGoal(msg.Goal)
		m.AgentPane.Clear()
		return m, nil

	// Answer to a pending request_input prompt — route to the session so
	// the agent goroutine unblocks with the typed answer as a tool result.
	case InputAnsweredMsg:
		m.AgentPane.AppendUserMessage(msg.Text)
		m.Session.AnswerInput(msg.Text)
		return m, nil

	// Esc while awaiting input — cancel the run (matches Esc-rejects-edit).
	case CancelAgentMsg:
		m.Session.CancelAgent()
		return m, nil

	// Dialog result — handle the user's choice
	case DialogResultMsg:
		return m.handleDialogResult(msg)

	case reloadWorkTreeResultMsg:
		if msg.snap.Err != nil {
			slog.Warn("reload work tree", "err", msg.snap.Err)
			m.AgentPane.AppendMeta("[reload project failed: " + msg.snap.Err.Error() + "]\n")
		} else {
			m.Session.ApplyWorkTreeSnapshot(msg.snap)
		}
		m.refreshProjectPane()
		return m, nil

	// File listing error — surface in agent pane
	case paletteErrorMsg:
		slog.Error("failed to list files", "err", msg.err)
		m.AgentPane.AppendMeta("\n[file listing failed: " + msg.err + "]\n")
		return m, nil

	// File listing completed — open the palette with results
	case paletteFilesMsg:
		m.Palette.Open(msg.items)
		return m, nil

	// Tab pressed in model selector — switch to a different provider's models
	case modelSelSwitchProfileMsg:
		if m.ListModels != nil {
			profile := msg.profile
			m.pendingModelProfile = profile
			m.AgentPane.AppendMeta("\n[fetching models for " + profile + "...]\n")
			listFn := m.ListModels
			return m, func() tea.Msg {
				items, err := listFn(profile)
				return modelListMsg{profile: profile, items: items, err: err}
			}
		}
		return m, nil

	// OAuth intermediate instruction (e.g., device code to display)
	case oauthInstructionMsg:
		m.AgentPane.AppendMeta("[" + msg.instruction + "]\n")
		m.AgentPane.AppendMeta("[waiting for authorization...]\n")
		return m, nil

	// API key entered — store and switch to the profile
	case apiKeyEnteredMsg:
		if msg.key == "" {
			return m, nil // cancelled
		}
		if m.StoreAPIKey != nil {
			if err := m.StoreAPIKey(msg.profile, msg.key); err != nil {
				m.AgentPane.AppendMeta("\n[failed to save key: " + err.Error() + "]\n")
				return m, nil
			}
			m.AgentPane.AppendMeta("\n[API key saved for " + msg.profile + "]\n")
			// Switch to this profile's default model.
			dm, switchErr := m.Session.SwitchModel(msg.profile, "")
			m.applySwitchResult(msg.profile, dm, switchErr)
		}
		return m, nil

	// OAuth connection completed — switch to the connected profile.
	// The switcher builds the agent on first successful connect, so the
	// connection is live without restart.
	case oauthConnectResultMsg:
		if msg.err != nil {
			m.AgentPane.AppendMeta("\n[connection failed: " + msg.err.Error() + "]\n")
			return m, nil
		}
		m.AgentPane.AppendMeta("\n[connected to " + msg.profile + "!]\n")
		dm, switchErr := m.Session.SwitchModel(msg.profile, "")
		m.applySwitchResult(msg.profile, dm, switchErr)
		return m, nil

	// Model list fetched — open the inline selector in the agent pane
	case modelListMsg:
		// Drop stale responses from superseded requests.
		if msg.profile != m.pendingModelProfile {
			return m, nil
		}
		if msg.err != nil {
			var profiles []string
			if m.LLMProfileNames != nil {
				profiles = m.LLMProfileNames()
			}
			// OAuth profile without token → offer to connect.
			if m.IsOAuthProfile != nil && m.HasOAuthToken != nil {
				if providerID := m.IsOAuthProfile(msg.profile); providerID != "" && !m.HasOAuthToken(msg.profile) {
					m.AgentPane.OpenModelSelector([]ModelSelectorItem{{
						ID: "_connect", Name: "Connect to " + msg.profile, Profile: msg.profile,
					}}, msg.profile, "", profiles)
					return m, nil
				}
			}
			// API key profile without key → offer to enter one.
			if m.StoreAPIKey != nil && m.HasAPIKey != nil && !m.HasAPIKey(msg.profile) {
				m.AgentPane.OpenModelSelector([]ModelSelectorItem{{
					ID: "_enter_key", Name: "Enter API key for " + msg.profile, Profile: msg.profile,
				}}, msg.profile, "", profiles)
				return m, nil
			}
			m.AgentPane.AppendMeta("\n[failed to list models: " + msg.err.Error() + "]\n")
			return m, nil
		}
		if len(msg.items) == 0 {
			m.AgentPane.AppendMeta("\n[no models available from provider]\n")
			return m, nil
		}
		var profiles []string
		if m.LLMProfileNames != nil {
			profiles = m.LLMProfileNames()
		}
		m.AgentPane.OpenModelSelector(msg.items, msg.profile, m.Session.LLMModel(), profiles)
		return m, nil

	// Model selector result — user selected a model/profile or cancelled
	case ModelSelectorResultMsg:
		if msg.Cancelled {
			return m, nil
		}
		// Profile selection (multi-profile mode) — fetch models for that profile.
		if msg.ModelID == "" && msg.Profile != "" && m.ListModels != nil {
			profile := msg.Profile
			m.pendingModelProfile = profile
			m.AgentPane.AppendMeta("\n[fetching models for " + profile + "...]\n")
			listFn := m.ListModels
			return m, func() tea.Msg {
				items, err := listFn(profile)
				return modelListMsg{profile: profile, items: items, err: err}
			}
		}
		// OAuth connect action — user selected "Connect to [provider]".
		if msg.ModelID == "_connect" && m.ConnectOAuth != nil {
			return m.startOAuthConnect(msg.Profile)
		}
		// API key entry action — switch to key input mode.
		if msg.ModelID == "_enter_key" {
			m.AgentPane.StartAPIKeyInput(msg.Profile)
			return m, nil
		}
		// Model selection — switch to the chosen model.
		if msg.ModelID != "" {
			dm, switchErr := m.Session.SwitchModel(msg.Profile, msg.ModelID)
			m.applySwitchResult(msg.Profile, dm, switchErr)
		}
		return m, nil

	// Palette result — user selected a file or cancelled
	case PaletteResultMsg:
		if !msg.Cancelled && msg.Category == "file" {
			return m.openFile(msg.Item.Value)
		}
		return m, nil

	// Search result — user selected a file:line from project search.
	// Reachable if the overlay closes before the message is delivered.
	case SearchOpenFileMsg:
		model, cmd := m.openFile(filepath.Join(m.Session.ProjectRoot(), msg.Path))
		if cmd == nil {
			m.Editor.MoveCursorTo(msg.Line-1, 0)
			m.Editor.EnsureCursorVisible()
		}
		return model, cmd

	// Async search results — forward to overlay if still active.
	case searchResultMsg:
		if m.SearchOverlay.Active {
			cmd := m.SearchOverlay.Update(msg)
			return m, cmd
		}
		return m, nil

	// Completion trigger — editor typed a trigger character, schedule debounced request.
	case completionTriggerMsg:
		return m, m.scheduleCompletion()

	// Completion debounce tick — fire the actual request.
	case completionTickMsg:
		return m.handleCompletionTick(msg)

	// Agent spinner advance — forward to the pane so it can advance the
	// frame and reschedule (or drop the loop if status went idle).
	case spinnerTickMsg:
		return m, m.AgentPane.Update(msg)

	// Completion result — show popup if still relevant.
	case completionResultMsg:
		var curLine, curCol int
		if m.Editor.Overlay != nil && m.Editor.Overlay.Active {
			oe := m.Editor.Overlay.Editor
			curLine = m.Editor.Overlay.StartLine + oe.CursorLine
			curCol = oe.CursorCol
		} else {
			curLine, curCol = m.Editor.CursorPosition()
		}
		if msg.path == m.Session.ActiveFile() &&
			msg.line == curLine &&
			msg.col == curCol &&
			len(msg.items) > 0 {
			m.Editor.Completion.Show(msg.items, msg.line, msg.col)
		}
		return m, nil

	// Go-to-definition result — apply navigation on the TUI goroutine.
	case goToDefResultMsg:
		return m.applyGoToDefinition(msg)

	// Hover result — display if still relevant, drop stale responses.
	case hoverResultMsg:
		curLine, curCol := m.Editor.CursorPosition()
		if msg.text != "" &&
			msg.path == m.Session.ActiveFile() &&
			msg.line == curLine &&
			msg.col == curCol {
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

	case ProjectSetActiveGoalMsg:
		if err := m.Session.SetActiveGoal(msg.Title); err != nil {
			slog.Warn("set active goal", "err", err)
			m.AgentPane.AppendMeta("[set active goal failed: " + err.Error() + "]\n")
		}
		m.refreshProjectPane()
		return m, nil

	case ProjectMarkGoalDoneMsg:
		if err := m.Session.MarkGoalDone(msg.Title); err != nil {
			slog.Warn("mark goal done", "err", err)
			m.AgentPane.AppendMeta("[mark done failed: " + err.Error() + "]\n")
		}
		m.refreshProjectPane()
		return m, nil

	case ProjectCreateFileMsg:
		if err := m.Session.WriteFile(msg.Path, ""); err != nil {
			slog.Warn("create file", "err", err)
			m.AgentPane.AppendMeta("[create failed: " + err.Error() + "]\n")
			return m, nil
		}
		m.refreshProjectPane()
		absPath := filepath.Join(m.Session.ProjectRoot(), msg.Path)
		return m.openFile(absPath)

	case ProjectCreateDirMsg:
		if err := m.Session.CreateDir(msg.Path); err != nil {
			slog.Warn("create dir", "err", err)
			m.AgentPane.AppendMeta("[create dir failed: " + err.Error() + "]\n")
			return m, nil
		}
		m.ProjectPane.AddEmptyDir(msg.Path)
		m.refreshProjectPane()
		return m, nil

	case ProjectDeleteFileMsg:
		absPath := filepath.Join(m.Session.ProjectRoot(), msg.Path)
		prevActive := m.Session.ActiveFile()
		if err := m.Session.DeleteFile(absPath); err != nil {
			slog.Warn("delete file", "err", err)
			m.AgentPane.AppendMeta("[delete failed: " + err.Error() + "]\n")
			return m, nil
		}
		// Preserve parent directory in tree if it's now empty on disk.
		parentRel := filepath.ToSlash(filepath.Dir(msg.Path))
		if parentRel != "." && parentRel != "" {
			parentAbs := filepath.Join(m.Session.ProjectRoot(), parentRel)
			if entries, err := os.ReadDir(parentAbs); err == nil && len(entries) == 0 {
				m.ProjectPane.AddEmptyDir(parentRel)
			}
		}
		m.refreshProjectPane()
		// If the active editor changed (deleted file or dir containing it),
		// rebuild the editor pane to reflect the session's fallback.
		if m.Session.ActiveFile() != prevActive {
			m.rebuildEditorModel()
		}
		return m, nil

	case tea.MouseWheelMsg:
		m.recentMouse = true
		// Translate Y for intent bar row
		translated := tea.Mouse(msg)
		translated.Y -= 1
		cmd := m.Regions.HandleMouse(tea.MouseWheelMsg(translated))
		return m, cmd

	case tea.MouseMsg:
		// Any click unfocuses the agent input; the agent pane's own
		// click handler re-focuses if the click landed in the input area.
		if _, ok := msg.(tea.MouseClickMsg); ok {
			m.AgentPane.SetInputActive(false)
		}
		// Translate Y for intent bar row
		mouse := msg.Mouse()
		mouse.Y -= 1
		localMsg := translateMouseMsg(msg, mouse)
		cmd := m.Regions.HandleMouse(localMsg)
		return m, cmd

	case tea.KeyPressMsg:
		slog.Debug("key event", "code", msg.Code, "mod", msg.Mod)
		return m.handleKey(msg)
	}
	return m, nil
}

// handleEngineEvent updates session state and renders the event.
func (m *AppModel) handleEngineEvent(ev event.Event) tea.Cmd {
	// Let session update domain state (intent, pending edit)
	m.Session.HandleEvent(ev)

	var cmd tea.Cmd

	// Render in agent pane (presentation)
	switch e := ev.(type) {
	case event.AgentToken:
		m.AgentPane.AppendToken(e.Text)
	case event.AgentToolCall:
		m.AgentPane.AppendMeta("\n> " + e.Name + "\n")
	case event.AgentStatus:
		cmd = tea.Batch(cmd, m.AgentPane.SetStatus(e.Status))
	case event.AgentEditProposed:
		m.clearEditorOverlay(false)

		// ReviewEdit computes the diff AND marks the edit as reviewed.
		// If the edit targets a different file, the session auto-switches
		// and we rebuild the EditorModel to render the correct buffer.
		diff, switched := m.Session.ReviewEdit()
		if switched {
			m.rebuildEditorModel()
			m.refreshDiagnostics(m.Session.ActiveFile())
		}
		if diff != nil {
			m.AgentPane.AppendMeta("\n--- Proposed: " + e.Edit.Reason + " ---\n")
			slog.Debug("overlay created", "startLine", diff.StartLine, "endLine", diff.EndLine, "newLines", len(diff.NewLines))

			// Build the overlay — applyApproval reads it for
			// search/replace content even when we skip the visual review.
			m.Editor.Overlay = NewDiffOverlay(diff)

			if m.dial.AutoApproveEdits() {
				// At LevelTrusted, skip the visual review step and apply
				// immediately.
				cmd = m.applyApproval()
			} else {
				cmd = tea.Batch(cmd, m.AgentPane.SetStatus(event.StatusReviewing))
				m.Editor.Overlay.Active = true
				// Auto-scroll so the diff is visible with some context above.
				target := diff.StartLine - 3
				if target < 0 {
					target = 0
				}
				m.Editor.SetScrollOffset(target)
				m.Editor.syncExtraVisualLines()
				m.Editor.ClampScroll()
			}
		} else {
			slog.Warn("ReviewEdit returned nil — search text not found or not unique")
			m.AgentPane.AppendMeta("[edit could not be matched — auto-rejecting]\n")
			m.Session.RejectEdit()
		}
	case event.AgentFileCreated:
		m.AgentPane.AppendMeta("\n[Created: " + e.Path + "]\n")
		// Set pendingReveal before openFile — openFile calls
		// refreshProjectPane internally, which triggers rebuild.
		// ExpandToPath runs during that rebuild to uncollapse
		// ancestor dirs so the new file is visible in the tree.
		if m.ProjectPane != nil {
			m.ProjectPane.pendingReveal = e.Path
		}
		m.openFile(e.Path)
		// If openFile failed (e.g. edit pending), the file was still
		// created on disk. Refresh the project pane so it appears in
		// the tree and pendingReveal is consumed.
		if m.ProjectPane != nil && m.ProjectPane.pendingReveal != "" {
			m.refreshProjectPane()
		}
	case event.AgentNavigate:
		prevActive := m.Session.ActiveEditor()
		if err := m.Session.NavigateAgent(e.Path, e.Line-1); err != nil {
			slog.Warn("agent navigate failed", "path", e.Path, "err", err)
			m.AgentPane.AppendMeta("[navigate failed: " + err.Error() + "]\n")
			break
		}
		// If the session switched files, update TUI-owned state to match.
		if m.Session.ActiveEditor() != prevActive {
			if m.fileWatcher != nil {
				m.fileWatcher.Watch(m.Session.ActiveFile())
			}
			m.rebuildEditorModel()
			m.refreshDiagnostics(m.Session.ActiveFile())
			m.refreshProjectPane()
		}
	case event.AgentError:
		// Terminal branch — drop pane status to idle so the spinner loop
		// stops rescheduling and any in-flight streaming tint settles.
		cmd = tea.Batch(cmd, m.AgentPane.SetStatus(event.StatusIdle))
		m.AgentPane.AppendMeta("\nError: " + e.Err + "\n")
		m.AgentPane.ClearAwaitingInput()
		m.clearEditorOverlay(false)
	case event.AgentWaiting:
		if m.Session.Phase() == session.PhasePlanning {
			cmd = tea.Batch(cmd, m.AgentPane.SetStatus(event.StatusPlanningWaiting))
		} else {
			cmd = tea.Batch(cmd, m.AgentPane.SetStatus(event.StatusWaiting))
		}
		m.AgentPane.SetInputActive(true)
		m.AgentPane.ResetInput()
		// Agent may have published /project.md — reload async to stay in sync.
		cmd = tea.Batch(cmd, m.reloadWorkTreeCmd())
	case event.AgentAwaitingInput:
		m.AgentPane.ShowAwaitingInput(e)
	case event.AgentDone:
		cmd = tea.Batch(cmd, m.AgentPane.SetStatus(event.StatusIdle))
		m.AgentPane.ClearAwaitingInput()
		summary := formatSessionSummary(m.AgentPane.usage)
		if summary != "" {
			m.AgentPane.AppendMeta("\n--- Done ---\n" + summary + "\n")
		} else {
			m.AgentPane.AppendMeta("\n--- Done ---\n")
		}
		m.AgentPane.SetInputActive(false)
		// Agent may have published /project.md — reload async to stay in sync.
		cmd = tea.Batch(cmd, m.reloadWorkTreeCmd())
		m.clearEditorOverlay(false)
	case event.FlushBuffers:
		saved, err := m.Session.SaveDirtyBuffers()
		e.Result <- event.FlushResult{Saved: saved, Err: err}
	case event.DiagnosticsUpdated:
		m.refreshDiagnostics(e.Path)
	case event.ReloadBuffers:
		m.reloadAllBuffers()
	case event.AgentInputEstimate:
		m.AgentPane.SetStreamingInput(e)
	case event.AgentTurnUsage:
		m.AgentPane.AppendMeta(formatTurnUsage(e))
		m.AgentPane.UpdateUsage(e)
	case event.AgentCompacted:
		m.AgentPane.AppendMeta(formatCompacted(e))
	}
	return cmd
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
	// Drop leaked mouse escape sequence fragments.
	// During rapid scrolling, Bubble Tea's parser can fail to consume full SGR
	// sequences. The fragments leak as printable text — a full SGR body like
	// '<65;14;32M' or '[<65;14;32M'. Gate behind recentMouse so we never
	// silently drop legitimate typed/pasted text.
	if m.recentMouse && msg.Text != "" {
		if isLeakedMouseSequence([]rune(msg.Text)) {
			// Keep recentMouse=true so consecutive leaked sequences from
			// rapid scrolling are all caught, not just the first one.
			return m, nil
		}
	}
	m.recentMouse = false

	// Agent pane input mode — most keys go to agent pane, but global
	// actions (model selector, dial cycle) are handled here first.
	if m.AgentPane.IsInputActive() {
		switch m.Keymap.Match(msg) {
		case ActionModelSelector:
			return m, m.openModelSelector()
		case ActionDialCycle:
			m.cycleDial()
			return m, nil
		case ActionStyleCycle:
			m.cycleStyle()
			return m, nil
		case ActionEvaluatorToggle:
			m.toggleEvaluator()
			return m, nil
		case ActionTerseToggle:
			m.toggleTerse()
			return m, nil
		}
		cmd := m.AgentPane.Update(msg)
		return m, cmd
	}

	// Project pane inline input — all keys go to project pane
	if m.ProjectPane != nil && m.ProjectPane.IsInputActive() {
		cmd := m.ProjectPane.Update(msg)
		return m, cmd
	}

	// Toggle focus between visible panes
	if msg.Code == '\\' && msg.Mod == tea.ModCtrl {
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
		slog.Debug("agent approve", "pending", m.Session.PendingEdit() != nil, "agent", m.Session.HasAgent())
		if m.Session.PendingEdit() != nil && m.Editor.Overlay != nil {
			cmd := m.applyApproval()
			return m, cmd
		}
		return m, nil

	case ActionAgentReject:
		// Completion popup gets priority — dismiss it first.
		if m.Editor.Completion.Active {
			m.Editor.Completion.Dismiss()
			return m, nil
		}
		if m.Session.PendingEdit() != nil {
			slog.Debug("overlay cleared", "reason", "reject")
			m.clearEditorOverlay(false)
			m.Session.RejectEdit()
			return m, nil
		}
		// No pending edit — cancel agent if active
		if m.Session.CurrentIntent() != "" && m.Session.HasAgent() {
			m.Session.CancelAgent()
			return m, m.AgentPane.SetStatus(event.StatusIdle)
		}
		// No intent either — fall through to focused pane

	case ActionAgentContinue:
		if m.Session.CanContinue() {
			m.Session.Continue()
		}
		return m, nil

	case ActionDialCycle:
		m.cycleDial()
		return m, nil

	case ActionStyleCycle:
		m.cycleStyle()
		return m, nil

	case ActionEvaluatorToggle:
		m.toggleEvaluator()
		return m, nil

	case ActionTerseToggle:
		m.toggleTerse()
		return m, nil

	case ActionModelSelector:
		return m, m.openModelSelector()

	case ActionAgentStart:
		if m.Session.HasAgent() {
			m.AgentPane.SetInputActive(true)
			m.AgentPane.SetPlanningMode(false)
		}
		return m, nil

	case ActionAgentPlan:
		if m.Session.HasAgent() {
			m.AgentPane.SetInputActive(true)
			m.AgentPane.SetPlanningMode(true)
		}
		return m, nil

	case ActionToggleProject:
		return m.handleToggleProject()

	case ActionReloadFile:
		return m.reloadActiveFile()

	case ActionNextBuffer:
		return m.switchBuffer(1)
	case ActionPrevBuffer:
		return m.switchBuffer(-1)

	case ActionOpenPalette:
		if root := m.Session.ProjectRoot(); root != "" {
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

	case ActionFindInProject:
		m.SearchOverlay.Open()
		return m, nil

	case ActionFind:
		m.Regions.FocusByName("editor")
		m.Editor.Find.Open(m.Editor.Engine(), false)
		return m, nil

	case ActionFindReplace:
		m.Regions.FocusByName("editor")
		m.Editor.Find.Open(m.Editor.Engine(), true)
		return m, nil

	case ActionHelp:
		m.Help.Open()
		return m, nil

	case ActionFocusProject:
		m.Regions.FocusByName("project")
		return m, nil

	case ActionFocusEditor:
		m.Regions.FocusByName("editor")
		return m, nil

	case ActionFocusAgent:
		m.Regions.FocusByName("agent")
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

// openModelSelector opens the model selector overlay.
// If multiple profiles exist, shows profile picker first.
// If one profile, fetches models directly.
// If no LLM is configured but OAuth profiles exist, offers connection.
// applySwitchResult updates the agent pane after a model switch attempt.
// Handles three outcomes: full success, success with persistence warning
// (displayModel non-empty but err non-nil), and outright failure.
func (m *AppModel) applySwitchResult(profile, displayModel string, err error) {
	if displayModel == "" && err != nil {
		m.AgentPane.AppendMeta("\n[switch failed: " + err.Error() + "]\n")
		return
	}
	label := displayModel
	if profile != "" {
		label = profile + ": " + displayModel
	}
	m.AgentPane.SetModelLabel(label)
	if err != nil {
		m.AgentPane.AppendMeta("\n[switched to " + label + " — selection may not persist]\n")
	} else {
		m.AgentPane.AppendMeta("\n[switched to " + label + "]\n")
	}
}

func (m *AppModel) openModelSelector() tea.Cmd {
	if m.ListModels == nil {
		// No provider configured — check if we can offer OAuth profiles to connect.
		if m.IsOAuthProfile != nil && m.HasOAuthToken != nil && m.LLMProfileNames != nil {
			profiles := m.LLMProfileNames()
			slog.Debug("model selector: no provider, checking profiles", "count", len(profiles))
			var connectItems []ModelSelectorItem
			for _, p := range profiles {
				if providerID := m.IsOAuthProfile(p); providerID != "" && !m.HasOAuthToken(p) {
					connectItems = append(connectItems, ModelSelectorItem{
						ID:      "_connect",
						Name:    "Connect to " + p,
						Profile: p,
					})
				}
			}
			slog.Debug("model selector: connect items", "count", len(connectItems))
			if len(connectItems) > 0 {
				m.AgentPane.OpenModelSelector(connectItems, connectItems[0].Profile, "", profiles)
				slog.Debug("model selector: opened", "active", m.AgentPane.IsModelSelectorActive())
				return nil
			}
		} else {
			slog.Debug("model selector: callbacks missing", "isOAuth", m.IsOAuthProfile != nil, "hasToken", m.HasOAuthToken != nil, "profiles", m.LLMProfileNames != nil)
		}
		globalPath := llmconfig.GlobalConfigPath()
		if globalPath == "" {
			globalPath = "<user-config-dir>/junto/llm.json"
		}
		m.AgentPane.AppendMeta("\n[no LLM configured — create " + globalPath + " or .project/llm.json]\n")
		return nil
	}
	// Fetch models for the current profile. Tab cycles providers if multiple exist.
	profile := m.Session.LLMProfile()
	m.pendingModelProfile = profile
	m.AgentPane.AppendMeta("\n[fetching models...]\n")
	listFn := m.ListModels
	return func() tea.Msg {
		items, err := listFn(profile)
		return modelListMsg{profile: profile, items: items, err: err}
	}
}

// startOAuthConnect initiates an OAuth connection flow for the given profile.
// The flow runs fully async — no blocking on the TUI thread.
func (m *AppModel) startOAuthConnect(profile string) (tea.Model, tea.Cmd) {
	m.AgentPane.AppendMeta("\n[connecting to " + profile + "...]\n")
	return m, m.ConnectOAuth(profile)
}

func (m *AppModel) handleDialogResult(_ DialogResultMsg) (tea.Model, tea.Cmd) {
	// Placeholder — implement specific dialog responses as needed.
	return m, nil
}

func (m *AppModel) View() tea.View {
	var content string
	if m.Width == 0 || m.Height == 0 {
		content = "Initializing..."
	} else {
		mem := m.Session.DistributedMemory()
		extra := 5 // dial + style + evaluator + terse + usage always shown
		indicators := make([]string, len(mem)+extra)
		copy(indicators, mem)
		idx := len(mem)
		indicators[idx] = m.dial.String()
		idx++
		if m.styleName != "" {
			indicators[idx] = "style:" + m.styleName
		} else {
			indicators[idx] = "style:none"
		}
		idx++
		if m.evaluatorEnabled {
			indicators[idx] = "eval:on"
		} else {
			indicators[idx] = "eval:off"
		}
		idx++
		if m.terse {
			indicators[idx] = "terse:on"
		} else {
			indicators[idx] = "terse:off"
		}
		idx++
		indicators[idx] = m.AgentPane.UsageIndicator()
		base := m.renderIntentBar() + "\n" + m.Regions.Render() + "\n" + renderStatusBar(m.Editor.statusInfo(), m.Width, indicators...)
		if m.Dialog.Active {
			content = m.Dialog.RenderOverlay(base, m.Width, m.Height)
		} else if m.Help.Active {
			content = m.Help.RenderOverlay(base, m.Width, m.Height)
		} else if m.Palette.Active {
			content = m.Palette.RenderOverlay(base, m.Width, m.Height)
		} else if m.SearchOverlay.Active {
			content = m.SearchOverlay.RenderOverlay(base, m.Width, m.Height)
		} else {
			content = base
		}
	}

	v := tea.NewView(content)
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

// rebuildEditorModel creates a new EditorModel from the session's active editor.
func (m *AppModel) rebuildEditorModel() {
	m.Editor = NewEditorModel(m.Session.ActiveEditor(), m.Keymap, m.Services)
	m.Editor.OnSave = func() { m.Session.NotifySaved() }
	m.Regions.ReplacePane("editor", m.Editor)
}

// switchBuffer cycles through open buffers by delta (+1 next, -1 prev).
func (m *AppModel) switchBuffer(delta int) (tea.Model, tea.Cmd) {
	files := m.Session.OpenFiles()
	if len(files) <= 1 {
		return m, nil
	}
	slices.Sort(files)
	active := m.Session.ActiveFile()
	idx := 0
	for i, f := range files {
		if f == active {
			idx = i
			break
		}
	}
	next := (idx + delta + len(files)) % len(files)
	return m.openFile(files[next])
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

	// Watch the newly opened file for external changes.
	if m.fileWatcher != nil {
		m.fileWatcher.Watch(m.Session.ActiveFile())
	}

	// Rebuild EditorModel with the new active editor from session.
	m.rebuildEditorModel()

	slog.Debug("file opened", "path", path)
	m.refreshDiagnostics(m.Session.ActiveFile())
	m.refreshProjectPane()
	return m, nil
}

// reloadAllBuffers reloads all open buffers from disk. Called after bash
// tool calls that may have modified files outside the edit approval flow.
// Skips buffers the user has modified in-editor to avoid clobbering unsaved work.
func (m *AppModel) reloadAllBuffers() {
	for _, path := range m.Session.OpenFiles() {
		m.handleFileChanged(path)
	}
}

// handleFileChanged reloads a file that was modified externally.
// Skips reload if the buffer has unsaved in-editor changes.
func (m *AppModel) handleFileChanged(path string) {
	// Don't reload buffers the user has modified in-editor.
	ed := m.Session.EditorForPath(path)
	if ed != nil && ed.IsModified() {
		slog.Debug("skip external reload (buffer modified)", "path", path)
		return
	}
	if err := m.Session.ReloadFile(path); err != nil {
		slog.Warn("auto-reload failed", "path", path, "err", err)
		return
	}
	// If the changed file is the active one, rebuild the editor model.
	if path == m.Session.ActiveFile() {
		m.rebuildEditorModel()
	}
	slog.Debug("auto-reloaded file", "path", path)
}

// reloadActiveFile re-reads the active file from disk into its buffer.
func (m *AppModel) reloadActiveFile() (tea.Model, tea.Cmd) {
	path := m.Session.ActiveFile()
	if path == "" {
		return m, nil
	}
	if err := m.Session.ReloadFile(path); err != nil {
		slog.Error("reload file failed", "path", path, "err", err)
		m.AgentPane.AppendMeta("[error: " + err.Error() + "]\n")
		return m, nil
	}
	m.rebuildEditorModel()
	slog.Debug("file reloaded", "path", path)
	return m, nil
}

func (m *AppModel) renderIntentBar() string {
	var text string
	var style lipgloss.Style

	_, goalPath := m.Session.ActiveGoal()

	switch {
	case !m.Session.HasAgent():
		text = " Editor"
		style = intentIdleStyle
	case goalPath != "":
		text = " " + goalPath
		style = intentActiveStyle
	default:
		text = " Ready"
		style = intentIdleStyle
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

// handleGoToDefinition dispatches an async definition lookup to avoid blocking the TUI.
func (m *AppModel) handleGoToDefinition() (tea.Model, tea.Cmd) {
	if !m.Session.HasLanguageService() {
		return m, nil
	}
	originPath := m.Session.ActiveFile()
	originLine, originCol := m.Editor.CursorPosition()
	return m, func() tea.Msg {
		loc, err := m.Session.LookupDefinition(originLine, originCol)
		if err != nil {
			return goToDefResultMsg{err: err}
		}
		return goToDefResultMsg{
			path:       loc.Path,
			line:       loc.Line,
			col:        loc.Col,
			originPath: originPath,
			originLine: originLine,
			originCol:  originCol,
		}
	}
}

// applyGoToDefinition handles the async definition result on the TUI goroutine.
// Pushes the nav stack, switches files if needed, and moves the cursor.
func (m *AppModel) applyGoToDefinition(msg goToDefResultMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		slog.Debug("go-to-definition failed", "err", msg.err)
		m.Editor.StatusMsg = msg.err.Error()
		return m, nil
	}

	// Push origin onto nav stack (primitives — no lang.Location in TUI).
	m.Session.PushNav(msg.originPath, msg.originLine, msg.originCol)

	// Switch file if the definition is in a different file.
	if msg.path != m.Session.ActiveFile() {
		if err := m.Session.SwitchTo(msg.path); err != nil {
			m.Session.PopNav() // undo the push
			m.Editor.StatusMsg = err.Error()
			return m, nil
		}
	}

	// Rebuild EditorModel if session switched files.
	if m.Session.ActiveEditor() != m.Editor.Engine() {
		m.rebuildEditorModel()
		m.refreshDiagnostics(m.Session.ActiveFile())
	}

	m.Session.ActiveEditor().MoveCursorTo(msg.line, msg.col)
	slog.Debug("go-to-definition", "path", msg.path, "line", msg.line, "col", msg.col)
	return m, nil
}

// handleGoBack returns to the previous location in the navigation stack.
func (m *AppModel) handleGoBack() (tea.Model, tea.Cmd) {
	loc := m.Session.GoBack()
	if loc == nil {
		return m, nil
	}

	// Session may have switched files — rebuild EditorModel if needed.
	if m.Session.ActiveEditor() != m.Editor.Engine() {
		m.rebuildEditorModel()
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

	path := m.Session.ActiveFile()
	line, col := m.Editor.CursorPosition()
	return m, func() tea.Msg {
		text, err := m.Session.HoverInfo(line, col)
		if err != nil {
			slog.Debug("hover failed", "err", err)
			return hoverResultMsg{}
		}
		return hoverResultMsg{text: text, path: path, line: line, col: col}
	}
}

// --- Autocomplete ---

// completionDebounce is the delay before firing a completion request.
const completionDebounce = 100 * time.Millisecond

// scheduleCompletion starts a debounced completion request. Called after
// typing a character. The tick carries a snapshot of path+cursor so stale
// ticks are dropped.
func (m *AppModel) scheduleCompletion() tea.Cmd {
	if !m.Session.HasLanguageService() {
		return nil
	}
	path := m.Session.ActiveFile()
	var line, col int
	if m.Editor.Overlay != nil && m.Editor.Overlay.Active {
		// Map overlay cursor to buffer position for the LSP request.
		// The overlay replaces buffer lines StartLine..EndLine, so the
		// overlay cursor line maps to StartLine + overlayCursorLine.
		oe := m.Editor.Overlay.Editor
		line = m.Editor.Overlay.StartLine + oe.CursorLine
		col = oe.CursorCol
	} else {
		line, col = m.Editor.CursorPosition()
	}
	return tea.Tick(completionDebounce, func(_ time.Time) tea.Msg {
		return completionTickMsg{path: path, line: line, col: col}
	})
}

// handleCompletionTick fires when the debounce timer expires.
// Drops stale ticks (cursor moved since scheduling). Dispatches async request.
func (m *AppModel) handleCompletionTick(msg completionTickMsg) (tea.Model, tea.Cmd) {
	// Drop if cursor moved since the tick was scheduled.
	// Compare against the right cursor (overlay or buffer).
	var curLine, curCol int
	if m.Editor.Overlay != nil && m.Editor.Overlay.Active {
		oe := m.Editor.Overlay.Editor
		curLine = m.Editor.Overlay.StartLine + oe.CursorLine
		curCol = oe.CursorCol
	} else {
		curLine, curCol = m.Editor.CursorPosition()
	}
	if msg.path != m.Session.ActiveFile() ||
		msg.line != curLine ||
		msg.col != curCol {
		return m, nil
	}
	line, col := msg.line, msg.col
	path := msg.path

	// Capture content snapshots on the TUI goroutine (no race).
	// For overlay editing, session needs both merged and original content
	// to temporarily sync the proposed code to LSP and revert afterward.
	var tempContent, originalContent string
	if m.Editor.Overlay != nil && m.Editor.Overlay.Active {
		originalContent = m.Editor.Content()
		tempContent = m.Editor.Overlay.MergedContent(m.Editor.Engine())
	}

	return m, func() tea.Msg {
		var result *lang.CompletionResult
		var err error

		if tempContent != "" {
			// Overlay: sync/query/revert atomically inside session.
			result, err = m.Session.RequestCompletionInContext(path, tempContent, originalContent, line, col)
		} else {
			result, err = m.Session.RequestCompletion(path, line, col)
		}

		if err != nil {
			slog.Debug("completion request failed", "path", path, "line", line, "col", col, "err", err)
			return completionResultMsg{}
		}
		if result == nil {
			return completionResultMsg{}
		}
		return completionResultMsg{
			items:        result.Items,
			isIncomplete: result.IsIncomplete,
			path:         path,
			line:         line,
			col:          col,
		}
	}
}

// reloadWorkTreeCmd returns a tea.Cmd that fetches the work tree from
// demarkus in a background goroutine. The snapshot is applied to session
// state on the TUI goroutine when reloadWorkTreeResultMsg is handled,
// avoiding data races on workTree/workTreeVer fields.
func (m *AppModel) reloadWorkTreeCmd() tea.Cmd {
	return func() tea.Msg {
		return reloadWorkTreeResultMsg{snap: m.Session.FetchWorkTreeSnapshot()}
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

// --- Edit Approval ---

// applyApproval validates the reviewed edit and applies it atomically.
// At dial levels with AutoContinue, the agent is also signalled to continue;
// otherwise the user must press Ctrl+N.
func (m *AppModel) applyApproval() tea.Cmd {
	o := m.Editor.Overlay
	oldLines := make([]string, 0, o.EndLine-o.StartLine+1)
	for i := o.StartLine; i <= o.EndLine; i++ {
		oldLines = append(oldLines, m.Editor.LineText(i))
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
		if m.Session.PendingEdit() == nil {
			m.clearEditorOverlay(false)
		}
		return nil
	}

	// Try the apply first — ApplyEdit short-circuits on LocateEdit failure
	// without mutating the buffer, so the overlay can stay visible if it fails.
	ok, reason := m.Editor.ApplyEdit(plan.Search, plan.Replace, plan.LineOrigins)
	if !ok {
		slog.Warn("apply failed", "reason", reason)
		m.AgentPane.AppendText("\n[apply failed: " + reason + "]\n")
		m.Session.AbortApproval()
		return nil
	}
	// bufferMutated=true: ApplyEdit already replaced the lines, so CollapseOverlay
	// must use the post-mutation coordinate translation (subtract removedCount,
	// not addedCount) to keep the viewport pointing at the right buffer line.
	m.clearEditorOverlay(true)
	m.AgentPane.AppendMeta("[applied]\n")

	// Refresh after CompleteApproval — that's when modifiedFiles is populated,
	// which the project pane reads to render the modified badge.
	var statusCmd tea.Cmd
	if m.dial.AutoContinue() {
		m.Session.ApproveAndContinue()
		statusCmd = m.AgentPane.SetStatus(event.StatusThinking)
	} else {
		m.Session.CompleteApproval()
		statusCmd = m.AgentPane.SetStatus(event.StatusEditing)
	}
	m.refreshProjectPane()
	return statusCmd
}
