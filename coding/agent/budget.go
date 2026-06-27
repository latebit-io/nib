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
// ([Agent.foundationBudgetCheck] in foundation_hooks.go). Both read the
// usage snapshot from [providerProxy.Snapshot] and latch
// [Agent.budgetExceeded] under [Agent.mu] via the shared
// [Agent.evaluateBudgetLatch] helper.
//
// Per-run usage accumulation does NOT live here and is not the
// forwarder's job — the forwarder DROPS [event.AgentTurnUsage]. The
// authoritative per-run total is accumulated in
// [providerProxy.recordUsage] (provider_proxy.go) on the Stream wrapper
// goroutine, stored in [providerProxy.session], and reset per-run via
// [providerProxy.ResetSession]. The budget gate reads that total back
// through [Agent.sessionSnapshot].

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
	// Already-latched and not-exceeded both yield "" — no message to
	// emit. Only a fresh overrun returns the formatted message.
	msg, _ := a.evaluateBudgetLatch()
	return msg
}

// evaluateBudgetLatch reads the [Agent.budgetExceeded] latch and, when
// not already latched, evaluates the per-task token cap against the
// current [providerProxy] usage snapshot. On a fresh overrun it sets the
// latch and returns the formatted message with exceeded=true. When the
// latch is already set it returns ("", true) — exceeded, but no new
// message to emit (avoids double-emit). Returns ("", false) when the cap
// is disabled or not yet crossed.
//
// Single home for the latch-read → [budget.Exceeded] → set-latch
// pattern shared by [Agent.checkTaskBudget] (post-turn, adapts to a
// string) and [Agent.foundationBudgetCheck] (pre-Stream, adapts to an
// error). [budget.Exceeded] never returns a non-empty message with
// exceeded=false, so an empty msg with exceeded=true unambiguously means
// "already latched".
func (a *Agent) evaluateBudgetLatch() (msg string, exceeded bool) {
	a.mu.Lock()
	if a.budgetExceeded {
		a.mu.Unlock()
		return "", true
	}
	a.mu.Unlock()

	msg, over := budget.Exceeded(a.sessionSnapshot(), a.taskTokenBudget)
	if !over {
		return "", false
	}

	a.mu.Lock()
	a.budgetExceeded = true
	a.mu.Unlock()
	return msg, true
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
		a.kit.Cancel()
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
