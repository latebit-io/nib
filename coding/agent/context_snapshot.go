package agent

import "github.com/latebit-io/nib/ai/llm"

// ContextSnapshot describes the agent's next-request input composition.
// Returned by [Agent.EstimateContext] for /context-style introspection
// — pure read, safe to call between turns, no LLM round-trip required.
type ContextSnapshot struct {
	// Estimate is the per-section client-side token estimate produced
	// by [llm.EstimateMessageTokens] against the current message slice
	// and the mode-filtered tool definitions.
	Estimate llm.InputEstimate

	// ToolCount is the number of tool definitions the LLM will receive
	// on the next request. Reflects mode-specific filtering (planning
	// mode applies a blocklist).
	ToolCount int

	// MessageCount is the number of messages currently held in the
	// saved transcript. Includes the system prompt if one has been
	// composed.
	MessageCount int
}

// EstimateContext returns the breakdown of the agent's next request
// without making an LLM call. Suitable for surfacing in a /context
// command so the developer can see which section (system prompt, tool
// definitions, history) is dominating the context window.
//
// Returns a zero [ContextSnapshot] when the kit foundation has not
// been built yet (no LLM credentials at startup — the model switcher
// will build the foundation on first connect).
func (a *Agent) EstimateContext() ContextSnapshot {
	if a.kit == nil {
		return ContextSnapshot{}
	}
	msgs := a.kit.State().Messages
	tools := a.activeToolDefs()
	return ContextSnapshot{
		Estimate:     llm.EstimateMessageTokens(msgs, tools),
		ToolCount:    len(tools),
		MessageCount: len(msgs),
	}
}
