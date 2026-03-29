package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/llm"
)

// GoToLineTool lets the agent navigate the developer's editor to a specific
// line in a file. This is a non-mutating "pointing" gesture — the agent uses
// it to direct attention while explaining code in the agent pane.
// Navigation is handled by the frontend via an AgentNavigate event.
type GoToLineTool struct {
	workspace Workspace
	send      func(event.Event)
}

// NewGoToLineTool creates a GoToLineTool with the given workspace and event sender.
func NewGoToLineTool(ws Workspace, send func(event.Event)) *GoToLineTool {
	return &GoToLineTool{workspace: ws, send: send}
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

// Execute sends a navigation event to the frontend for the specified file and line.
func (t *GoToLineTool) Execute(ctx context.Context, call llm.ToolCall) string {
	if ctx.Err() != nil {
		return "Error: agent canceled"
	}
	var args goToLineArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("Error: invalid arguments: %v", err)
	}
	if args.Path == "" {
		return "Error: path is required"
	}
	if args.Line < 1 {
		return "Error: line must be >= 1"
	}
	// Validate file exists by attempting to read it.
	if _, err := t.workspace.ReadFile(args.Path); err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	t.send(event.AgentNavigate{Path: args.Path, Line: args.Line})
	return fmt.Sprintf("Navigated to %s line %d.", args.Path, args.Line)
}
