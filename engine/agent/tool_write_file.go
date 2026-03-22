package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/latebit-io/junto/engine/llm"
)

// WriteFileTool lets the LLM create new files in the project.
type WriteFileTool struct {
	workspace Workspace
	cache     *FileCache
	send      func(Event)
}

// NewWriteFileTool creates a WriteFileTool with the given dependencies.
func NewWriteFileTool(ws Workspace, cache *FileCache, send func(Event)) *WriteFileTool {
	return &WriteFileTool{workspace: ws, cache: cache, send: send}
}

func (t *WriteFileTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        "write_file",
			Description: "Create a new file with the given content. Use this only for files that do not exist yet. For existing files, use edit_file.",
			Parameters: llm.FunctionParams{
				Type: "object",
				Properties: map[string]llm.FunctionParam{
					"path": {
						Type:        "string",
						Description: "File path relative to project root.",
					},
					"content": {
						Type:        "string",
						Description: "Full content of the new file.",
					},
					"reason": {
						Type:        "string",
						Description: "Brief explanation of why this file is needed.",
					},
				},
				Required: []string{"path", "content", "reason"},
			},
		},
	}
}

type writeArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Reason  string `json:"reason"`
}

func (t *WriteFileTool) Execute(_ context.Context, call llm.ToolCall) string {
	var args writeArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("Error: invalid arguments: %v", err)
	}
	if args.Path == "" {
		return "Error: path is required"
	}

	if err := t.workspace.WriteFile(args.Path, args.Content); err != nil {
		return fmt.Sprintf("Error: %v", err)
	}

	t.cache.Set(t.workspace.CanonPath(args.Path), args.Content)
	t.send(FileCreatedEvent{Path: args.Path})

	return fmt.Sprintf("File created: %s", args.Path)
}
