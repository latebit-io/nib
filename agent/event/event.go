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

// AgentStart marks the beginning of a run (one Prompt invocation).
// Frontends use this to flip a "thinking" indicator on.
type AgentStart struct{}

// AgentEnd marks the end of a run. Messages contains the final transcript
// after all turns have completed (or after an early termination).
type AgentEnd struct {
	// Messages is the full conversation transcript at run-end.
	Messages []llm.Message
	// Reason says why the run ended.
	Reason EndReason
	// Usage totals the run's turns.
	Usage RunUsage
	// FinalText is the last non-empty text the model wrote in this run
	// since the last reply: the answer, with earlier tool-use narration
	// superseded. Empty when it wrote none. A turn that broke off
	// mid-stream counts with the text it had streamed, which Messages lacks.
	FinalText string
}

// EndReason says why a run ended; see [AgentEnd.Reason].
type EndReason string

const (
	// EndCompleted: a one-shot run (Options.OneShot in the agent package)
	// finished with nothing left to do. A parking agent never completes;
	// its runs end by one of the other reasons.
	EndCompleted EndReason = "completed"
	// EndAborted: the run's context was cancelled, by Agent.Abort or by
	// the caller's context. Errors that follow the cancel are not emitted.
	EndAborted EndReason = "aborted"
	// EndError: the run failed; an [Error] event carries the cause.
	EndError EndReason = "error"
	// EndMaxTurns: the turn cap was reached; [MaxTurnsReached] precedes.
	EndMaxTurns EndReason = "max_turns"
	// EndTerminated: every tool result of a batch asked the run to stop
	// (AfterToolCallResult.Terminate in the agent package).
	EndTerminated EndReason = "terminated"
	// EndTruncated: the provider ran out of output budget and the turn
	// was not retried. An [Error] precedes unless a truncation hook ran.
	EndTruncated EndReason = "truncated"
)

// RunUsage totals the provider-reported usage of every turn in a run.
// Unlike [TurnUsage] events, which are dropped when the consumer lags,
// the total is always complete. Token fields sum those of [llm.Usage].
type RunUsage struct {
	// Turns is the number of LLM turns that ran to their end.
	Turns int
	// ToolCalls is the number of tool calls the model requested.
	ToolCalls int
	// PromptTokens is the fresh (uncached) input tokens.
	PromptTokens int
	// CompletionTokens is the output tokens.
	CompletionTokens int
	// CachedTokens is the input tokens served from cache.
	CachedTokens int
	// CacheWriteTokens is the input tokens written to the cache.
	CacheWriteTokens int
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

// TurnUsage reports provider-reported token consumption for a single
// turn (zero when the provider returned no usage). Client-side estimates
// are an application concern layered on via a provider wrapper.
type TurnUsage struct {
	// Turn is the 1-indexed turn number within the current run.
	Turn int
	// PromptTokens is the provider-reported fresh (uncached) input tokens (0 if unavailable).
	PromptTokens int
	// CompletionTokens is the provider-reported output tokens (0 if unavailable).
	CompletionTokens int
	// CachedTokens is the provider-reported cached input tokens (0 if unavailable).
	CachedTokens int
	// ToolCalls is the number of tool calls dispatched in this turn.
	ToolCalls int
}

// Error carries a loop-level error — anything that can't be represented as
// a tool error or recovered through silent retry. Aborts the current run.
type Error struct {
	// Err is the human-readable error message.
	Err string
}

// MaxTurnsReached signals the run ended because it hit the configured
// turn cap (Options.MaxTurns in the agent package) — the loop was about
// to start another LLM turn without an intervening user message. A policy
// outcome, not a failure: the transcript is well-formed (every dispatched
// tool call has its result appended) and [AgentEnd] follows as usual, so
// consumers can continue the conversation with a fresh prompt. Frontends
// typically render this as "stopped at the turn limit" rather than as an
// error.
type MaxTurnsReached struct {
	// Turns is the number of LLM turns taken since the last user input
	// when the cap fired — always equal to the configured maximum.
	Turns int
}

// AgentParked signals the loop has finished a turn with no further work
// queued (no tool calls, no steering, no follow-up) and is about to park
// on the reply channel awaiting the next user message. Emitted from
// inside the loop just before [Agent.awaitReply], routed through the
// same event pipeline as every other foundation event so consumers see
// it in stream order relative to the trailing MessageUpdate/MessageEnd
// of the parking turn.
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
func (ToolEnd) agentEvent() {}

// agentEvent satisfies [Event].
func (TurnUsage) agentEvent() {}

// agentEvent satisfies [Event].
func (Error) agentEvent() {}

// agentEvent satisfies [Event].
func (MaxTurnsReached) agentEvent() {}

// agentEvent satisfies [Event].
func (AgentParked) agentEvent() {}
