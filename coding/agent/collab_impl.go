package agent

import (
	"context"

	"github.com/latebit-io/junto/coding/tools"
	"github.com/latebit-io/junto/engine/event"
)

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
// race may have swapped to a different run's channels. The full
// orchestration (validation, proposal delivery, await, recordEdit,
// continue, diagnostics) lives in [approval.Orchestrator]; this
// method is just the agent-side bridge from the tool world to it.
func (a *Agent) Propose(ctx context.Context, p tools.EditProposal) (string, bool) {
	coord := coordFromCtx(ctx)
	if coord == nil {
		return "Error: edit proposal received without a run-scoped approval coordinator (no active run?)", true
	}
	return a.approvalFlow.Handle(ctx, coord, p)
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
