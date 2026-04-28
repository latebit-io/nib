package agent

import (
	"context"

	"github.com/latebit-io/junto/ai/llm"
)

// Tool is a capability the agent can invoke during the LLM loop. Each tool
// publishes its OpenAI-compatible schema and runs the corresponding logic.
//
// The agent calls Definition once at registration to populate the tool list
// sent to the LLM, and Execute every time the LLM dispatches a call. Tools
// must be safe to invoke concurrently when registered with concurrent
// execution; per-tool state (caches, retry counters) needs its own
// synchronization.
type Tool interface {
	// Definition returns the tool's function schema for the LLM.
	Definition() llm.ToolDef

	// Execute handles a tool call and returns a result fed back to the LLM.
	// The agent does not interpret Result.Content beyond passing it as the
	// tool message body — application-layer interpretation happens in hooks.
	Execute(ctx context.Context, call llm.ToolCall) ToolResult
}

// ToolResult is the value a tool returns to the agent loop.
//
// The shape is deliberately minimal: a string fed to the LLM and an error
// flag. Application-specific concepts (edit proposals, approval state,
// navigation requests) do NOT live here — they belong in the application
// layer and are surfaced through hooks (BeforeToolCall / AfterToolCall) or
// via tool-internal channels the application layer owns.
type ToolResult struct {
	// Content is the text returned to the LLM as the tool's reply.
	Content string

	// IsError flags that the tool execution failed. The LLM still sees
	// Content; frontends can render the failure distinctively.
	IsError bool
}

// Hooks let the application layer extend the loop without modifying it.
// Every field is optional — a nil hook is skipped. Hooks run synchronously
// on the agent's goroutine; long-running work should be dispatched off the
// hook's call stack.
type Hooks struct {
	// BeforeToolCall fires after the LLM emits a tool call but before the
	// tool's Execute method runs. The hook can block execution by setting
	// BeforeToolCallResult.Block; the loop emits a synthesized error
	// tool-result in that case so the LLM sees the rejection.
	BeforeToolCall func(ctx context.Context, c BeforeToolCallContext) (BeforeToolCallResult, error)

	// AfterToolCall fires after Execute returns, before the loop forwards
	// the result to the LLM. The hook can override Content / IsError or
	// signal early termination.
	AfterToolCall func(ctx context.Context, c AfterToolCallContext) (AfterToolCallResult, error)

	// TransformContext rewrites the message slice before each LLM call.
	// Used for compaction, redaction, or context-set injection. Return the
	// (possibly modified) slice; nil is treated as "use the input as-is."
	TransformContext func(ctx context.Context, messages []llm.Message) ([]llm.Message, error)

	// GetSteeringMessages returns messages the application wants injected
	// after the current assistant turn finishes. Returning nil or an empty
	// slice means "nothing pending."
	GetSteeringMessages func(ctx context.Context) ([]llm.Message, error)

	// GetFollowUpMessages returns messages the application wants processed
	// after the agent would otherwise stop. Distinct from steering: these
	// are NOT injected mid-run; they trigger a fresh continuation.
	GetFollowUpMessages func(ctx context.Context) ([]llm.Message, error)
}

// BeforeToolCallContext carries the data BeforeToolCall needs to evaluate a
// pending tool call.
type BeforeToolCallContext struct {
	// CallID is the tool call's unique identifier.
	CallID string
	// Name is the tool name the LLM dispatched.
	Name string
	// Args is the raw JSON argument string from the LLM.
	Args string
}

// BeforeToolCallResult is the optional return shape from BeforeToolCall.
// Non-zero fields override or block; the zero value lets execution proceed.
type BeforeToolCallResult struct {
	// Block prevents the tool from executing. The loop emits a synthesized
	// error tool-result so the LLM observes the rejection.
	Block bool
	// Reason is the message surfaced to the LLM when Block is true.
	// Empty Reason yields a generic "tool blocked" message.
	Reason string
}

// AfterToolCallContext carries the data AfterToolCall sees about a finished
// tool call.
type AfterToolCallContext struct {
	// CallID identifies the tool call that finished.
	CallID string
	// Name is the tool name that ran.
	Name string
	// Args is the raw JSON argument string from the LLM.
	Args string
	// Result is the tool's return value before any hook overrides apply.
	Result ToolResult
}

// AfterToolCallResult is the optional override from AfterToolCall. Each
// pointer field, when non-nil, replaces the corresponding part of the
// finalized result. Nil means "leave that field as the tool returned it."
type AfterToolCallResult struct {
	// Content overrides the tool result text (when non-nil).
	Content *string
	// IsError overrides the error flag (when non-nil).
	IsError *bool
	// Terminate signals the agent should stop after the current tool
	// batch. Early termination only fires when every finalized tool
	// result in the batch sets this to true.
	Terminate bool
}

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
