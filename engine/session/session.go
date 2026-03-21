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
	Agent  *agent.Agent
	Events <-chan agent.Event // frontend reads agent events from here

	// Intent — session-level contract between developer and agent
	CurrentIntent string   // the active goal
	IntentDone    bool     // true when intent was completed (not cleared)
	IntentHistory []string // resolved intents archived in order

	// PendingEdit is the edit currently awaiting approval (nil = none)
	PendingEdit *agent.PendingEdit
}

// New creates a session. Pass nil agent and events for editor-only mode.
func New(e *editor.Editor, ag *agent.Agent, events <-chan agent.Event) *Session {
	return &Session{
		Editor: e,
		Agent:  ag,
		Events: events,
	}
}

// HasAgent returns true if the session has an active agent.
func (s *Session) HasAgent() bool {
	return s.Agent != nil
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
	s.CurrentIntent = goal
	s.IntentDone = false
	s.Agent.Run(s.Editor.Buf.Path, s.Editor.Buf.Content(), goal)
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
		s.Agent.Cancel()
		s.PendingEdit = nil
	}
}

// --- Edit Approval Flow ---

// ApproveEdit applies the pending edit to the editor buffer.
// Returns (true, "") on success, or (false, reason) on failure.
// On success, signals the agent that the edit was approved.
// On failure, signals the agent that the edit was rejected.
func (s *Session) ApproveEdit() (bool, string) {
	if s.PendingEdit == nil || !s.HasAgent() {
		return false, "no pending edit"
	}
	ok, reason := s.Editor.ApplyEdit(s.PendingEdit.Search, s.PendingEdit.Replace)
	if ok {
		s.Agent.Approve()
	} else {
		s.Agent.Reject()
	}
	s.PendingEdit = nil
	return ok, reason
}

// RejectEdit rejects the pending edit and signals the agent.
func (s *Session) RejectEdit() {
	if s.PendingEdit == nil || !s.HasAgent() {
		return
	}
	s.PendingEdit = nil
	s.Agent.Reject()
}

// Continue signals the agent to proceed after the developer has finished editing.
func (s *Session) Continue() {
	if s.HasAgent() {
		s.Agent.Continue(s.Editor.Buf.Content())
	}
}

// --- Agent Event Handling ---

// HandleEvent processes an agent event and updates session state.
// Returns the event for the frontend to render.
func (s *Session) HandleEvent(ev agent.Event) {
	switch e := ev.(type) {
	case agent.EditProposedEvent:
		s.PendingEdit = &e.Edit
	case agent.ErrorEvent:
		s.PendingEdit = nil
		_ = e // error text is in the event for the frontend to display
	case agent.DoneEvent:
		s.PendingEdit = nil
		if e.Success {
			s.ArchiveIntent()
		}
	case agent.TokenEvent, agent.StatusEvent:
		// No session state changes — frontend renders these directly
	}
}
