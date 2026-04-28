package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/latebit-io/junto/ai/llm"
)

// ListFilesTool lets the LLM see the project file structure.
type ListFilesTool struct {
	workspace FileReader
}

// NewListFilesTool creates a ListFilesTool with the given workspace.
func NewListFilesTool(ws FileReader) *ListFilesTool {
	return &ListFilesTool{workspace: ws}
}

// Definition returns the tool schema for the LLM.
func (t *ListFilesTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        "list_files",
			Description: "List all files in the project (respects .gitignore). Returns paths relative to the project root. Use this to discover files before reading or editing them.",
			Parameters: llm.FunctionParams{
				Type:       "object",
				Properties: map[string]llm.FunctionParam{},
				Required:   []string{},
			},
		},
	}
}

// Execute lists all project files, filtering out .project/ metadata.
func (t *ListFilesTool) Execute(_ context.Context, _ llm.ToolCall) ToolResult {
	files, err := t.workspace.ListFiles()
	if err != nil {
		return textResult(fmt.Sprintf("Error: %v", err))
	}
	// Filter out .project/ — project metadata, not source files.
	filtered := files[:0]
	for _, f := range files {
		if !strings.HasPrefix(f, ".project/") && f != ".project" {
			filtered = append(filtered, f)
		}
	}
	return textResult(strings.Join(filtered, "\n"))
}
