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
func (a *Agent) Propose(ctx context.Context, p tools.EditProposal) (string, bool) {
	body := a.handleEditProposal(ctx, p)
	// handleEditProposal returns "Error: …" prefixed strings only on
	// fatal delivery / cancellation failures. Rejection notes and
	// recalibration messages also start with "Error" sometimes (legacy
	// strings); we only flag the unrecoverable cases here so frontends
	// don't treat normal rejection as a tool error.
	switch body {
	case "Error: agent canceled":
		return body, true
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
