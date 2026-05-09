package ui

import (
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/coding/session"
	"github.com/latebit-io/nib/engine/buffer"
	"github.com/latebit-io/nib/engine/openfile"
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
func (m *AppModel) editorForOpenFile(of *openfile.OpenFile) *editor.Editor {
	if of == nil {
		return nil
	}
	key := of.Buf.Path
	if key != "" {
		key = m.Session.CanonPath(of.Buf.Path)
	}
	if m.editorPool == nil {
		m.editorPool = make(map[string]*editor.Editor)
	}
	if e, ok := m.editorPool[key]; ok {
		return e
	}
	e := editor.New(of.Buf)
	if m.highlighterFactory != nil && of.Buf.Path != "" {
		e.SetHighlighter(m.highlighterFactory(of.Buf.Path))
	}
	m.editorPool[key] = e
	return e
}

// activeEditor returns the editor for the session's currently active
// open file. May return nil when the session has no active file.
func (m *AppModel) activeEditor() *editor.Editor {
	return m.editorForOpenFile(m.Session.ActiveOpenFile())
}

// clampPooledEditor clamps the cursor and viewport for the pooled
// editor at path so a buffer-shrinking reload (external tool, agent
// edit, ReloadFromDisk) doesn't leave the cursor out-of-bounds.
// No-op if no editor is pooled for the path. The pool key resolution
// matches [editorForOpenFile]: empty-path scratch handles use "" as
// the key, every other path is canonicalized.
func (m *AppModel) clampPooledEditor(path string) {
	key := path
	if key != "" {
		key = m.Session.CanonPath(path)
	}
	ed, ok := m.editorPool[key]
	if !ok {
		return
	}
	// Self-call MoveCursorTo with current position so the editor's
	// internal clamp logic (line bounds, column bounds) runs against
	// the new buffer length.
	ed.MoveCursorTo(ed.CursorLine, ed.CursorCol)
	ed.ClampScroll()
}

// dropPooledEditors removes pool entries for a path and any children
// (when path was a directory), closing each editor's highlighter so
// tree-sitter grammar instances are released. Mirrors the matching
// logic in [session.cleanupDeletedPath].
func (m *AppModel) dropPooledEditors(path string) {
	canon := m.Session.CanonPath(path)
	dirPrefix := canon + string(filepath.Separator)
	for k, ed := range m.editorPool {
		if k == canon || strings.HasPrefix(k, dirPrefix) {
			ed.Close()
			delete(m.editorPool, k)
		}
	}
}

// SetHighlighterFactory installs the syntax-highlighter factory used by
// the editor pool. Existing pooled editors are re-decorated to match.
// Pass nil to disable highlighting. Called at startup by the composition
// root.
func (m *AppModel) SetHighlighterFactory(fn syntax.HighlighterFactory) {
	m.highlighterFactory = fn
	for _, e := range m.editorPool {
		if e == nil || e.Buf == nil || e.Buf.Path == "" {
			continue
		}
		if fn == nil {
			e.SetHighlighter(nil)
			continue
		}
		e.SetHighlighter(fn(e.Buf.Path))
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

// NewApp creates the application model.
func NewApp(sess *session.Session) AppModel {
	km := DefaultKeymap()
	svc := NewServices()

	m := AppModel{
		Session:      sess,
		Services:     svc,
		Keymap:       km,
		dial:         session.LevelTrusted,
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
	case ProjectAddContextMsg:
		return m.handleProjectAddContext(msg)
	case ProjectRemoveContextMsg:
		return m.handleProjectRemoveContext(msg)
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
		// Indented bullet reads as a sub-action rather than a sibling of
		// the agent's prose. Renders dim via the metaRawLines path.
		m.AgentPane.AppendMeta("\n  ● " + e.Name + "\n")
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

			// Validator guard: refuse auto-approval whenever any
			// non-"pass" verdict reaches us. The agent already
			// exhausted the per-CanonPath retry budget feeding
			// feedback back to the LLM (so the model had its 3
			// attempts to self-correct), and the proposal is now
			// surfacing precisely BECAUSE the LLM couldn't fix it.
			// Auto-applying it under LevelTrusted would invert the
			// validator's purpose. Fail-closed on unknown verdicts
			// too — a future verdict must explicitly opt into
			// auto-apply rather than slipping through this gate.
			//
			// LevelYolo is the explicit opt-out: the developer has
			// accepted that validator Block findings may slip
			// through. m.dial.AutoApproveBlock() returns true only
			// at LevelYolo, so the gate retains the LevelTrusted
			// safety valve by default.
			// summaryRequiresReview is the must-surface gate (true for
			// any non-pass verdict, including retry). summaryHasBlock
			// is the narrower snooze-eligibility gate — only Block
			// findings can be silenced for the rest of the session,
			// because only Block carries the "developer has eyes-on
			// for this file's recurring architectural concern"
			// semantics. Retry is per-edit (LLM exhausted its budget
			// on this specific change) and must be reviewed each time
			// — auto-applying future retries on a once-approved file
			// would re-open a fail-open path for validator-exhausted
			// proposals.
			needsReview := summaryRequiresReview(e.ValidatorSummaries)
			blockSnoozeEligible := summaryHasBlock(e.ValidatorSummaries)
			snoozed := blockSnoozeEligible && m.blockedPaths[e.Edit.Path]
			autoApprove := m.dial.AutoApproveEdits() &&
				(!needsReview || m.dial.AutoApproveBlock() || snoozed)

			if autoApprove {
				switch {
				case snoozed:
					// Per-file snooze: the developer already saw and
					// approved a Block on this file earlier in the
					// session. Repeat Blocks add no new information,
					// so auto-apply with a marker banner instead of
					// re-prompting.
					m.AgentPane.AppendMeta(snoozeBannerForSummaries(e.Edit.Path, e.ValidatorSummaries))
				case needsReview:
					// LevelYolo override path.
					m.AgentPane.AppendMeta(yoloOverrideBannerForSummaries(e.ValidatorSummaries))
				}
				// At LevelTrusted+, skip the visual review step and apply
				// immediately.
				cmd = m.applyApproval()
			} else {
				status := event.StatusReviewing
				if needsReview {
					// Only arm the snooze cache for Block findings —
					// see the comment above on blockSnoozeEligible.
					// Retry-only proposals still surface (needsReview
					// is true) but a manual approval must NOT promote
					// the path into blockedPaths, otherwise a future
					// retry would auto-apply.
					if blockSnoozeEligible {
						m.pendingBlockedPath = e.Edit.Path
					} else {
						m.pendingBlockedPath = ""
					}
					m.AgentPane.AppendMeta(reviewBannerForSummaries(e.ValidatorSummaries))
					// Distinct status so the indicator stands out
					// from routine reviewing — block-review means
					// "validator flagged this, eyes-on required."
					status = event.StatusBlockReview
				}
				cmd = tea.Batch(cmd, m.AgentPane.SetStatus(status))
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
			m.Session.RejectEdit("search-mismatch")
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
		prevActive := m.activeEditor()
		if err := m.Session.NavigateAgent(e.Path); err != nil {
			slog.Warn("agent navigate failed", "path", e.Path, "err", err)
			m.AgentPane.AppendMeta("[navigate failed: " + err.Error() + "]\n")
			break
		}
		// Place the cursor on the TUI's editor for the (possibly newly
		// active) file. Session no longer holds a UI cursor, so this is
		// the frontend's responsibility now.
		if ed := m.activeEditor(); ed != nil {
			ed.ClearSelection()
			ed.MoveCursorTo(e.Line-1, e.Col)
			ed.EnsureCursorVisible()
		}
		// If the session switched files, update TUI-owned state to match.
		if m.activeEditor() != prevActive {
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
		m.clearEditorOverlay(false)
	case event.AgentWaiting:
		switch {
		case m.Session.Phase() == session.PhasePlanning:
			cmd = tea.Batch(cmd, m.AgentPane.SetStatus(event.StatusPlanningWaiting))
		case e.Finished:
			cmd = tea.Batch(cmd, m.AgentPane.SetStatus(event.StatusFinished))
		default:
			cmd = tea.Batch(cmd, m.AgentPane.SetStatus(event.StatusWaiting))
		}
		m.AgentPane.SetInputActive(true)
		m.AgentPane.ResetInput()
		// Agent may have published /project.md — reload async to stay in sync.
		cmd = tea.Batch(cmd, m.reloadWorkTreeCmd())
	case event.AgentDone:
		cmd = tea.Batch(cmd, m.AgentPane.SetStatus(event.StatusIdle))
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
		m.AgentPane.AppendTurnUsage(e)
		m.AgentPane.UpdateUsage(e)
	case event.AgentCompacted:
		m.AgentPane.AppendMeta(formatCompacted(e))
	}
	return cmd
}

// Validator-gate predicates and banner formatters
// (summaryRequiresReview, summaryHasBlock, snoozeBannerForSummaries,
// yoloOverrideBannerForSummaries, reviewBannerForSummaries,
// joinNonPassVerdicts) live in validator_gate.go.
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

// rebuildEditorModel creates a new EditorModel wrapping the pooled
// editor for the session's currently active open file. Cursor/scroll
// state is preserved because [editorForOpenFile] returns the same
// editor instance across calls for a given path.
func (m *AppModel) rebuildEditorModel() {
	ed := m.activeEditor()
	if ed == nil {
		ed = editor.New(buffer.New())
	}
	m.Editor = NewEditorModel(ed, m.Keymap, m.Services)
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
//
// Implementation lives in app_approval.go.
