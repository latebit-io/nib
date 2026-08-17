package ui

import (
	tea "charm.land/bubbletea/v2"
)

// Agent-pane goal-submission handlers.
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
	if !m.Session.HasAgent() {
		m.noteNoAgent()
		return m, nil
	}
	// Reset the per-run spend counter on every submission so the budget
	// indicator tracks the same per-run scope the agent's budget gate
	// uses (the gate resets on both RunWithMode and Reply). The fresh
	// path's Clear() below also zeroes it; the continued path relies on
	// this call since it preserves the transcript and totals.
	m.AgentPane.BeginRun()
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
	if !m.Session.HasAgent() {
		m.noteNoAgent()
		return m, nil
	}
	m.AgentPane.BeginRun()
	m.Session.SubmitPlanningGoal(msg.Goal)
	m.AgentPane.Clear()
	return m, nil
}

// noAgentNotice is the inline feedback for a goal submitted with no LLM
// provider configured. Session.SubmitGoal would silently no-op; surfacing
// the reason and the recovery action keeps Enter from dropping on the floor.
const noAgentNotice = "[not sent: no LLM configured. Alt+M to pick a provider or enter an API key, or set LLM_API_KEY]\n"

// noteNoAgent surfaces noAgentNotice in the agent pane instead of handing
// the goal to a session that has no agent. The transcript is left intact
// (no BeginRun / Clear) so the notice stays visible in the no-agent splash.
func (m *AppModel) noteNoAgent() {
	m.AgentPane.AppendMeta(noAgentNotice)
}
