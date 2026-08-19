// Package budget holds the pure token-accounting types and helpers
// used by an agent's per-task budget enforcement. The consuming agent
// owns the runtime concerns (mutex, run identity, latching, event
// emission); this package owns the data shapes (Session, Turn) and the
// math (Resolve, Exceeded). Splitting the math out lets it be
// table-driven tested without an agent and keeps the consumer's budget
// code focused on coordination rather than arithmetic.
package budget

import (
	"fmt"
	"strconv"

	"github.com/latebit-io/nib/ai/llm"
)

// RecommendedTaskTokens is the suggested cap on prompt+completion
// tokens for a single agent run. It is NOT applied by default — the
// zero value of NewOptions.TaskTokenBudget leaves the budget disabled
// (see [Resolve]). It is exported so a caller that wants the safety
// net can opt in with NewOptions.TaskTokenBudget =
// budget.RecommendedTaskTokens (or NIB_TASK_TOKEN_BUDGET set to this
// value). Sized to comfortably cover a multi-file refactor or a small
// game build while still tripping long before a runaway loop burns the
// developer's wallet. The 2026-04-26 pacman regression burned 55M+
// tokens on a single task; this value catches that class of cascade
// two orders of magnitude earlier.
const RecommendedTaskTokens = 2_000_000

// Session holds accumulated token consumption across an agent run.
type Session struct {
	// TotalPromptTokens is the sum of provider-reported input tokens.
	TotalPromptTokens int
	// TotalCompletionTokens is the sum of provider-reported output tokens.
	TotalCompletionTokens int
	// TotalCachedTokens is the sum of provider-reported cached input tokens.
	TotalCachedTokens int
	// TotalCacheWriteTokens is the sum of provider-reported cache-write
	// input tokens (disjoint from prompt and cached).
	TotalCacheWriteTokens int
	// Turns is the number of completed turns.
	Turns int
}

// AddUsage folds one LLM call's provider-reported usage into the
// session totals. A nil usage is a no-op. Turns is not touched — the
// caller decides what constitutes a turn.
func (s *Session) AddUsage(usage *llm.Usage) {
	if usage == nil {
		return
	}
	s.TotalPromptTokens += usage.PromptTokens
	s.TotalCompletionTokens += usage.CompletionTokens
	s.TotalCachedTokens += usage.CachedTokens
	s.TotalCacheWriteTokens += usage.CacheWriteTokens
}

// totalTokens returns every input-side token category summed with the
// output: prompt, cached and cache-write are documented disjoint in
// [llm.Usage], so adding all three counts the input footprint once.
func (s Session) totalTokens() int {
	return s.TotalPromptTokens + s.TotalCachedTokens + s.TotalCacheWriteTokens + s.TotalCompletionTokens
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
	// CacheWriteTokens is the cumulative cache-write-input-token count
	// across this turn (disjoint from PromptTokens and CachedTokens).
	CacheWriteTokens int
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
	t.CacheWriteTokens += usage.CacheWriteTokens
}

// Resolve maps the user-facing NewOptions.TaskTokenBudget value to
// the internal token cap:
//
//	== 0 — disabled (returns 0; the budget check is skipped). Default.
//	 < 0 — disabled (returns 0; the budget check is skipped).
//	 > 0 — passed through verbatim as the cap.
//
// The safety net is OFF by default: only a positive value arms it. A
// caller that wants the recommended cap opts in with
// [RecommendedTaskTokens]; a caller that wants none leaves the field
// zero. Both 0 and any negative collapse to the same disabled state.
func Resolve(input int) int {
	if input > 0 {
		return input
	}
	return 0
}

// ParseEnvCap resolves a raw NIB_TASK_TOKEN_BUDGET-style env value
// into the per-task token cap. An empty string means "unset" and
// yields 0 — the cap disabled by default. A parsed integer is mapped
// through [Resolve] (<= 0 disabled, > 0 the cap).
//
// A non-empty value that is not an integer is a configuration error,
// not a silent no-op: ParseEnvCap returns a non-nil error so the
// caller fails loudly. The variable is only ever set with intent to
// change the cap, so a typo (e.g. "2_000_000" or "2m") must not slip
// through and leave the guardrail off without a word.
//
// Keeping the parse here (rather than inline at each binary's wiring)
// lets the env-override semantics be table-tested without a real
// process environment and keeps the construction sites identical.
func ParseEnvCap(raw string) (limit int, err error) {
	if raw == "" {
		return 0, nil
	}
	n, convErr := strconv.Atoi(raw)
	if convErr != nil {
		return 0, fmt.Errorf("budget: invalid token budget %q: expected an integer token count", raw)
	}
	return Resolve(n), nil
}

// Exceeded reports whether committed usage has crossed the cap and
// returns the formatted abort message on a hit. Returns ("", false)
// when the cap is disabled or not yet exceeded.
//
// Sums prompt + cached + cache-write + completion tokens. Per the
// [llm.Usage] convention the three input categories are disjoint, so
// the input footprint is counted exactly once; cached tokens are still
// real tokens against the context window and the developer's spend.
//
// Comparator is `>=` (not `>`) so the cap value itself is over the
// line. The consuming agent latches the first hit; this helper does
// no state mutation.
func Exceeded(committed Session, limit int) (msg string, exceeded bool) {
	if limit <= 0 {
		return "", false
	}
	used := committed.totalTokens()
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
