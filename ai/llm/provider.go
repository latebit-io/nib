package llm

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"time"
)

// Message represents a single message in a conversation.
type Message struct {
	// Role is the message role: "system", "user", "assistant", or "tool".
	Role string `json:"role"`
	// Content is the text content of the message.
	Content string `json:"content,omitempty"`
	// ToolCalls are the tool calls requested by the assistant (assistant role only).
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID identifies which tool call this result is for (tool role only).
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// ToolCall represents a function call the LLM wants to make.
type ToolCall struct {
	// ID is the unique identifier for this tool call.
	ID string `json:"id"`
	// Type is the tool call type (always "function").
	Type string `json:"type"`
	// Function contains the function name and arguments.
	Function FunctionCall `json:"function"`
}

// FunctionCall is the name and arguments of a tool call.
type FunctionCall struct {
	// Name is the function name to call.
	Name string `json:"name"`
	// Arguments is the raw JSON string of function arguments.
	Arguments string `json:"arguments"`
}

// Usage holds token consumption data from a single LLM call.
// Populated from the provider's response when available.
//
// Field semantics are NORMALIZED across providers: regardless of
// whether the upstream API returns a gross input_tokens (OpenAI) or
// a fresh-only input_tokens (Anthropic), adapters populate
// PromptTokens with the FRESH (uncached) portion and CachedTokens
// with the cached subset. The two are disjoint. Total per-turn
// input = PromptTokens + CachedTokens.
//
// This matches opencode's `tokens.input` and pi's `input` so the
// per-turn footprint formula (input + cached + output [+
// cache_write + reasoning]) is directly comparable across the
// three tools.
type Usage struct {
	// PromptTokens is the number of FRESH (uncached) input tokens
	// — the portion paid for at the full per-token rate on this
	// turn. Disjoint from CachedTokens.
	PromptTokens int
	// CompletionTokens is the number of output tokens generated.
	CompletionTokens int
	// CachedTokens is the number of input tokens served from cache
	// at the discounted rate. Disjoint from PromptTokens. Zero
	// when caching is not active.
	CachedTokens int
	// CacheWriteTokens is the number of input tokens written to the
	// prompt cache on this turn (Anthropic cache_creation_input_tokens).
	// Billed at a premium (~1.25x the fresh input rate) over PromptTokens,
	// and disjoint from both PromptTokens and CachedTokens. Zero when no
	// cache write occurred or the provider does not report it.
	CacheWriteTokens int
}

// StreamEvent is one chunk from the LLM stream.
type StreamEvent struct {
	// Token is the text delta (may be empty on the final event).
	Token string
	// ToolCalls are the accumulated tool calls (populated when Done is true).
	ToolCalls []ToolCall
	// Done is true when the stream is complete.
	Done bool
	// Usage holds token consumption data, populated on the final event
	// when the provider reports usage. Nil when unavailable.
	Usage *Usage
	// Truncated is true when the provider stopped mid-generation because
	// the output token limit was reached. Any accumulated ToolCalls on a
	// truncated event may have incomplete arguments and must not be
	// executed — silently applying a truncated edit corrupts the file.
	Truncated bool
	// Err carries a provider-side stream failure as a terminal event:
	// a streamed error block, a payload-limit breach, or a mid-stream
	// stall. When non-nil this is the final event (Done is also set) and
	// any accumulated content, tool calls, or usage may be partial.
	// Consumers must surface Err instead of treating the turn as a clean
	// completion. Nil on success.
	Err error
}

// CacheControl marks a message or tool definition for provider-level prompt
// caching. Supported by Anthropic models via OpenRouter — cached input tokens
// cost ~90% less on subsequent requests with the same prefix.
type CacheControl struct {
	// Type is the cache control type (always "ephemeral").
	Type string `json:"type"`
}

// maxToolArgBytes is the maximum cumulative size of streamed tool call
// arguments. Matches the SSE scanner's 10MB cap to prevent unbounded growth.
const maxToolArgBytes = 10 * 1024 * 1024

// maxToolCalls is the maximum number of concurrent tool calls in a single
// response. Prevents unbounded slice/map growth from malformed SSE payloads.
const maxToolCalls = 128

// sseIdleTimeout caps the wait between consecutive SSE chunks. Normal slow
// generation streams tokens well within this window; a longer gap means the
// connection has stalled mid-stream, so the watchdog tears the body down to
// fail the turn instead of hanging it forever. Generous enough not to kill a
// model that pauses to think between tokens.
const sseIdleTimeout = 60 * time.Second

// streamWatchdog closes the response body when no SSE chunk has arrived
// within sseIdleTimeout, converting a silent mid-stream stall into a prompt
// read error. Callers reset it after every successful read and stop it on
// return; fired reports whether the timeout (not a normal close) triggered.
type streamWatchdog struct {
	timer   *time.Timer
	stalled atomic.Bool
}

// newStreamWatchdog starts a watchdog that closes body after sseIdleTimeout
// of inactivity. The returned watchdog must be stopped by the caller.
func newStreamWatchdog(body io.Closer) *streamWatchdog {
	w := &streamWatchdog{}
	w.timer = time.AfterFunc(sseIdleTimeout, func() {
		w.stalled.Store(true)
		_ = body.Close() // unblock the blocked read; close error is not actionable
	})
	return w
}

// reset restarts the idle countdown after a chunk is read.
func (w *streamWatchdog) reset() { w.timer.Reset(sseIdleTimeout) }

// stop halts the watchdog. Safe to call after it has fired.
func (w *streamWatchdog) stop() { w.timer.Stop() }

// fired reports whether the watchdog closed the body due to a stall.
func (w *streamWatchdog) fired() bool { return w.stalled.Load() }

// errStreamStalled is the terminal error surfaced when the watchdog tears
// down a stalled stream.
var errStreamStalled = fmt.Errorf("llm: stream stalled (no data for %s)", sseIdleTimeout)

// trySend sends an event on ch, returning false if ctx is cancelled.
// Prevents SSE goroutines from blocking indefinitely when the downstream
// reader stalls or abandons the stream.
func trySend(ctx context.Context, ch chan<- StreamEvent, evt StreamEvent) bool {
	select {
	case ch <- evt:
		return true
	case <-ctx.Done():
		return false
	}
}

// Provider abstracts an LLM backend for streaming chat completions.
//
// Implementations should pass [github.com/latebit-io/nib/kit/contracttest.Provider]
// — the fixture verifies channel closure on completion and cancel,
// no-events-after-Done, Truncated-implies-Done, and concurrent-Stream
// safety against any conforming implementation.
type Provider interface {
	// Stream sends messages with the given tool definitions and returns
	// a channel of streaming events. The channel is closed when the
	// response is complete or ctx is cancelled.
	Stream(ctx context.Context, messages []Message, tools []ToolDef) (<-chan StreamEvent, error)
}

// Effort is a provider-agnostic reasoning-effort level. Providers map it
// to their own reasoning control (OpenAI-style reasoning_effort, Anthropic
// extended-thinking budget). The empty value means "unset" — no reasoning
// field is sent and the provider's default applies. Effort is fixed at
// provider construction, like the model, since [Provider.Stream] takes no
// per-call parameters.
type Effort string

// Effort levels, mirroring the set an agent definition may request.
const (
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	EffortXHigh  Effort = "xhigh"
	EffortMax    Effort = "max"
)

// openAIEffort maps an [Effort] to an OpenAI reasoning-effort value
// (low|medium|high). nib's higher tiers (xhigh, max) collapse to "high",
// the ceiling the OpenAI reasoning API accepts. Returns "" for an unset or
// unrecognized effort so the caller omits the field entirely.
func openAIEffort(e Effort) string {
	switch e {
	case EffortLow:
		return "low"
	case EffortMedium:
		return "medium"
	case EffortHigh, EffortXHigh, EffortMax:
		return "high"
	default:
		return ""
	}
}
