package agent

import (
	"context"
	"strings"

	"github.com/latebit-io/junto/coding/tools"
	"github.com/latebit-io/junto/engine/event"
)

// fatalProposalMarkers are the unrecoverable outcomes [handleEditProposal]
// can produce. Matched as substrings so the IsError flag stays accurate
// even if the upstream message gains a wrapped error suffix. Rejection
// notes ("The developer rejected this edit…") and validator
// recalibration are normal flow and intentionally NOT listed here.
var fatalProposalMarkers = []string{
	"Error: agent canceled",
	"Error: could not deliver edit proposal to frontend",
	"Error: continue channel closed",
	"Error: approval channel closed",
}

// Propose satisfies [tools.Approver]. Tools in coding/tools call this
// when they want an edit reviewed; the body returned is fed to the LLM
// as the tool result. The bool reports whether the call should be
// surfaced as a tool error to the frontend (only set on delivery
// failures and cancellation; rejection feedback and recalibration
// notes are normal flow).
//
// The run's [*approval.Coordinator] is recovered from ctx (it was
// stashed by RunWithMode/Reply via [ctxWithCoord]). A nil coord
// means the tool is being invoked outside an active run — a
// programming error worth surfacing as a tool error rather than
// silently routing through [Agent.coord], which the run-handoff
// race may have swapped to a different run's channels.
func (a *Agent) Propose(ctx context.Context, p tools.EditProposal) (string, bool) {
	coord := coordFromCtx(ctx)
	if coord == nil {
		return "Error: edit proposal received without a run-scoped approval coordinator (no active run?)", true
	}
	body := a.handleEditProposal(ctx, coord, p)
	for _, marker := range fatalProposalMarkers {
		if strings.Contains(body, marker) {
			return body, true
		}
	}
	return body, false
}

// Navigate satisfies [tools.Navigator]. The go_to_line tool calls this
// to point the editor at a specific file/line.
func (a *Agent) Navigate(_ context.Context, nav event.AgentNavigate) {
	a.send(nav)
}

// FileCreated satisfies [tools.FileCreator]. The write_file tool calls
// this after successfully creating a new file.
func (a *Agent) FileCreated(_ context.Context, path string) {
	a.send(event.AgentFileCreated{Path: path})
}

// OnComplete satisfies [tools.TaskReviewer]. The update_task tool
// calls this after marking a task complete; the returned string is
// appended to the tool result so the LLM sees the verdict in-band.
func (a *Agent) OnComplete(ctx context.Context, baseMessage string) string {
	return a.runTaskReview(ctx, baseMessage)
}
