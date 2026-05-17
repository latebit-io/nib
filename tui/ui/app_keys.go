package ui

import (
	"errors"
	"log/slog"
	"path/filepath"

	tea "charm.land/bubbletea/v2"
	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/engine/filelist"
)

// Keypress dispatch helpers.
// Extracted from app.go's handleKey() (Phase 7 of AppModel decomposition).
//
// `handleKey` itself stays in app.go as the top-level router (mouse-leak
// guard → input-mode short-circuits → action router → focused-pane
// fallback). The two big chunks live here:
//
//   - handleAgentInputKey: while the agent input is focused, only a
//     small set of global shortcuts is intercepted; everything else is
//     forwarded to the pane so it lands as character input.
//   - handleGlobalAction:  the cross-pane action switch. Returns a
//     `handled` bool so the caller can decide whether to fall through
//     to focused-pane delegation. ActionAgentReject's "no overlay, no
//     intent" branch is the load-bearing case for the fallthrough — a
//     reject keypress with nothing to reject acts like a normal input
//     and reaches the focused pane.

// handleAgentInputKey routes a keypress while the agent input is focused.
// A small allow-list of UI shortcuts (model selector, terse toggle) is
// intercepted; everything else flows to the agent pane as input.
// Caller invokes only when AgentPane.IsInputActive() is true.
func (m *AppModel) handleAgentInputKey(msg tea.KeyPressMsg) tea.Cmd {
	switch m.Keymap.Match(msg) {
	case ActionModelSelector:
		return m.openModelSelector()
	case ActionTerseToggle:
		m.toggleTerse()
		return nil
	}
	return m.AgentPane.Update(msg)
}

// handleGlobalAction routes a global / cross-pane keymap action. Returns
// `handled=true` if the action consumed the keypress; `handled=false`
// means the caller should fall through to focused-pane delegation.
//
// The fallthrough exists for ActionAgentReject's "no overlay, no
// intent" branch: a reject keypress with nothing to reject should act
// like a normal input and reach the focused pane. Unmatched actions
// (the keymap returned an action this switch doesn't recognise) also
// fall through — same delegation semantics.
func (m *AppModel) handleGlobalAction(action Action) (tea.Cmd, bool) {
	switch action {
	case ActionQuit:
		m.Quit = true
		return tea.Quit, true

	case ActionAgentApprove:
		slog.Debug("agent approve", "pending", m.Session.PendingEdit() != nil, "agent", m.Session.HasAgent())
		if m.Session.PendingEdit() != nil && m.Editor.Overlay != nil {
			// Block snooze (pendingBlockedPath → blockedPaths) is
			// handled inside applyApproval after ApplyEdit lands —
			// snoozing here would silently disable future Block
			// review on this path even when the approval failed.
			return m.applyApproval(), true
		}
		return nil, true

	case ActionAgentReject:
		// Completion popup gets priority — dismiss it first.
		if m.Editor.Completion.Active {
			m.Editor.Completion.Dismiss()
			return nil, true
		}
		if m.Session.PendingEdit() != nil {
			slog.Debug("overlay cleared", "reason", "reject")
			// Reject means "this specific edit is wrong" not "stop
			// prompting me on this file" — clear the pending Block
			// path WITHOUT promoting it to snoozed.
			m.pendingBlockedPath = ""
			m.clearEditorOverlay(false)
			m.Session.RejectEdit("user")
			return nil, true
		}
		if m.Session.CurrentIntent() != "" && m.Session.HasAgent() {
			m.Session.CancelAgent()
			return m.AgentPane.SetStatus(event.StatusIdle), true
		}
		// No overlay, no intent — fall through to focused pane.
		return nil, false

	case ActionTerseToggle:
		m.toggleTerse()
		return nil, true

	case ActionModelSelector:
		return m.openModelSelector(), true

	case ActionAgentStart:
		if m.Session.HasAgent() {
			m.AgentPane.SetInputActive(true)
			m.AgentPane.SetPlanningMode(false)
		}
		return nil, true

	case ActionAgentPlan:
		if m.Session.HasAgent() {
			m.AgentPane.SetInputActive(true)
			m.AgentPane.SetPlanningMode(true)
		}
		return nil, true

	case ActionToggleProject:
		_, cmd := m.handleToggleProject()
		return cmd, true

	case ActionReloadFile:
		_, cmd := m.reloadActiveFile()
		return cmd, true

	case ActionNextBuffer:
		_, cmd := m.switchBuffer(1)
		return cmd, true

	case ActionPrevBuffer:
		_, cmd := m.switchBuffer(-1)
		return cmd, true

	case ActionOpenPalette:
		root := m.Session.ProjectRoot()
		if root == "" {
			return nil, true
		}
		return func() tea.Msg {
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
		}, true

	case ActionGoToDefinition:
		_, cmd := m.handleGoToDefinition()
		return cmd, true

	case ActionGoBack:
		_, cmd := m.handleGoBack()
		return cmd, true

	case ActionHover:
		_, cmd := m.handleHover()
		return cmd, true

	case ActionFindInProject:
		m.SearchOverlay.Open()
		return nil, true

	case ActionFind:
		m.Regions.FocusByName("editor")
		m.Editor.Find.Open(m.Editor.Engine(), false)
		return nil, true

	case ActionFindReplace:
		m.Regions.FocusByName("editor")
		m.Editor.Find.Open(m.Editor.Engine(), true)
		return nil, true

	case ActionHelp:
		m.Help.Open()
		return nil, true

	case ActionFocusProject:
		m.Regions.FocusByName("project")
		return nil, true

	case ActionFocusEditor:
		m.Regions.FocusByName("editor")
		return nil, true

	case ActionFocusAgent:
		m.Regions.FocusByName("agent")
		return nil, true
	}
	// Unmatched action — fall through to focused-pane delegation.
	return nil, false
}
