package agent

import (
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/nudges"
)

// Post-turn nudge integration for Agent.
//
// The agent emits two one-shot nudges per developer input:
//
//  1. Narrative gate — the assistant declared "all done" while
//     enumerating outstanding work AND the tracked task tree is
//     empty. Catches the failure mode where the model wraps up
//     prematurely on a missing/unloaded plan.
//  2. Permission gate — the assistant ended a turn with a
//     permission-seeking question instead of a tool call. The agent
//     turns that stall back into action.
//
// Both gates use string-pattern detectors that live in
// [coding/nudges]; the agent-side glue here owns the per-call
// gating math (consulted by [Agent.foundationSteering] in
// foundation_hooks.go).

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
