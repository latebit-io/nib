package agent

import (
	"context"
	"fmt"

	"github.com/latebit-io/nib/coding/memory"
)

// Agent-internal gates and intent helpers.
//
// recordEdit + maxValidatorRetries are the bookkeeping the
// [coding/approval.Orchestrator] callbacks into when an edit is
// approved (append to taskEdits for end-of-task review, clear the
// per-path retry counter). enforceActiveTaskGate is the dispatch-
// time check that mutating tools require an active `[>]` task in
// /project.md. intentReminder appends the developer's current
// intent to every tool result. fetchMemorySummary re-reads the
// project memory snapshot at run start.
//
// These all read or mutate Agent state under [Agent.mu]. They live
// here together because each represents an "agent-side policy"
// surface — gates and bookkeeping — distinct from the run loop and
// turn pipeline. Pure helpers in [coding/nudges] / [coding/budget]
// / etc. own the math and pattern matching; this file glues them
// to agent state.

// recordEdit tracks an approved edit for end-of-turn review. Also
// resets the validator retry budget for this path — an approval (even
// if the developer modified the proposal) means the latest version
// landed, so subsequent edits to the same file start with a fresh
// budget rather than inheriting the previous churn.
func (a *Agent) recordEdit(proposal EditProposal) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.taskEdits = append(a.taskEdits, taskEdit{
		Path:    proposal.Path,
		Search:  proposal.Edit.Search,
		Replace: proposal.Edit.Replace,
	})
	delete(a.validatorRetries, proposal.CanonPath)
}

// maxValidatorRetries caps how many times the agent will silently
// regenerate a proposal that tripped a validator's Retry verdict before
// surfacing the bad version to the developer. Matches tool_edit_file.go's
// search-validation retry budget so the two layers compose predictably.
const maxValidatorRetries = 3

// fetchMemorySummary re-fetches /summary.md from the memory store so each
// conversation sees the latest state. Thin wrapper around
// [memory.FetchSummary]; the result is returned (not stored on the struct)
// to avoid data races between concurrent run() goroutines.
func (a *Agent) fetchMemorySummary(ctx context.Context) string {
	return memory.FetchSummary(ctx, a.memoryStore, a.memorySummary)
}

// enforceActiveTaskGate blocks mutating tools in execution mode when the
// session's work tree has no active `[>]` task. Returns empty string
// when the call may proceed, or a user-facing error directing the agent
// to activate or add a task.
//
// The gate reads session-cached state (via [TaskReader]) rather than
// re-fetching /project.md on every call. This both avoids per-tool-call
// round trips to demarkus and, crucially, breaks the startup deadlock
// where a flaky memory backend leaves the tree unloaded and the task
// tools refusing to operate on nil state.
//
// Cases handled intentionally:
//   - Workspace does not expose [TaskReader] → gate off.
//   - Planning mode → gate off (planning-mode blocklist already rejects these).
//   - Non-mutating tool → gate off (reads and LSP queries are free).
//   - Work tree not loaded (no /project.md, or startup fetch failed) →
//     gate off. Matches the original fresh-project onboarding carve-out
//     and prevents an unreachable demarkus from trapping the agent.
//     The TUI periodically reloads the tree; once it is populated the
//     gate re-engages automatically.
//   - Work tree loaded, no active task → blocked with guidance.
//   - Work tree loaded, active task present → proceed.
func (a *Agent) enforceActiveTaskGate(_ context.Context, toolName string) string {
	if a.mode == ModePlanning {
		return ""
	}
	if !mutatingTools[toolName] {
		return ""
	}
	tt, ok := a.workspace.(TaskReader)
	if !ok {
		return ""
	}
	if !tt.WorkTreeLoaded() {
		return ""
	}
	if tt.ActiveTaskPath() != "" {
		return ""
	}
	return fmt.Sprintf(
		"Error: tool %q blocked — no active task in /project.md. "+
			"Call update_task with action=\"activate\" (on an existing task) or project_task_add "+
			"(to create one) first, then retry.",
		toolName,
	)
}

// intentReminder returns a string reminding the LLM of the current intent.
// Appended to tool results so the LLM sees it every turn.
func (a *Agent) intentReminder() string {
	a.mu.Lock()
	intent := a.intent
	a.mu.Unlock()
	if intent == "" {
		return ""
	}
	return "\n\nReminder — developer's intent: " + intent
}
