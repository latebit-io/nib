// Package event defines the generic agent-loop lifecycle events.
//
// These events describe what the loop is doing: starting a run, finishing
// a turn, streaming text, executing a tool. They carry no application
// knowledge — no edit proposals, no approval state, no UI status kinds.
// Applications consume this stream and translate into their own event
// vocabulary at the application boundary.
package event

import "github.com/latebit-io/nib/ai/llm"

// Event is the sealed interface for every event the agent loop emits. The
// marker is unexported so the family is closed at the package boundary:
// only types defined in this file can implement Event.
type Event interface {
	agentEvent()
}

// AgentStart marks the beginning of a run (one Prompt or Continue invocation).
// Frontends use this to flip a "thinking" indicator on.
type AgentStart struct{}

// AgentEnd marks the end of a run. Messages contains the final transcript
// after all turns have completed (or after an early termination).
type AgentEnd struct {
	// Messages is the full conversation transcript at run-end.
	Messages []llm.Message
}

// TurnStart marks the start of a single turn (one assistant response,
// possibly followed by tool calls). A run consists of one or more turns.
type TurnStart struct{}

// TurnEnd marks the end of a single turn after the assistant message has
// been finalized and all tool calls in that message have executed.
type TurnEnd struct {
	// Message is the finalized assistant message for this turn.
	Message llm.Message
}

// MessageStart signals the start of streaming an assistant message.
// Emitted before the first token arrives.
type MessageStart struct{}

// MessageUpdate carries one streamed delta from the LLM. Frontends append
// to their rendering buffer. Emitted many times per assistant message.
type MessageUpdate struct {
	// Delta is the text fragment newly appended to the assistant message.
	Delta string
}

// MessageEnd signals the assistant message has finished streaming and the
// final content + tool calls are now available.
type MessageEnd struct {
	// Message is the finalized assistant message.
	Message llm.Message
}

// ToolStart announces the agent is about to execute a tool call. Emitted
// before the tool's Execute method is invoked so frontends can render the
// pending operation.
type ToolStart struct {
	// CallID uniquely identifies this tool call within the conversation.
	CallID string
	// Name is the tool name (e.g., "read_file").
	Name string
	// Args is the raw JSON argument string from the LLM.
	Args string
}

// ToolUpdate carries an in-progress update from a long-running tool. Tools
// that stream output (e.g., bash commands) emit these between Start and End.
type ToolUpdate struct {
	// CallID identifies the tool call this update belongs to.
	CallID string
	// Partial is tool-specific intermediate state. Frontends type-assert
	// against known tool result types when they want to render progress.
	Partial any
}

// ToolEnd reports a tool call has completed. Result is the text returned
// to the LLM as the tool result; IsError flags whether the tool execution
// failed (the LLM still sees Result, but frontends can highlight the error).
type ToolEnd struct {
	// CallID identifies the tool call that ended.
	CallID string
	// Result is the text fed back to the LLM as the tool result.
	Result string
	// IsError is true when the tool execution failed.
	IsError bool
}

// TurnUsage reports token consumption for a single turn. Combines the
// provider-reported counts (when available) with client-side estimates
// so the UI can show real-time cost without waiting for the provider's
// final tally.
type TurnUsage struct {
	// Turn is the 1-indexed turn number within the current run.
	Turn int
	// PromptTokens is the provider-reported total input tokens (0 if unavailable).
	PromptTokens int
	// CompletionTokens is the provider-reported output tokens (0 if unavailable).
	CompletionTokens int
	// CachedTokens is the provider-reported cached input tokens (0 if unavailable).
	CachedTokens int
	// ToolCalls is the number of tool calls dispatched in this turn.
	ToolCalls int
	// SystemEst is the estimated system prompt tokens.
	SystemEst int
	// ToolsEst is the estimated tool definition tokens.
	ToolsEst int
	// HistoryEst is the estimated conversation history tokens.
	HistoryEst int
	// NewEst is the estimated new input tokens.
	NewEst int
	// CompletionEst is the estimated output tokens (from streamed content length).
	CompletionEst int
}

// InputEstimate signals the estimated input token composition before an LLM
// call starts. Sent right before the stream opens so the frontend can show
// real-time input cost while the response streams in.
type InputEstimate struct {
	// System is the estimated system prompt tokens.
	System int
	// Tools is the estimated tool definition tokens.
	Tools int
	// History is the estimated conversation history tokens.
	History int
	// New is the estimated new input tokens.
	New int
}

// Compacted signals conversation history was reduced to control token cost.
// Emitted once per compaction pass, before the next LLM call.
type Compacted struct {
	// BeforeTokens is the estimated history tokens before compaction.
	BeforeTokens int
	// AfterTokens is the estimated history tokens after compaction.
	AfterTokens int
}

// Error carries a loop-level error — anything that can't be represented as
// a tool error or recovered through silent retry. Aborts the current run.
type Error struct {
	// Err is the human-readable error message.
	Err string
}

// AgentParked signals the loop has finished a turn with no further work
// queued (no tool calls, no steering, no follow-up) and is about to park
// on the reply channel awaiting the next user message. Emitted from
// inside the loop just before [Agent.awaitReply], routed through the
// same event pipeline as every other foundation event so consumers see
// it in stream order relative to the trailing AgentToken/MessageEnd of
// the parking turn.
//
// Finished is populated by the [Hooks.BeforePark] callback (when set);
// the foundation has no opinion on what "finished" means at the
// application layer. Coding agents typically populate it from a task
// tracker; non-coding agents can leave it false.
type AgentParked struct {
	// Finished reflects an application-level "all work done" signal,
	// supplied by [Hooks.BeforePark]. False when the hook is unset.
	Finished bool
}

// agentEvent satisfies [Event].
func (AgentStart) agentEvent() {}

// agentEvent satisfies [Event].
func (AgentEnd) agentEvent() {}

// agentEvent satisfies [Event].
func (TurnStart) agentEvent() {}

// agentEvent satisfies [Event].
func (TurnEnd) agentEvent() {}

// agentEvent satisfies [Event].
func (MessageStart) agentEvent() {}

// agentEvent satisfies [Event].
func (MessageUpdate) agentEvent() {}

// agentEvent satisfies [Event].
func (MessageEnd) agentEvent() {}

// agentEvent satisfies [Event].
func (ToolStart) agentEvent() {}

// agentEvent satisfies [Event].
func (ToolUpdate) agentEvent() {}

// agentEvent satisfies [Event].
func (ToolEnd) agentEvent() {}

// agentEvent satisfies [Event].
func (TurnUsage) agentEvent() {}

// agentEvent satisfies [Event].
func (InputEstimate) agentEvent() {}

// agentEvent satisfies [Event].
func (Compacted) agentEvent() {}

// agentEvent satisfies [Event].
func (Error) agentEvent() {}

// agentEvent satisfies [Event].
func (AgentParked) agentEvent() {}
