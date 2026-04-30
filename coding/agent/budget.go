package agent

import (
	"log/slog"

	"github.com/latebit-io/junto/coding/budget"
	"github.com/latebit-io/junto/engine/event"
)

// Per-turn budget integration for Agent.
//
// The budget math + types live in [coding/budget]; this file is the
// agent-side glue: per-run accounting (recordTurnUsage), the
// pre-Stream sanity check (shouldAbortForBudget), the post-Stream
// latch (checkTaskBudget), and the abort path that fires when the
// latch trips (abortIfBudgetExceeded). All four read or mutate
// [Agent.sessionUsage] / [Agent.budgetExceeded] under [Agent.mu]
// and gate on runID so late updates from cancelled runs cannot
// pollute a fresh run's totals.

// recordTurnUsage accumulates turn-level usage into the session total and
// sends an AgentTurnUsage event to the frontend. The runID parameter is
// checked against the current run — late updates from canceled runs are
// silently ignored to prevent pollution of the new run's totals.
func (a *Agent) recordTurnUsage(runID uint64, tu budget.Turn) {
	a.mu.Lock()
	if runID != a.runID {
		a.mu.Unlock()
		return
	}
	a.turnCounter++
	turn := a.turnCounter
	a.sessionUsage.TotalPromptTokens += tu.PromptTokens
	a.sessionUsage.TotalCompletionTokens += tu.CompletionTokens
	a.sessionUsage.TotalCachedTokens += tu.CachedTokens
	a.sessionUsage.Turns = turn
	a.mu.Unlock() // safe: runID matched, so this run is still active

	a.send(event.AgentTurnUsage{
		Turn:             turn,
		PromptTokens:     tu.PromptTokens,
		CompletionTokens: tu.CompletionTokens,
		CachedTokens:     tu.CachedTokens,
		ToolCalls:        tu.ToolCalls,
		SystemEst:        tu.LastEstimate.System,
		ToolsEst:         tu.LastEstimate.Tools,
		HistoryEst:       tu.LastEstimate.History,
		NewEst:           tu.LastEstimate.New,
		CompletionEst:    tu.CompletionEst,
	})
}

// shouldAbortForBudget reports whether the task token budget would be
// exceeded if the in-flight turn's pending usage (tu) were committed to
// sessionUsage right now. Used by [Agent.processLLMTurn] BETWEEN inner
// Stream calls so a multi-iteration turn (one Stream call per tool-call
// round-trip) cannot blow past the cap unchecked.
//
// Distinct from [Agent.checkTaskBudget] in two ways:
//
//  1. Reads `tu` (pending) plus `sessionUsage` (committed) so the check
//     reflects state that has not yet been recorded. checkTaskBudget
//     only sees committed totals.
//  2. Does NOT latch `budgetExceeded`. Latching is the abort path's
//     job — checkTaskBudget runs after recordTurnUsage and is the
//     single source of truth for "we have aborted." This helper only
//     decides whether processLLMTurn should stop early; the actual
//     abort still flows through runLoop's [Agent.abortIfBudgetExceeded].
//
// Returns false when the budget is disabled (taskTokenBudget <= 0) or
// already latched (the run is unwinding) so the inner loop does not
// double-fire.
func (a *Agent) shouldAbortForBudget(tu budget.Turn) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.budgetExceeded {
		return false
	}
	return budget.WouldExceed(a.sessionUsage, tu, a.taskTokenBudget)
}

// checkTaskBudget reports whether the per-task token budget has been
// exceeded by the current sessionUsage. Returns the formatted error
// message on overrun (and latches budgetExceeded so it does not double-
// emit) or "" otherwise. A zero budget disables the check.
//
// Sums prompt + completion tokens. Cached tokens are already a subset of
// prompt and would double-count if added separately. Called from runLoop
// immediately after each recordTurnUsage so a runaway turn is caught
// before the next LLM call commits more tokens.
func (a *Agent) checkTaskBudget(runID uint64) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if runID != a.runID || a.budgetExceeded {
		return ""
	}
	msg, exceeded := budget.Exceeded(a.sessionUsage, a.taskTokenBudget)
	if !exceeded {
		return ""
	}
	a.budgetExceeded = true
	return msg
}

// abortIfBudgetExceeded fires the per-task token budget abort when usage
// has crossed the cap. Returns true when the abort fired so the run loop
// should `return` immediately. Aborting cancels the agent (so any
// in-flight tool wait unblocks) and surfaces an AgentError; the deferred
// AgentDone in run/resumeRun then reports success=false. Extracted from
// runLoop to keep the loop's cyclomatic complexity below the package
// threshold.
func (a *Agent) abortIfBudgetExceeded(runID uint64, success *bool) bool {
	msg := a.checkTaskBudget(runID)
	if msg == "" {
		return false
	}
	slog.Warn("agent: task token budget exceeded; aborting", "msg", msg)
	a.send(event.AgentError{Err: msg})
	a.Cancel()
	*success = false
	return true
}
