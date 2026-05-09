package ui

import (
	"log/slog"

	tea "charm.land/bubbletea/v2"
	"github.com/latebit-io/nib/coding/session"
)

// Work-tree refresh + goal-state handlers.
// Extracted from app.go's Update() (Phase 3 of AppModel decomposition).
//
// The work tree is the project's plan/goal/task graph fetched from the
// demarkus memory server. It is loaded asynchronously to keep the TUI
// responsive: a tea.Cmd issues the fetch on a background goroutine, and
// the result is applied to session state on the TUI goroutine via
// reloadWorkTreeResultMsg, avoiding data races on workTree/workTreeVer.

// reloadWorkTreeResultMsg delivers an async work-tree fetch result back to
// the TUI goroutine for application via Session.ApplyWorkTreeSnapshot.
type reloadWorkTreeResultMsg struct {
	snap session.WorkTreeSnapshot
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

// handleReloadWorkTreeResult applies a fetched work-tree snapshot to the
// session, surfacing fetch errors in the agent pane, then refreshes the
// project pane to reflect the new state.
func (m *AppModel) handleReloadWorkTreeResult(msg reloadWorkTreeResultMsg) (tea.Model, tea.Cmd) {
	if msg.snap.Err != nil {
		slog.Warn("reload work tree", "err", msg.snap.Err)
		m.AgentPane.AppendMeta("[reload project failed: " + msg.snap.Err.Error() + "]\n")
	} else {
		m.Session.ApplyWorkTreeSnapshot(msg.snap)
	}
	m.refreshProjectPane()
	return m, nil
}

// handleProjectSetActiveGoal sets the active goal on the session and
// surfaces any error in the agent pane. Project pane is refreshed
// regardless so any optimistic UI rolls back on failure.
func (m *AppModel) handleProjectSetActiveGoal(msg ProjectSetActiveGoalMsg) (tea.Model, tea.Cmd) {
	if err := m.Session.SetActiveGoal(msg.Title); err != nil {
		slog.Warn("set active goal", "err", err)
		m.AgentPane.AppendMeta("[set active goal failed: " + err.Error() + "]\n")
	}
	m.refreshProjectPane()
	return m, nil
}

// handleProjectMarkGoalDone marks a goal complete on the session and
// surfaces any error in the agent pane. Project pane is refreshed
// regardless so any optimistic UI rolls back on failure.
func (m *AppModel) handleProjectMarkGoalDone(msg ProjectMarkGoalDoneMsg) (tea.Model, tea.Cmd) {
	if err := m.Session.MarkGoalDone(msg.Title); err != nil {
		slog.Warn("mark goal done", "err", err)
		m.AgentPane.AppendMeta("[mark done failed: " + err.Error() + "]\n")
	}
	m.refreshProjectPane()
	return m, nil
}
