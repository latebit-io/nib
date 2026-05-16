package agent

import (
	"context"

	"github.com/latebit-io/nib/agent/event"
	"github.com/latebit-io/nib/ai/llm"
)

// Hooks let the application layer extend the loop without modifying it.
// Every field is optional — a nil hook is skipped. Hooks run synchronously
// on the agent's goroutine; long-running work should be dispatched off the
// hook's call stack.
//
// Adding a new hook field is a parallel-surface change — see /nib/patterns.md
// "Parallel-surface triple: `Hooks` ↔ `mergeHooks` ↔ `chainX`". The field
// here must be paired with a `mergeHooks` switch case and a `chainX` helper
// in `kit/toolset.go`.
type Hooks struct {
	// BeforeToolCall fires after the LLM emits a tool call but before the
	// tool's Execute method runs. The hook can block execution by setting
	// BeforeToolCallResult.Block; the loop emits a synthesized error
	// tool-result in that case so the LLM sees the rejection.
	BeforeToolCall func(ctx context.Context, c BeforeToolCallInput) (BeforeToolCallResult, error)

	// AfterToolCall fires after Execute returns, before the loop forwards
	// the result to the LLM. The hook can override Content / IsError or
	// signal early termination.
	AfterToolCall func(ctx context.Context, c AfterToolCallInput) (AfterToolCallResult, error)

	// TransformContext rewrites the message slice before each LLM call.
	// Used for compaction, redaction, or context-set injection. Return the
	// (possibly modified) slice; nil is treated as "use the input as-is."
	TransformContext func(ctx context.Context, messages []llm.Message) ([]llm.Message, error)

	// SteeringMessages returns messages the application wants injected
	// after the current assistant turn finishes. Returning nil or an empty
	// slice means "nothing pending."
	SteeringMessages func(ctx context.Context) ([]llm.Message, error)

	// FollowUpMessages returns messages the application wants processed
	// after the agent would otherwise stop. Distinct from steering: these
	// are NOT injected mid-run; they trigger a fresh continuation.
	FollowUpMessages func(ctx context.Context) ([]llm.Message, error)

	// BeforePark fires after FollowUpMessages returns no messages and
	// before the loop parks on [Agent.awaitReply]. The hook returns the
	// payload the foundation includes in [event.AgentParked]; the
	// foundation has no opinion on what Finished means — the hook owns
	// the application-level "all work done" signal. A nil hook causes
	// the foundation to emit a zero-value AgentParked.
	//
	// BeforePark exists so that the application's "we're parked" signal
	// flows through the same event pipeline as every other foundation
	// event, preserving order with trailing MessageUpdate/AgentEnd
	// events. Sending an application-shaped event directly from
	// FollowUpMessages races the event pipeline and was the
	// historical source of out-of-order delivery to consumers.
	BeforePark func(ctx context.Context) (event.AgentParked, error)

	// OnTruncated fires when the provider's terminal Done event reports
	// Truncated=true, before the foundation surfaces the truncation as a
	// run-ending error. The hook owns the recovery decision: splice
	// rejection or recovery messages into the transcript and either retry
	// the turn or end the run.
	//
	// Returning Retry=true continues the loop after appending Messages —
	// the truncated tool calls are NEVER executed (partial arguments may
	// corrupt state) but their tool-role rejections in Messages keep the
	// transcript well-formed for the next provider call. Returning
	// Retry=false ends the run silently after appending Messages — the
	// hook is responsible for surfacing a user-facing [event.Error] (or an
	// application-shaped error event) before returning. A nil hook is
	// equivalent to Retry=false with the foundation emitting a generic
	// "provider truncated response" error.
	OnTruncated func(ctx context.Context, c TruncationInput) (TruncationResult, error)
}

// BeforeToolCallInput carries the data BeforeToolCall needs to evaluate a
// pending tool call.
type BeforeToolCallInput struct {
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

// AfterToolCallInput carries the data AfterToolCall sees about a finished
// tool call.
type AfterToolCallInput struct {
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

// TruncationInput carries the data [Hooks.OnTruncated] needs to decide
// whether to retry the turn or end the run.
type TruncationInput struct {
	// Assistant is the truncated assistant message. The foundation has
	// ALREADY appended it to the transcript before invoking the hook so
	// any rejection messages the hook returns slot in immediately after
	// it. ToolCalls (if any) carry possibly-partial argument JSON; the
	// hook must NOT attempt to execute them.
	Assistant llm.Message
	// ToolCalls is the pending tool call slice from the truncated
	// assistant message — equivalent to Assistant.ToolCalls but lifted
	// for ergonomic access. Chat-completion transcripts require a
	// tool-role reply for every entry before the next assistant turn;
	// hooks that retry must include those replies in [TruncationResult].
	ToolCalls []llm.ToolCall
}

// TruncationResult is the optional decision returned from
// [Hooks.OnTruncated]. The zero value (Retry=false, Messages=nil) ends
// the run without appending anything.
type TruncationResult struct {
	// Retry continues the loop with another turn after appending
	// Messages. False ends the run after appending Messages.
	Retry bool
	// Messages are appended to the transcript before retry — typically
	// a tool-role rejection for each pending tool call (chat-completion
	// transcripts validate the pairing) plus an optional user-role
	// nudge. Empty/nil is allowed; the foundation just continues with
	// the transcript as the hook left it.
	Messages []llm.Message
}
