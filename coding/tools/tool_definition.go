package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/engine/lang"
)

// GoToDefinitionTool lets the LLM jump to a symbol's definition via LSP.
type GoToDefinitionTool struct {
	workspace FileReader
	provider  lang.DefinitionProvider
}

// NewGoToDefinitionTool creates a GoToDefinitionTool.
func NewGoToDefinitionTool(ws FileReader, provider lang.DefinitionProvider) *GoToDefinitionTool {
	return &GoToDefinitionTool{workspace: ws, provider: provider}
}

// Definition returns the tool schema for the LLM.
func (t *GoToDefinitionTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        "go_to_definition",
			Description: "Jump to the definition of a symbol at a specific position in a file. Returns the file path and line number where the symbol is defined. Use this to follow function calls, type references, or variable declarations to their source.",
			Parameters: llm.FunctionParams{
				Type: "object",
				Properties: map[string]llm.FunctionParam{
					"path": {
						Type:        "string",
						Description: "File path relative to project root.",
					},
					"line": {
						Type:        "integer",
						Description: "1-indexed line number where the symbol is.",
					},
					"col": {
						Type:        "integer",
						Description: "0-indexed column (character offset) on the line.",
					},
				},
				Required: []string{"path", "line", "col"},
			},
		},
	}
}

// Execute resolves the definition location for the symbol at the given position.
func (t *GoToDefinitionTool) Execute(ctx context.Context, call llm.ToolCall) ToolResult {
	var args posArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return textResult(fmt.Sprintf("Error: invalid arguments: %v", err))
	}
	if errMsg := validatePosArgs(args); errMsg != "" {
		return textResult(errMsg)
	}

	canon := t.workspace.CanonPath(args.Path)
	if !inProject(t.workspace, canon) {
		return textResult("Error: path is outside the project root")
	}

	ctx, cancel := context.WithTimeout(ctx, lspTimeout)
	defer cancel()

	loc, err := t.provider.Definition(ctx, canon, args.Line-1, args.Col)
	if err != nil {
		return textResult(fmt.Sprintf("Error: %v", err))
	}
	if loc.Path == "" {
		return textResult("No definition found.")
	}

	projectRoot := t.workspace.ProjectRoot()
	relPath, relErr := filepath.Rel(projectRoot, loc.Path)
	if relErr != nil {
		relPath = loc.Path
	}

	return textResult(fmt.Sprintf("Definition: %s:%d:%d", relPath, loc.Line+1, loc.Col))
}
