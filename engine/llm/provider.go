package llm

import "context"

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
type Usage struct {
	// PromptTokens is the total number of input tokens.
	PromptTokens int
	// CompletionTokens is the number of output tokens generated.
	CompletionTokens int
	// CachedTokens is the number of input tokens served from cache
	// (a subset of PromptTokens). Zero when caching is not active.
	CachedTokens int
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
type Provider interface {
	// Stream sends messages with the given tool definitions and returns
	// a channel of streaming events. The channel is closed when the
	// response is complete or ctx is cancelled.
	Stream(ctx context.Context, messages []Message, tools []ToolDef) (<-chan StreamEvent, error)
}
