// Package budget holds the pure token-accounting types and helpers
// used by the agent's per-task budget enforcement. The Agent owns the
// runtime concerns (mutex, runID, latching, event emission); this
// package owns the data shapes (Session, Turn) and the math
// (Resolve, WouldExceed, Exceeded). Splitting the math out lets it be
// table-driven tested without an Agent and keeps the Agent's budget
// methods focused on coordination rather than arithmetic.
package budget

import (
	"fmt"

	"github.com/latebit-io/nib/ai/llm"
)

// DefaultTaskTokens caps prompt+completion tokens for a single agent
// run when the caller does not set NewOptions.TaskTokenBudget
// explicitly. Sized to comfortably cover a multi-file refactor or a
// small game build while still tripping long before a runaway loop
// burns the developer's wallet. The 2026-04-26 pacman regression
// burned 55M+ tokens on a single task; this default catches that
// class of cascade two orders of magnitude earlier. Override with
// NewOptions.TaskTokenBudget when a larger or smaller cap suits the
// workload.
const DefaultTaskTokens = 2_000_000

// Session holds accumulated token consumption across an agent run.
type Session struct {
	// TotalPromptTokens is the sum of provider-reported input tokens.
	TotalPromptTokens int
	// TotalCompletionTokens is the sum of provider-reported output tokens.
	TotalCompletionTokens int
	// TotalCachedTokens is the sum of provider-reported cached input tokens.
	TotalCachedTokens int
	// Turns is the number of completed turns.
	Turns int
}

// Turn accumulates token consumption across multiple LLM calls within
// a single agent turn (the inner loop may call Stream multiple times
// due to tool-call iterations). Provider counts are summed across all
// calls in the turn. LastEstimate reflects the final LLM call only —
// it shows the current input composition, which is the most
// meaningful snapshot (summing estimates across iterations would
// double-count the system prompt and tools).
type Turn struct {
	// PromptTokens is the cumulative prompt-token count across this turn.
	PromptTokens int
	// CompletionTokens is the cumulative completion-token count across this turn.
	CompletionTokens int
	// CachedTokens is the cumulative cached-input-token count across this turn.
	CachedTokens int
	// CompletionEst is the client-side output-token estimate, summed across calls.
	CompletionEst int
	// ToolCalls is the count of tool-call dispatches in this turn.
	ToolCalls int
	// LastEstimate captures the input composition of the FINAL LLM call.
	LastEstimate llm.InputEstimate
}

// AddUsage incorporates provider-reported usage from one LLM call
// into the running turn totals. A nil usage is a no-op so callers do
// not need to nil-check before delegating.
func (t *Turn) AddUsage(usage *llm.Usage) {
	if usage == nil {
		return
	}
	t.PromptTokens += usage.PromptTokens
	t.CompletionTokens += usage.CompletionTokens
	t.CachedTokens += usage.CachedTokens
}

// Resolve maps the user-facing NewOptions.TaskTokenBudget value to
// the internal token cap:
//
//	== 0 — use [DefaultTaskTokens].
//	 < 0 — explicit unlimited (returns 0; the budget check is skipped).
//	 > 0 — passed through verbatim as the cap.
//
// Returning 0 to mean "disabled" inside the Agent is a deliberate
// asymmetry: a developer who wants the safety net off has to opt in
// with a negative number; the default is always-on.
func Resolve(input int) int {
	switch {
	case input < 0:
		return 0
	case input > 0:
		return input
	default:
		return DefaultTaskTokens
	}
}

// WouldExceed reports whether committed + pending usage crosses the
// cap. Pure projection of the math used by the Agent's inner-loop
// gate (between provider Stream calls within a single turn). Returns
// false when the cap is disabled (cap <= 0).
//
// Sums prompt + completion tokens. Cached tokens are already a subset
// of prompt and would double-count if added separately.
//
// Comparator is `>=` (not `>`) so the cap value itself is over the
// line — a turn whose accounting lands exactly at the budget triggers
// the abort rather than letting one more Stream call slip through.
func WouldExceed(committed Session, pending Turn, limit int) bool {
	if limit <= 0 {
		return false
	}
	used := committed.TotalPromptTokens + committed.TotalCompletionTokens
	add := pending.PromptTokens + pending.CompletionTokens
	return used+add >= limit
}

// Exceeded reports whether committed usage alone has crossed the cap
// and returns the formatted abort message on a hit. Pure projection
// of the math used by the Agent's outer-loop gate (after a turn's
// usage has been recorded). Returns ("", false) when the cap is
// disabled or not yet exceeded.
//
// The Agent layer is responsible for latching budgetExceeded after
// the first hit; this helper does no state mutation.
func Exceeded(committed Session, limit int) (msg string, exceeded bool) {
	if limit <= 0 {
		return "", false
	}
	used := committed.TotalPromptTokens + committed.TotalCompletionTokens
	if used < limit {
		return "", false
	}
	msg = fmt.Sprintf(
		"task token budget exceeded: %d tokens used (cap %d) across %d turn(s); "+
			"aborting to prevent runaway cost. "+
			"Set NewOptions.TaskTokenBudget = -1 to disable, or a higher value to raise the cap.",
		used, limit, committed.Turns,
	)
	return msg, true
}
