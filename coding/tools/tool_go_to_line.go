package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/event"
)

// GoToLineTool lets the agent navigate the developer's editor to a specific
// line in a file. This is a non-mutating "pointing" gesture — the agent uses
// it to direct attention while explaining code in the agent pane.
// Navigation is published through the [Navigator] collaborator so the
// tool never sees the events channel directly.
type GoToLineTool struct {
	workspace FileReader
	nav       Navigator
}

// NewGoToLineTool creates a GoToLineTool with the given workspace and
// navigator. The navigator publishes navigation events on the agent's
// events channel.
func NewGoToLineTool(ws FileReader, nav Navigator) *GoToLineTool {
	return &GoToLineTool{workspace: ws, nav: nav}
}

type goToLineArgs struct {
	Path string `json:"path"`
	Line int    `json:"line"`
}

// Definition returns the tool schema for the LLM.
func (t *GoToLineTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name: "go_to_line",
			Description: "Navigate the editor to a specific line in a file. " +
				"Use this to direct the developer's attention to code you're discussing. " +
				"Does not modify the file.",
			Parameters: llm.FunctionParams{
				Type: "object",
				Properties: map[string]llm.FunctionParam{
					"path": {
						Type:        "string",
						Description: "File path relative to project root.",
					},
					"line": {
						Type:        "integer",
						Description: "Line number (1-indexed).",
					},
				},
				Required: []string{"path", "line"},
			},
		},
	}
}

// Execute validates the file exists and publishes a navigation event
// before returning a confirmation message.
func (t *GoToLineTool) Execute(ctx context.Context, call llm.ToolCall) ToolResult {
	if ctx.Err() != nil {
		return errorResult("Error: agent canceled")
	}
	var args goToLineArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return errorResult(fmt.Sprintf("Error: invalid arguments: %v", err))
	}
	if args.Path == "" {
		return errorResult("Error: path is required")
	}
	if args.Line < 1 {
		return errorResult("Error: line must be >= 1")
	}
	// Validate file exists by attempting to read it.
	if _, err := t.workspace.ReadFile(args.Path); err != nil {
		return errorResult(fmt.Sprintf("Error: %v", err))
	}
	t.nav.Navigate(ctx, event.AgentNavigate{Path: args.Path, Line: args.Line})
	return textResult(fmt.Sprintf("Navigated to %s line %d.", args.Path, args.Line))
}
