package agent

import (
	"github.com/latebit-io/nib/coding/event"
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
// exceeded by the current session usage (read from [providerProxy.Snapshot]).
// Returns the formatted error message on overrun (and latches
// budgetExceeded so subsequent calls do not double-emit) or "" otherwise.
// A zero budget disables the check.
//
// Sums prompt + completion tokens. Cached tokens are already a subset
// of prompt and would double-count if added separately. Called from
// the foundation TransformContext hook
// ([Agent.foundationBudgetCheck]) immediately before each Stream so a
// runaway turn is caught before the next LLM call commits more
// tokens.
func (a *Agent) checkTaskBudget() string {
	a.mu.Lock()
	if a.budgetExceeded {
		a.mu.Unlock()
		return ""
	}
	a.mu.Unlock()

	msg, exceeded := budget.Exceeded(a.sessionSnapshot(), a.taskTokenBudget)
	if !exceeded {
		return ""
	}

	a.mu.Lock()
	a.budgetExceeded = true
	a.mu.Unlock()
	return msg
}

// onTurnSettled is the callback providerProxy invokes after emitting
// AgentTurnUsage on a Stream's Done event. Runs the post-turn budget
// check and aborts the run if the cap was crossed. Synchronous in
// the providerProxy Stream wrapper goroutine; abort marks kit's
// per-run outcome unsuccess so the eventual AgentDone surfaces with
// Success=false. Lives on Agent (rather than as a closure inside
// buildKitAgent) so the bound function value is stable across
// providerProxy reads of [providerProxy.onTurnSettled].
func (a *Agent) onTurnSettled() {
	msg := a.checkTaskBudget()
	if msg == "" {
		return
	}
	a.send(event.AgentError{Err: msg})
	if a.kit != nil {
		a.kit.Abort()
	}
}

// sessionSnapshot returns the per-run usage snapshot from providerProxy,
// or a zero session when providerProxy is nil. The nil-safe path covers
// bare-Agent tests that construct an [Agent] without going through
// [New] — production [Agent.providerProxy] is always non-nil after
// [Agent.buildKitAgent].
func (a *Agent) sessionSnapshot() budget.Session {
	if a.providerProxy == nil {
		return budget.Session{}
	}
	return a.providerProxy.Snapshot()
}
