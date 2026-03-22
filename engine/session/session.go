// Package session orchestrates the developer-agent collaboration workflow.
// It owns the intent lifecycle, pending edit state, and the approve/reject/continue
// flow — domain logic that every frontend must enforce identically.
package session

import (
	"github.com/latebit-io/junto/engine/agent"
	"github.com/latebit-io/junto/engine/editor"
)

// Session coordinates the interaction between the developer and agent.
// Frontends read its state to render and call its methods to drive the workflow.
type Session struct {
	Editor *editor.Editor
	agent  *agent.Agent
	Events <-chan agent.Event // frontend reads agent events from here

	// Intent — session-level contract between developer and agent
	CurrentIntent string   // the active goal
	IntentDone    bool     // true when intent was completed (not cleared)
	IntentHistory []string // resolved intents archived in order

	// PendingEdit is the edit currently awaiting approval (nil = none)
	PendingEdit *agent.PendingEdit

	// editReviewed is set by ReviewEdit. ApproveEdit requires it.
	// This enforces the contract: every frontend must compute and present
	// the diff before approving — no blind approvals.
	editReviewed bool
}

// New creates a session. Pass nil agent and events for editor-only mode.
func New(e *editor.Editor, ag *agent.Agent, events <-chan agent.Event) *Session {
	return &Session{
		Editor: e,
		agent:  ag,
		Events: events,
	}
}

// HasAgent returns true if the session has an active agent.
func (s *Session) HasAgent() bool {
	return s.agent != nil
}

// --- Intent Lifecycle ---

// SubmitGoal starts the agent with a new goal.
// Archives any previous intent that was still active.
func (s *Session) SubmitGoal(goal string) {
	if !s.HasAgent() {
		return
	}
	// Archive previous intent if redirecting mid-run
	if s.CurrentIntent != "" && !s.IntentDone {
		s.ArchiveIntent()
	}
	// Clear stale pending edit from previous run — Agent.Run cancels the
	// prior run internally, so any pending approval is no longer valid.
	s.PendingEdit = nil
	s.editReviewed = false
	s.CurrentIntent = goal
	s.IntentDone = false
	s.agent.Run(s.Editor.Buf.Path, s.Editor.Buf.Content(), goal)
}

// ArchiveIntent marks the current intent as done.
// The intent stays visible as completed until the developer starts a new one.
func (s *Session) ArchiveIntent() {
	if s.CurrentIntent != "" {
		s.IntentDone = true
		s.IntentHistory = append(s.IntentHistory, s.CurrentIntent)
	}
}

// ClearIntent cancels the current intent without archiving.
// Used when the developer explicitly escapes/deletes the intent.
func (s *Session) ClearIntent() {
	s.CurrentIntent = ""
	s.IntentDone = false
}

// CancelAgent cancels the current agent run, clears intent, and resets pending edit.
func (s *Session) CancelAgent() {
	if s.HasAgent() {
		s.ClearIntent()
		s.agent.Cancel()
		s.PendingEdit = nil
		s.editReviewed = false
	}
}

// --- Edit Approval Flow ---
//
// The engine enforces a two-step review contract:
//
//   1. ReviewEdit  — frontend computes and presents the diff to the developer.
//   2. ApproveEdit — frontend passes the (possibly modified) replacement text.
//
// ApproveEdit fails if ReviewEdit was not called first. This guarantees that
// every frontend — TUI, GUI, web — shows the developer what the agent proposes
// before anything is applied. No blind approvals.

// ReviewEdit computes the diff for the pending edit and marks it as reviewed.
// Frontends MUST call this and present the result before calling ApproveEdit.
// Returns nil if there is no pending edit or the search text has no unique match.
func (s *Session) ReviewEdit() *editor.DiffResult {
	if s.PendingEdit == nil {
		return nil
	}
	diff := s.Editor.ComputeDiff(s.PendingEdit.Search, s.PendingEdit.Replace)
	if diff != nil {
		s.editReviewed = true
	}
	return diff
}

// ApproveEdit applies the reviewed edit to the editor buffer.
// The frontend must provide the final search and replace text — typically
// the full affected lines from the diff, with the replacement possibly
// modified by the developer.
//
// Returns (true, "") on success, or (false, reason) on failure.
// Fails if ReviewEdit was not called first.
func (s *Session) ApproveEdit(search, replace string) (bool, string) {
	if s.PendingEdit == nil || !s.HasAgent() {
		return false, "no pending edit"
	}
	if !s.editReviewed {
		return false, "edit not reviewed — call ReviewEdit first"
	}
	ok, reason := s.Editor.ApplyEdit(search, replace)
	if ok {
		s.agent.Approve()
	} else {
		s.agent.Reject()
	}
	s.PendingEdit = nil
	s.editReviewed = false
	return ok, reason
}

// RejectEdit rejects the pending edit and signals the agent.
func (s *Session) RejectEdit() {
	if s.PendingEdit == nil || !s.HasAgent() {
		return
	}
	s.PendingEdit = nil
	s.editReviewed = false
	s.agent.Reject()
}

// Continue signals the agent to proceed after the developer has finished editing.
func (s *Session) Continue() {
	if s.HasAgent() {
		s.agent.Continue(s.Editor.Buf.Content())
	}
}

// --- Agent Event Handling ---

// HandleEvent processes an agent event and updates session state.
// Returns the event for the frontend to render.
func (s *Session) HandleEvent(ev agent.Event) {
	switch e := ev.(type) {
	case agent.EditProposedEvent:
		s.PendingEdit = &e.Edit
		s.editReviewed = false
	case agent.ErrorEvent:
		s.PendingEdit = nil
		s.editReviewed = false
		_ = e // error text is in the event for the frontend to display
	case agent.DoneEvent:
		s.PendingEdit = nil
		s.editReviewed = false
		if e.Success {
			s.ArchiveIntent()
		}
	case agent.TokenEvent, agent.StatusEvent:
		// No session state changes — frontend renders these directly
	}
}
