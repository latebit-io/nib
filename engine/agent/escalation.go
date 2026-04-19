package agent

// maxTokensEscalator is the optional capability surface LLM providers
// implement to support runtime escalation of their output-token cap. The
// agent escalates after detecting a truncated response so the retry has
// more headroom, rather than looping against the same ceiling.
type maxTokensEscalator interface {
	MaxTokens() int
	SetMaxTokens(int)
}

// maxTokensInitialEscalation is the starting value used when the provider
// had no cap set (e.g. OpenAI-compat defaults to "unset"). Chosen to give
// the model meaningful extra room while staying below typical per-model caps
// that would reject the request outright.
const maxTokensInitialEscalation = 32768

// maxTokensCeiling bounds escalation to prevent unbounded doubling against
// models that will never honour it. 65536 matches the largest output cap
// supported by current frontier models as of this writing.
const maxTokensCeiling = 65536

// maxTruncationRetries bounds consecutive truncated turns before the agent
// abandons the run. Three total attempts is enough for the usual escalation
// path (default → 32K → 65K ceiling) plus one final "split the work" nudge;
// beyond that a looping or malfunctioning model would just burn requests.
// Reset to zero on the first non-truncated turn so long sessions with
// occasional truncations don't accumulate toward the limit.
const maxTruncationRetries = 2

// escalateMaxTokens returns the next max-tokens value for a provider whose
// previous turn was truncated. Doubles the current value, starting at
// maxTokensInitialEscalation when unset, capped at maxTokensCeiling.
// Returns the ceiling unchanged when already at or above it.
func escalateMaxTokens(current int) int {
	if current >= maxTokensCeiling {
		return maxTokensCeiling
	}
	next := current * 2
	if next < maxTokensInitialEscalation {
		next = maxTokensInitialEscalation
	}
	if next > maxTokensCeiling {
		next = maxTokensCeiling
	}
	return next
}
