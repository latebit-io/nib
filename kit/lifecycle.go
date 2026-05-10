package kit

// AgentLifecycle is the minimum surface a frontend needs from a
// kit-level agent: the ability to release the goroutines and channels
// the agent holds when the frontend exits. [*Agent] satisfies it
// directly; coding's agent satisfies it transitively through its
// embedded kit-agent handle.
//
// This is the named seam between a frontend and an agent. Today the
// surface is small (Close only) because that is the entire literal
// surface the reference TUI consumes — every other agent-shaped
// interaction routes through a session, [Compactor], or
// consumer-defined interfaces (HistoryResetter, etc.). Growing the
// surface requires either (a) a frontend that needs more than Close,
// or (b) a non-coding agent that needs to share the TUI's session
// wiring. Neither has materialized; the port is honest at one method.
type AgentLifecycle interface {
	// Close releases the resources the agent holds. After Close
	// returns, no further events will be emitted on the agent's
	// event channel and no further runs may be started. Idempotent;
	// safe to call exactly once.
	Close()
}

// Compile-time assertion that [*Agent] satisfies [AgentLifecycle].
// Kept next to the interface so a Close-signature change fails the
// build at the contract definition.
var _ AgentLifecycle = (*Agent)(nil)
