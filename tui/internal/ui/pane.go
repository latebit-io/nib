package ui

import tea "charm.land/bubbletea/v2"

// Pane is the interface for self-contained UI modules.
// Each pane owns its own Update logic (keys, mouse, domain messages),
// rendering, and size management. AppModel acts as a thin orchestrator
// that handles cross-pane actions and delegates everything else.
//
// To add a new pane: implement this interface, register it with the
// RegionManager, and add cross-pane routing in AppModel if needed.
type Pane interface {
	Update(msg tea.Msg) tea.Cmd
	Render() string
	SetSize(width, height int)
}

// Titled is an optional interface a Pane can implement to display a title
// embedded in its border. RegionManager checks for this via type assertion.
type Titled interface {
	Title() string
}

// GoalSubmittedMsg is emitted by the agent pane when the user submits a goal.
// AppModel catches this and wires up the agent run with editor state.
type GoalSubmittedMsg struct {
	// Goal is the user-entered text describing what the agent should do.
	Goal string
}

// PlanningGoalSubmittedMsg is emitted when the user submits a goal in planning mode.
// AppModel routes this to Session.SubmitPlanningGoal.
type PlanningGoalSubmittedMsg struct {
	// Goal is the user-entered text for the planning-mode goal.
	Goal string
}

// InputAnsweredMsg is emitted by the agent pane when the user answers a
// request_input prompt. AppModel routes Text to Session.AnswerInput, which
// unblocks the agent goroutine waiting on the structured prompt.
type InputAnsweredMsg struct {
	// Text is the developer's verbatim answer — typically an option ID,
	// but free-form text is valid.
	Text string
}

// CancelAgentMsg is emitted by the agent pane when the user presses Esc
// while the agent is awaiting a structured input answer. AppModel routes
// this to Session.CancelAgent — matches the Esc-rejects-edit model.
type CancelAgentMsg struct{}

// clampScrollOffset returns the scroll offset needed to keep selected visible
// within a list that shows maxVisible items at a time.
func clampScrollOffset(selected, current, maxVisible int) int {
	if selected < current {
		return selected
	}
	if selected >= current+maxVisible {
		return selected - maxVisible + 1
	}
	return current
}
