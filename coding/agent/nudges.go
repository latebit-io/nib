package agent

import (
	"github.com/latebit-io/junto/ai/llm"
	"github.com/latebit-io/junto/coding/nudges"
	"github.com/latebit-io/junto/engine/event"
)

// Post-turn nudge integration for Agent.
//
// The agent emits two one-shot nudges per developer input:
//
//  1. Narrative gate — the assistant declared "all done" while
//     enumerating outstanding work AND the tracked task tree is
//     empty. Catches the failure mode where the model wraps up
//     prematurely on a missing/unloaded plan.
//  2. Permission gate (autonomous mode only) — the assistant ended
//     a turn with a permission-seeking question instead of a tool
//     call. Under autonomous operation that's a stall the agent
//     should turn back into action.
//
// Both gates use string-pattern detectors that live in
// [coding/nudges]; the agent-side glue here owns the message-slice
// mutation and the once-per-input guard.

// tryInjectPostTurnNudge runs the post-turn nudge gates (narrative,
// permission) in order and returns true when one fires so the caller
// should `continue` the run loop. The narrative gate flags wrap-ups that
// enumerate outstanding work while the task tree is empty. The
// permission gate (autonomous mode only) flags turns that yielded with a
// question instead of a tool call.
//
// Each gate is one-shot per developer input — narrativeFired and
// permissionFired are mutated in place when their gate fires. Both
// gates skip on error turns; the caller is responsible for that check
// so a turn whose model errored does not consume a one-shot slot for
// the next clean turn.
func (a *Agent) tryInjectPostTurnNudge(messages *[]llm.Message, narrativeFired, permissionFired *bool) bool {
	if !*narrativeFired && a.shouldNudgeOutstanding(*messages) {
		*narrativeFired = true
		*messages = append(*messages, llm.Message{
			Role:    "user",
			Content: nudges.OutstandingNudgeMessage,
		})
		a.send(event.AgentToken{Text: "\n[Nudge: outstanding-work language detected — track or scrub it]\n"})
		a.send(event.AgentStatus{Status: event.StatusThinking})
		return true
	}
	if !*permissionFired && a.currentAutonomous() && nudges.ShouldNudgePermissionQuestion(*messages) {
		*permissionFired = true
		*messages = append(*messages, llm.Message{
			Role:    "user",
			Content: nudges.PermissionNudgeMessage,
		})
		a.send(event.AgentToken{Text: "\n[Nudge: permission-seeking question detected in autonomous mode — act, don't ask]\n"})
		a.send(event.AgentStatus{Status: event.StatusThinking})
		return true
	}
	return false
}

// shouldNudgeOutstanding reports whether the most recent assistant message
// enumerates outstanding work AND the tracked task tree is empty — the
// failure mode where the model declares "all done" while listing items
// that still need doing. Returns false when no TaskReader is wired (no
// project plan = nothing to compare narrative against).
func (a *Agent) shouldNudgeOutstanding(messages []llm.Message) bool {
	if !a.tasksAllComplete() {
		return false
	}
	last := nudges.LastAssistantContent(messages)
	if last == "" {
		return false
	}
	return nudges.ContainsOutstandingWorkMarker(last)
}

// tasksAllComplete reports whether the workspace exposes a loaded task
// tree with no in-progress and no pending work. Returns false when:
//
//   - the workspace is not a TaskReader — no plan to compare against;
//   - the work tree has not loaded yet — empty pending != complete, it's
//     "unknown", and reading "unknown" as "done" would fire the Finished
//     signal and the narrative gate before the developer's plan is even
//     present;
//   - an active [>] task exists — FindNextPendingTask only walks [ ]
//     tasks, so a tree with one active and zero pending leaves reads
//     as empty here. Without the active-task check the agent declares
//     completion mid-work and the DONE chip lights up while it's still
//     mid-task.
func (a *Agent) tasksAllComplete() bool {
	tt, ok := a.workspace.(TaskReader)
	if !ok || !tt.WorkTreeLoaded() {
		return false
	}
	return tt.ActiveTaskPath() == "" && tt.NextPendingTask() == ""
}
