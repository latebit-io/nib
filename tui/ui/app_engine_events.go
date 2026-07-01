package ui

import (
	"log/slog"

	tea "charm.land/bubbletea/v2"
	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/coding/session"
)

// Engine → TUI event bridge.
// Extracted from app.go (Phase 8 of AppModel decomposition).
//
// Pipeline placement: this is the consumer end of the foundation→kit→
// coding→TUI event channel chain (see /nib/architecture.md). Session
// already owns domain state for each event (intent, pending edit,
// usage); this layer is presentation only — agent pane rendering,
// editor overlay lifecycle, file/cursor side effects when the agent
// navigates or creates files, project pane refresh on work-tree
// publishes.
//
// Validator-gate predicates and banner formatters (summaryRequiresReview,
// summaryHasBlock, snooze/yolo/reviewBannerForSummaries) live in
// validator_gate.go.
//
// AgentEditProposed branch carries the most architectural weight — the
// validator-gate auto-approval decision and the per-file Block snooze
// cache wire together here. Inline comments document the invariants.

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
		// the agent's prose. AppendToolCall folds any pending turn-usage
		// stats into the bullet line (Option B layout); subsequent tool
		// bullets in the same turn render unadorned.
		m.AgentPane.AppendToolCall(e.Name)
	case event.SubagentActivity:
		// Nested child progress, rendered indented under the parent as its
		// own sub-pane rather than interleaved as sibling tool bullets.
		m.AgentPane.AppendSubagentActivity(e)
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
			m.AgentPane.AppendProposal(e.Edit.Reason)
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
			// Auto-applying would invert the validator's purpose.
			// Fail-closed on unknown verdicts too — a future verdict
			// must explicitly opt into auto-apply rather than slipping
			// through this gate.
			if !summaryRequiresReview(e.ValidatorSummaries) {
				// Skip the visual review step and apply immediately.
				cmd = m.applyApproval()
			} else {
				m.AgentPane.AppendMeta(reviewBannerForSummaries(e.ValidatorSummaries))
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
			m.AgentPane.AppendError("edit could not be matched — auto-rejecting")
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
			m.AgentPane.AppendError("navigate failed: " + err.Error())
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
		m.AgentPane.AppendError(e.Err)
		m.clearEditorOverlay(false)
	case event.AgentWaiting:
		switch {
		case m.Session.Phase() == session.PhasePlanning:
			cmd = tea.Batch(cmd, m.AgentPane.SetStatus(event.StatusPlanningWaiting))
		case e.Finished:
			cmd = tea.Batch(cmd, m.AgentPane.SetStatus(event.StatusFinished))
		default:
			cmd = tea.Batch(cmd, m.AgentPane.SetStatus(event.StatusWaiting))
			// In terse mode the agent often ends a turn after a tool
			// run with no closing prose, so the transcript trails off
			// into apply_patch / [applied] lines and the user can't
			// tell whether nib is still working or parked for input.
			// The REPLY chip surfaces the state in the bottom-right
			// corner but the eye lives at the transcript tail — drop
			// a dim end-of-turn marker so the parked state is visible
			// where the developer is reading. The marker is part of
			// the current beat and disappears when that beat collapses
			// on the next user message, so it never clutters the
			// long-term history.
			m.AgentPane.AppendMeta("\n↩ awaiting your reply\n")
		}
		// Defensive flush: if a queued submission is still pending here
		// (the tool-less AgentTurnUsage branch should normally have
		// caught it first), commit it now so the banner doesn't outlive
		// the parked run.
		m.AgentPane.FlushPendingUserMessage()
		m.AgentPane.SetInputActive(true)
		m.AgentPane.ResetInput()
		// Agent may have published /project.md — reload async to stay in sync.
		cmd = tea.Batch(cmd, m.reloadWorkTreeCmd())
	case event.AgentDone:
		cmd = tea.Batch(cmd, m.AgentPane.SetStatus(event.StatusIdle))
		// Defensive flush — see AgentWaiting branch. AgentDone shouldn't
		// fire with a pending submission (kit.Reply would have queued
		// the input and the foundation would process it before ending)
		// but flushing here keeps the banner from leaking into the
		// session-summary block in pathological cases.
		m.AgentPane.FlushPendingUserMessage()
		// Flush any stats stashed by the final turn before the session
		// summary so the per-turn line for a tool-less final reply
		// still surfaces (otherwise it would be silently dropped at
		// session end).
		m.AgentPane.FlushPendingTurnUsage()
		summary := formatSessionSummary(m.AgentPane.usage, m.AgentPane.modelLabel)
		if summary != "" {
			m.AgentPane.AppendMeta("\n--- Done ---\n" + summary + "\n")
		} else {
			m.AgentPane.AppendMeta("\n--- Done ---\n")
		}
		m.AgentPane.SetInputActive(false)
		// Agent may have published /project.md — reload async to stay in sync.
		cmd = tea.Batch(cmd, m.reloadWorkTreeCmd())
		m.clearEditorOverlay(false)
	case event.DiagnosticsUpdated:
		m.refreshDiagnostics(e.Path)
	case event.ReloadBuffers:
		m.reloadAllBuffers()
	case event.AgentInputEstimate:
		m.AgentPane.SetStreamingInput(e)
	case event.AgentTurnUsage:
		// AppendTurnUsage stashes for the next bullet AND advances the
		// running totals internally — single call covers both
		// formerly-separate concerns.
		m.AgentPane.AppendTurnUsage(e)
		// A turn that ended with no tool calls is the boundary at which
		// the foundation will park on awaitReply — and pick up any
		// queued user input as the next message. Flushing the pending
		// user-message banner here means "You: …" lands between the
		// settled prior turn and the queued-input-driven new turn, in
		// the right order. Turns with ToolCalls > 0 keep streaming
		// tools and would interrupt mid-task.
		if e.ToolCalls == 0 {
			m.AgentPane.FlushPendingUserMessage()
		}
	case event.AgentCompacted:
		m.AgentPane.AppendMeta(formatCompacted(e))
	}
	return cmd
}
