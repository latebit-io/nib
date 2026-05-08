package session

import (
	"context"
	"log/slog"

	"github.com/latebit-io/nib/coding/event"
)

// Intent lifecycle — the session-level contract between developer and agent.
// Submission, planning-phase transitions, archiving, cancellation. State
// (currentIntent, intentDone, intentHistory, phase, pendingEdit, etc.) lives
// on Session in session.go.

// CurrentIntent returns the active goal string.
func (s *Session) CurrentIntent() string {
	return s.currentIntent
}

// IntentDone reports whether the current intent was completed.
func (s *Session) IntentDone() bool {
	return s.intentDone
}

// IntentHistory returns the resolved intents archived in order.
func (s *Session) IntentHistory() []string {
	return s.intentHistory
}

// SubmitGoal sends a message to the agent. If the agent is waiting for input
// (mid-conversation), the message continues the existing conversation.
// Otherwise, a new conversation is started. Returns true when the message
// continued an existing conversation, false when a new one was started.
//
// Must be called from a single goroutine (the TUI main goroutine).
func (s *Session) SubmitGoal(goal string) bool {
	if !s.HasAgent() {
		return false
	}

	s.emitCapture("intent", map[string]any{
		"goal":  goal,
		"phase": phaseName(s.phase),
	})

	// Handle planning phase commands.
	if s.phase == PhasePlanning {
		return s.handlePlanningInput(goal)
	}

	// Continue existing conversation or resume a saved one.
	// Reply handles both: queuing to a running agent, and resuming
	// from saved messages when the run has exited.
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if s.agent.Reply(ctx, goal) {
		return true
	}

	// No conversation to continue or resume — start fresh.
	s.startNewConversation(goal, event.ModeExecution)
	s.phase = PhaseExecution
	return false
}

// SubmitPlanningGoal starts a new conversation in planning mode.
// Write-side tools are disabled; the agent discusses design before coding.
// No-op if no agent is configured.
func (s *Session) SubmitPlanningGoal(goal string) {
	if !s.HasAgent() {
		return
	}

	s.startNewConversation(goal, event.ModePlanning)
	s.phase = PhasePlanning
}

// handlePlanningInput processes input during the planning phase.
// ":done" transitions to execution with the original goal.
// ":skip" transitions to execution immediately.
// Other input continues the planning conversation.
func (s *Session) handlePlanningInput(input string) bool {
	switch input {
	case ":done":
		// Transition to execution — cancel planning, start execution
		// with the original goal. The plan is persisted in demarkus
		// by the planning agent, so the execution agent picks it up
		// via memory summary.
		originalGoal := s.currentIntent
		s.agent.Cancel()
		// Reload work tree — the planning agent may have published
		// or updated /project.md during the conversation.
		if err := s.workTree.Reload(); err != nil {
			slog.Warn("session: reload work tree after planning", "err", err)
		}
		s.startNewConversation(originalGoal, event.ModeExecution)
		s.phase = PhaseExecution
		return false

	case ":skip":
		// Skip planning entirely — start execution with original goal.
		originalGoal := s.currentIntent
		s.agent.Cancel()
		s.startNewConversation(originalGoal, event.ModeExecution)
		s.phase = PhaseExecution
		return false

	default:
		// Continue planning conversation.
		ctx := s.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		return s.agent.Reply(ctx, input)
	}
}

// startNewConversation archives any previous intent, clears stale state,
// and starts a new agent conversation in the specified mode.
func (s *Session) startNewConversation(goal string, mode event.Mode) {
	if s.currentIntent != "" && !s.intentDone {
		s.ArchiveIntent()
	}
	// Clear stale pending edit from previous run — Agent.Run cancels the
	// prior run internally, so any pending approval is no longer valid.
	s.pendingEdit = nil
	s.editReviewed = false
	s.currentIntent = goal
	s.intentDone = false
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	s.agent.RunWithMode(ctx, s.activeFile, s.activeEditor.Buf.Content(), goal, s.ContextFiles(), mode)
}

// ArchiveIntent marks the current intent as done.
// The intent stays visible as completed until the developer starts a new one.
func (s *Session) ArchiveIntent() {
	if s.currentIntent != "" {
		s.intentDone = true
		s.intentHistory = append(s.intentHistory, s.currentIntent)
	}
}

// ClearIntent cancels the current intent without archiving.
// Used when the developer explicitly escapes/deletes the intent.
func (s *Session) ClearIntent() {
	s.currentIntent = ""
	s.intentDone = false
}

// CancelAgent cancels the current agent run, clears intent, and
// resets every approval-flow field so a cancel mid-staged-apply does
// not leave stagedEditFile/pendingApproval latched — those gate
// SwitchTo/ReloadFile/DeleteFile, so leaking them locks the editor
// against further file operations until restart.
func (s *Session) CancelAgent() {
	if s.HasAgent() {
		s.ClearIntent()
		s.agent.Cancel()
		s.pendingEdit = nil
		s.pendingProposedReplace = ""
		s.pendingApproval = nil
		s.stagedEditFile = ""
		s.editReviewed = false
	}
}
