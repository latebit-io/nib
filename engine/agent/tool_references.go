package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/latebit-io/junto/ai/llm"
	"github.com/latebit-io/junto/engine/lang"
)

// FindReferencesTool lets the LLM find all references to a symbol.
type FindReferencesTool struct {
	workspace FileReader
	provider  lang.ReferenceProvider
}

// NewFindReferencesTool creates a FindReferencesTool.
func NewFindReferencesTool(ws FileReader, provider lang.ReferenceProvider) *FindReferencesTool {
	return &FindReferencesTool{workspace: ws, provider: provider}
}

// Definition returns the tool schema for the LLM.
func (t *FindReferencesTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        "find_references",
			Description: "Find all references to the symbol at a specific position in a file. Returns file paths and line numbers of every usage. Use this to understand the impact of a change before editing.",
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

// Execute runs the references lookup.
func (t *FindReferencesTool) Execute(ctx context.Context, call llm.ToolCall) ToolResult {
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

	locs, err := t.provider.References(ctx, canon, args.Line-1, args.Col)
	if err != nil {
		return textResult(fmt.Sprintf("Error: %v", err))
	}
	if len(locs) == 0 {
		return textResult("No references found.")
	}

	projectRoot := t.workspace.ProjectRoot()
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d reference(s) found:\n\n", len(locs))
	for _, loc := range locs {
		relPath, relErr := filepath.Rel(projectRoot, loc.Path)
		if relErr != nil {
			relPath = loc.Path
		}
		fmt.Fprintf(&sb, "%s:%d:%d\n", relPath, loc.Line+1, loc.Col)
	}
	return textResult(truncateForPreview(sb.String()))
}
