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

// StreamEvent is one chunk from the LLM stream.
type StreamEvent struct {
	// Token is the text delta (may be empty on the final event).
	Token string
	// ToolCalls are the accumulated tool calls (populated when Done is true).
	ToolCalls []ToolCall
	// Done is true when the stream is complete.
	Done bool
}

// Provider abstracts an LLM backend for streaming chat completions.
type Provider interface {
	// Stream sends messages with the given tool definitions and returns
	// a channel of streaming events. The channel is closed when the
	// response is complete or ctx is cancelled.
	Stream(ctx context.Context, messages []Message, tools []ToolDef) (<-chan StreamEvent, error)
}
