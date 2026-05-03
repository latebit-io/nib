package agent

import (
	"github.com/latebit-io/nib/kit/budget"
)

// Per-run budget integration for Agent.
//
// The pure budget math + types live in [kit/budget]; this file is
// the agent-side glue: the post-Stream latch ([Agent.checkTaskBudget])
// and the abort path fired from the foundation TransformContext hook
// ([Agent.foundationBudgetCheck] in foundation_hooks.go). Both touch
// [Agent.sessionUsage] / [Agent.budgetExceeded] under [Agent.mu];
// ordering between read and write is documented at each call site.
//
// Per-turn accumulation lives in [Agent.augmentAndAccumulate]
// (forwarder.go) — the post-translation point where each
// [event.AgentTurnUsage] from kit gets folded into [Agent.sessionUsage]
// before reaching the frontend.

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
