package agent

import (
	"context"

	"github.com/latebit-io/nib/ai/llm"
)

// Tool is a capability the agent can invoke: a schema for the model and
// the logic behind it. One admitted by [Options.ParallelTools] may run
// concurrently and must guard its own state. kit/contracttest checks tools.
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
