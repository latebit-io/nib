package ui

import (
	"log/slog"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/latebit-io/nib/coding/event"
)

// Edit-approval pipeline.
// Extracted from app.go (Phase 4 of AppModel decomposition).

// applyApproval runs the prepare → apply pipeline and signals the agent
// via CompleteApproval. The session's PrepareApproval validates the
// reviewed edit, the editor applies it atomically, and CompleteApproval
// unblocks the agent so it proceeds without an additional developer step.
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

	// ApplyEdit short-circuits on LocateEdit failure without mutating
	// the buffer. On failure we MUST clear the overlay: PrepareApproval
	// already cleared session.pendingEdit, so a developer pressing Esc
	// at this point hits the "No pending edit" branch of
	// ActionAgentReject and cancels the entire agent instead of
	// dismissing the dead diff.
	ok, reason := m.Editor.ApplyEdit(plan.Search, plan.Replace, plan.LineOrigins)
	if !ok {
		slog.Warn("apply failed", "reason", reason)
		m.AgentPane.AppendText("\n[apply failed: " + reason + "]\n")
		m.Session.AbortApproval()
		m.clearEditorOverlay(false)
		return nil
	}

	// bufferMutated=true: ApplyEdit already replaced the lines, so CollapseOverlay
	// must use the post-mutation coordinate translation (subtract removedCount,
	// not addedCount) to keep the viewport pointing at the right buffer line.
	m.clearEditorOverlay(true)
	m.AgentPane.AppendMeta("[applied]\n")

	// Refresh after CompleteApproval — that's when modifiedFiles is populated,
	// which the project pane reads to render the modified badge.
	m.Session.CompleteApproval()
	statusCmd := m.AgentPane.SetStatus(event.StatusThinking)
	m.refreshProjectPane()
	return statusCmd
}
