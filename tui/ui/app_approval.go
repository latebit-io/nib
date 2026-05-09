package ui

import (
	"log/slog"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/latebit-io/nib/coding/event"
)

// Edit-approval pipeline.
// Extracted from app.go (Phase 4 of AppModel decomposition).
//
// Approval has two responsibilities the original implementation tangled
// together: running the prepare → apply pipeline, and reconciling the
// "snooze this path from future block surfaces" decision. They are split
// here so that the snooze invariant (only outcomeSucceeded promotes a
// pending block) is unit-testable in `snooze_guard_test.go` without
// standing up the full Session/Editor stack — the original bug was a
// state-ordering one, and its test should not require running the
// pipeline at all.

// approvalOutcome enumerates how applyApproval finished. The caller
// uses it to decide pending-block snoozing — promotion to
// blockedPaths happens only on outcomeSucceeded; both terminal
// failure paths drop the pending snapshot; the retryable preparation
// failure leaves it in place so a follow-up press can still snooze.
//
// Modelling the three branches as data (not three call sites that
// each remember to call the snooze helper) is what fixes the original
// regression: a future edit to applyApproval can't accidentally
// reach the success suffix without also producing outcomeSucceeded,
// and the snooze helper has exactly one caller.
type approvalOutcome int

const (
	// outcomeSucceeded: PrepareApproval+ApplyEdit both landed. Snooze.
	outcomeSucceeded approvalOutcome = iota
	// outcomePreparationFailedTerminal: PrepareApproval failed AND the
	// session has no PendingEdit anymore (agent gave up on this edit).
	// No retry possible; drop the pending snapshot.
	outcomePreparationFailedTerminal
	// outcomePreparationFailedRetryable: PrepareApproval failed but
	// PendingEdit is still set. Developer can retry; keep the pending
	// snapshot so a successful retry can still snooze.
	outcomePreparationFailedRetryable
	// outcomeApplyFailed: ApplyEdit rejected the patch; AbortApproval
	// cleared PendingEdit. Drop the pending snapshot.
	outcomeApplyFailed
)

// applyApproval validates the reviewed edit and applies it
// atomically; CompleteApproval signals the agent which proceeds
// without an additional developer step.
//
// The flow splits cleanly: tryApplyApproval runs the prepare → apply
// pipeline and returns an [approvalOutcome];
// reconcilePendingBlockOnOutcome decides snoozing based on that
// outcome. Centralising the snooze decision in one
// data-driven helper is what protects the
// "snooze only on confirmed approval" invariant — see the type
// comment on [approvalOutcome] for why.
func (m *AppModel) applyApproval() tea.Cmd {
	blockedPath := m.pendingBlockedPath
	outcome, cmd := m.tryApplyApproval()
	m.reconcilePendingBlockOnOutcome(blockedPath, outcome)
	return cmd
}

// tryApplyApproval runs the prepare → apply pipeline and signals the
// agent via CompleteApproval. Returns the outcome plus any tea.Cmd
// the success branch produced. It does NOT touch pendingBlockedPath /
// blockedPaths — that reconciliation is the caller's responsibility,
// see [reconcilePendingBlockOnOutcome].
func (m *AppModel) tryApplyApproval() (approvalOutcome, tea.Cmd) {
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
			return outcomePreparationFailedTerminal, nil
		}
		return outcomePreparationFailedRetryable, nil
	}

	// ApplyEdit short-circuits on LocateEdit failure without mutating
	// the buffer. On failure we MUST clear the overlay: PrepareApproval
	// already cleared session.pendingEdit, so a developer pressing Esc
	// at this point hits the "No pending edit" branch of
	// ActionAgentReject and cancels the entire agent instead of
	// dismissing the dead diff. Leaving the overlay visible (the
	// previous design intent) made sense only when a retry path
	// existed; AbortApproval below now closes that off, so the overlay
	// has no purpose post-failure.
	ok, reason := m.Editor.ApplyEdit(plan.Search, plan.Replace, plan.LineOrigins)
	if !ok {
		slog.Warn("apply failed", "reason", reason)
		m.AgentPane.AppendText("\n[apply failed: " + reason + "]\n")
		m.Session.AbortApproval()
		m.clearEditorOverlay(false)
		return outcomeApplyFailed, nil
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
	return outcomeSucceeded, statusCmd
}

// reconcilePendingBlockOnOutcome promotes (or drops) the pending
// blocked-path snapshot based on what tryApplyApproval reported.
//
// Snooze rule: only outcomeSucceeded promotes blockedPath into
// blockedPaths. The two terminal failure outcomes drop the snapshot
// so a fresh proposal must re-trigger the surface; the retryable
// outcome leaves the snapshot alone so the developer's retry can
// still snooze on success.
//
// Pure with respect to AppModel — only mutates blockedPaths and
// pendingBlockedPath — so the snooze invariant is unit-testable
// without standing up the full Session/Editor stack. This is
// deliberate: the original bug was a state-ordering one (snooze
// promoted at keypress before the pipeline ran), and the test for
// it should not depend on running the pipeline at all.
func (m *AppModel) reconcilePendingBlockOnOutcome(blockedPath string, outcome approvalOutcome) {
	switch outcome {
	case outcomeSucceeded:
		if blockedPath != "" {
			m.blockedPaths[blockedPath] = true
			m.pendingBlockedPath = ""
		}
	case outcomePreparationFailedTerminal, outcomeApplyFailed:
		m.pendingBlockedPath = ""
	case outcomePreparationFailedRetryable:
		// Leave pendingBlockedPath in place — a successful retry
		// of the same proposal should still be able to snooze.
	}
}
