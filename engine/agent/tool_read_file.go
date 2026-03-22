package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/latebit-io/junto/engine/llm"
)

// ReadFileTool lets the LLM read any file in the project.
type ReadFileTool struct {
	workspace Workspace
	cache     *FileCache
}

// NewReadFileTool creates a ReadFileTool with the given dependencies.
func NewReadFileTool(ws Workspace, cache *FileCache) *ReadFileTool {
	return &ReadFileTool{workspace: ws, cache: cache}
}

func (t *ReadFileTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        "read_file",
			Description: "Read the contents of a file. Use a path relative to the project root. Use this before editing to see the exact current state.",
			Parameters: llm.FunctionParams{
				Type: "object",
				Properties: map[string]llm.FunctionParam{
					"path": {
						Type:        "string",
						Description: "File path relative to project root (e.g. \"internal/auth/middleware.go\").",
					},
				},
				Required: []string{"path"},
			},
		},
	}
}

type readArgs struct {
	Path string `json:"path"`
}

func (t *ReadFileTool) Execute(_ context.Context, call llm.ToolCall) string {
	var args readArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("Error: invalid arguments: %v", err)
	}
	if args.Path == "" {
		return "Error: path is required"
	}

	canon := t.workspace.CanonPath(args.Path)

	// Check cache first
	if content, ok := t.cache.Get(canon); ok {
		return content
	}

	// Read from workspace (disk)
	content, err := t.workspace.ReadFile(args.Path)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}

	t.cache.Set(canon, content)
	return content
}
