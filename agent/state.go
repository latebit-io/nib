package agent

import "github.com/latebit-io/nib/ai/llm"

// State is the public snapshot of an agent's runtime state. Returned by
// [Agent.State] for read-only inspection by frontends and hooks. Mutating
// the slices or maps does not affect the agent.
type State struct {
	// Messages is the conversation transcript at snapshot time.
	Messages []llm.Message
	// Streaming is true while the agent is processing a turn.
	Streaming bool
	// PendingToolCalls is the set of tool call IDs currently executing.
	PendingToolCalls map[string]bool
	// LastError is the most recent loop-level error message, empty when none.
	LastError string
}
