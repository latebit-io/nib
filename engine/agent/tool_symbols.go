package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/latebit-io/junto/engine/lang"
	"github.com/latebit-io/junto/engine/llm"
)

// WorkspaceSymbolsTool lets the LLM search for symbols by name across the project.
type WorkspaceSymbolsTool struct {
	workspace Workspace
	provider  lang.SymbolProvider
}

// NewWorkspaceSymbolsTool creates a WorkspaceSymbolsTool.
func NewWorkspaceSymbolsTool(ws Workspace, provider lang.SymbolProvider) *WorkspaceSymbolsTool {
	return &WorkspaceSymbolsTool{workspace: ws, provider: provider}
}

// Definition returns the tool schema for the LLM.
func (t *WorkspaceSymbolsTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        "workspace_symbols",
			Description: "Search for symbols (functions, types, variables, constants) by name across the project. Returns symbol locations with their kind. Use this to find where types or functions are defined.",
			Parameters: llm.FunctionParams{
				Type: "object",
				Properties: map[string]llm.FunctionParam{
					"query": {
						Type:        "string",
						Description: "Symbol name or partial name to search for (e.g. \"Session\", \"ApplyEdit\").",
					},
				},
				Required: []string{"query"},
			},
		},
	}
}

type symbolArgs struct {
	Query string `json:"query"`
}

// Execute runs the workspace symbol search.
func (t *WorkspaceSymbolsTool) Execute(ctx context.Context, call llm.ToolCall) string {
	var args symbolArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("Error: invalid arguments: %v", err)
	}
	if args.Query == "" {
		return "Error: query is required"
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	symbols, err := t.provider.WorkspaceSymbols(ctx, args.Query)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	if len(symbols) == 0 {
		return "No symbols found."
	}

	projectRoot := t.workspace.ProjectRoot()
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d symbol(s) found:\n\n", len(symbols))
	for _, sym := range symbols {
		relPath, relErr := filepath.Rel(projectRoot, sym.Path)
		if relErr != nil {
			relPath = sym.Path
		}
		fmt.Fprintf(&sb, "[%s] %s — %s:%d\n", sym.Kind, sym.Name, relPath, sym.Line+1)
	}

	out := sb.String()
	if len(out) > maxContentPreview {
		out = out[:maxContentPreview] + "\n... (truncated)"
	}
	return out
}
