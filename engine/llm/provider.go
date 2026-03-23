package llm

import "context"

// Message represents a single message in a conversation.
type Message struct {
	Role       string     `json:"role"`                   // "system", "user", "assistant", "tool"
	Content    string     `json:"content,omitempty"`      // text content
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // tool calls (assistant role only)
	ToolCallID string     `json:"tool_call_id,omitempty"` // which tool call this result is for (tool role only)
}

// ToolCall represents a function call the LLM wants to make.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // always "function"
	Function FunctionCall `json:"function"`
}

// FunctionCall is the name and arguments of a tool call.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // raw JSON string
}

// StreamEvent is one chunk from the LLM stream.
type StreamEvent struct {
	Token     string     // text delta (may be empty on final event)
	ToolCalls []ToolCall // accumulated tool calls (populated on Done)
	Done      bool       // true when the stream is complete
}

// Provider abstracts an LLM backend for streaming chat completions.
type Provider interface {
	// Stream sends messages with the given tool definitions and returns
	// a channel of streaming events. The channel is closed when the
	// response is complete or ctx is cancelled.
	Stream(ctx context.Context, messages []Message, tools []ToolDef) (<-chan StreamEvent, error)
}
