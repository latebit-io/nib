package agent

import (
	"context"

	"github.com/latebit-io/nib/ai/llm"
)

// Tool is a capability the agent can invoke during the LLM loop. Each tool
// publishes its OpenAI-compatible schema and runs the corresponding logic.
//
// The agent calls Definition once at registration to populate the tool list
// sent to the LLM, and Execute every time the LLM dispatches a call. Tools
// must be safe to invoke concurrently when registered with concurrent
// execution; per-tool state (caches, retry counters) needs its own
// synchronization.
//
// Implementations should pass [github.com/latebit-io/nib/kit/contracttest.Tool]
// — the fixture verifies Definition() shape and stability, panic-free
// handling of malformed/empty arguments, and concurrent Execute safety.
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
