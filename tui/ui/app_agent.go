package ui

import (
	tea "charm.land/bubbletea/v2"
)

// Agent-pane goal-submission handlers.
// Extracted from app.go's Update() (Phase 7 of AppModel decomposition).
//
// Two pane → session paths: a normal goal (which may continue an existing
// conversation) and a planning-mode goal (which always starts fresh). The
// pane-clear distinction is load-bearing — appending a follow-up to a
// continued conversation should preserve the transcript, while a fresh
// goal needs the pane reset before the new turn lands.

// handleGoalSubmitted hands a user goal to the session. If the session
// reports the goal as a continuation of an in-flight conversation, the
// goal is appended to the agent pane as a user message; otherwise the
// pane is cleared so the new conversation starts on a clean slate.
//
// When the agent is mid-stream (status is animated — Thinking, Planning,
// or Linting), the message is queued behind a banner instead of being
// stamped immediately. Stamping now would put the "── ♩ beat N ──"
// separator + "You: …" line above the prior beat's still-arriving tail
// tokens, leaving the user's message visually adrift inside the agent's
// previous output. [AgentPaneModel.FlushPendingUserMessage] commits the
// queued text once the first tool-less AgentTurnUsage arrives (the
// natural park boundary the foundation hits before picking up the
// queued input).
func (m *AppModel) handleGoalSubmitted(msg GoalSubmittedMsg) (tea.Model, tea.Cmd) {
	continued := m.Session.SubmitGoal(msg.Goal)
	if !continued {
		m.AgentPane.Clear()
		return m, nil
	}
	if statusAnimates(m.AgentPane.StatusKind()) {
		m.AgentPane.QueueUserMessage(msg.Goal)
	} else {
		m.AgentPane.AppendUserMessage(msg.Goal)
	}
	return m, nil
}

// handlePlanningGoalSubmitted hands a planning-mode goal to the session
// and clears the agent pane. Planning goals always start fresh —
// there's no "continued conversation" branch.
func (m *AppModel) handlePlanningGoalSubmitted(msg PlanningGoalSubmittedMsg) (tea.Model, tea.Cmd) {
	m.Session.SubmitPlanningGoal(msg.Goal)
	m.AgentPane.Clear()
	return m, nil
}
