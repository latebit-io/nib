package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/latebit-io/junto/engine/llm"
)

// GoToLineTool lets the agent navigate the developer's editor to a specific
// line in a file. This is a non-mutating "pointing" gesture — the agent uses
// it to direct attention while explaining code in the agent pane.
type GoToLineTool struct {
	nav Navigator
}

// NewGoToLineTool creates a GoToLineTool with the given navigator.
func NewGoToLineTool(nav Navigator) *GoToLineTool {
	return &GoToLineTool{nav: nav}
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

// Execute navigates the editor to the specified line.
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
	if err := t.nav.GoToLine(args.Path, args.Line); err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	return fmt.Sprintf("Navigated to %s line %d.", args.Path, args.Line)
}
