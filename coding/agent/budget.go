package agent

import (
	upevent "github.com/latebit-io/nib/agent/event"
	"github.com/latebit-io/nib/coding/budget"
	"github.com/latebit-io/nib/coding/event"
)

// Per-run budget integration for Agent.
//
// The pure budget math + types live in [coding/budget]; this file is
// the agent-side glue: per-run accounting (recordTurnUsage), the
// post-Stream latch ([Agent.checkTaskBudget]), and the abort path
// fired from the foundation TransformContext hook
// ([Agent.foundationBudgetCheck] in foundation_hooks.go). All four
// touch [Agent.sessionUsage] / [Agent.budgetExceeded] under
// [Agent.mu]; ordering between read and write is documented at each
// call site.
//
// The runID staleness gate the inline path used (decision #19) was
// retired alongside the loop swap: the foundation owns run lifecycle,
// the wrapper waits for [upagent.Agent.WaitForIdle] before starting a
// new run, so a stale goroutine cannot accumulate against the new
// run's totals.

// recordTurnUsage accumulates one foundation [upevent.TurnUsage] event
// into the session total and emits the engine-level
// [event.AgentTurnUsage] for the frontend's status bar / cost panel.
// completionEst is the streamed assistant content estimate captured
// by the translator on [upevent.MessageEnd] — provider-reported
// counts (PromptTokens, CompletionTokens, CachedTokens, ToolCalls)
// arrive on the TurnUsage event; the *Est fields are the
// pre-Stream estimate stash from TransformContext combined with this
// post-Stream content estimate.
func (a *Agent) recordTurnUsage(tu upevent.TurnUsage, completionEst int) {
	a.mu.Lock()
	a.turnCounter++
	turn := a.turnCounter
	a.sessionUsage.TotalPromptTokens += tu.PromptTokens
	a.sessionUsage.TotalCompletionTokens += tu.CompletionTokens
	a.sessionUsage.TotalCachedTokens += tu.CachedTokens
	a.sessionUsage.Turns = turn
	estimate := a.lastEstimate
	a.mu.Unlock()

	a.send(event.AgentTurnUsage{
		Turn:             turn,
		PromptTokens:     tu.PromptTokens,
		CompletionTokens: tu.CompletionTokens,
		CachedTokens:     tu.CachedTokens,
		ToolCalls:        tu.ToolCalls,
		SystemEst:        estimate.System,
		ToolsEst:         estimate.Tools,
		HistoryEst:       estimate.History,
		NewEst:           estimate.New,
		CompletionEst:    completionEst,
	})
}

// checkTaskBudget reports whether the per-task token budget has been
// exceeded by the current sessionUsage. Returns the formatted error
// message on overrun (and latches budgetExceeded so subsequent calls
// do not double-emit) or "" otherwise. A zero budget disables the
// check.
//
// Sums prompt + completion tokens. Cached tokens are already a subset
// of prompt and would double-count if added separately. Called from
// the foundation TransformContext hook
// ([Agent.foundationBudgetCheck]) immediately before each Stream so a
// runaway turn is caught before the next LLM call commits more
// tokens.
func (a *Agent) checkTaskBudget() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.budgetExceeded {
		return ""
	}
	msg, exceeded := budget.Exceeded(a.sessionUsage, a.taskTokenBudget)
	if !exceeded {
		return ""
	}
	a.budgetExceeded = true
	return msg
}
