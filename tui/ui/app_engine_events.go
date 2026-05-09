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
