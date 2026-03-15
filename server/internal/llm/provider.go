package llm

import "context"

// Message represents a single message in a conversation.
type Message struct {
	Role    string // "system", "user", "assistant"
	Content string
}

// StreamEvent is one chunk from the LLM stream.
type StreamEvent struct {
	Token string // text delta (may be empty on final event)
	Done  bool   // true when the stream is complete
}

// Provider abstracts an LLM backend for streaming chat completions.
type Provider interface {
	// Stream sends messages and returns a channel of streaming events.
	// The channel is closed when the response is complete or ctx is cancelled.
	Stream(ctx context.Context, messages []Message) (<-chan StreamEvent, error)
}
