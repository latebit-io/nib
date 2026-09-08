package ui

import (
	"log/slog"

	tea "charm.land/bubbletea/v2"
	"github.com/latebit-io/nib/coding/session"
)

// Work-tree refresh handler.
//
// The work tree is the project's plan/goal/task graph fetched from the
// demarkus memory server. It is loaded asynchronously to keep the TUI
// responsive: a tea.Cmd issues the fetch on a background goroutine, and
// the result is applied to session state on the TUI goroutine via
// reloadWorkTreeResultMsg, avoiding data races on workTree/workTreeVer.
//
// Developer-driven activate/complete handlers were removed: the project
// pane is a read view, and /project.md mutations belong to the agent
// (via update_task or the bundled activate_task / complete_task tool
// fields).

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
