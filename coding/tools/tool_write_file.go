package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/latebit-io/nib/ai/llm"
)

// WriteFileTool lets the LLM create new files in the project. The
// file-created notification flows through the [FileCreator]
// collaborator so the tool never sees the events channel directly.
type WriteFileTool struct {
	workspace FileWriter
	cache     *FileCache
	creator   FileCreator
}

// NewWriteFileTool creates a WriteFileTool with the given dependencies.
// The creator publishes a file-created event after successful writes.
func NewWriteFileTool(ws FileWriter, cache *FileCache, creator FileCreator) *WriteFileTool {
	return &WriteFileTool{workspace: ws, cache: cache, creator: creator}
}

// Definition returns the tool schema for the LLM.
func (t *WriteFileTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        "write_file",
			Description: "Create a new file. Errors if the file already exists — use edit_file for existing files.",
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

// Execute creates a new file, seeds the cache, and publishes a
// file-created event before returning the confirmation message.
//
// Validation order mirrors replace_file so a malformed call is
// rejected with the same shape regardless of which file tool the LLM
// picked: argument-size cap → unmarshal → path required → content
// size cap → project-root containment → write.
func (t *WriteFileTool) Execute(ctx context.Context, call llm.ToolCall) ToolResult {
	if len(call.Function.Arguments) > maxToolArgsBytes {
		return errorResult(fmt.Sprintf("Error: arguments too large (%d bytes, max %d).", len(call.Function.Arguments), maxToolArgsBytes))
	}
	var args writeArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return errorResult(fmt.Sprintf("Error: invalid arguments: %v", err))
	}
	if args.Path == "" {
		return errorResult("Error: path is required")
	}
	if len(args.Content) > maxDiffInputBytes {
		return errorResult(fmt.Sprintf(
			"Error: content too large (%d bytes, max %d). Split the file or use multiple edit_file calls.",
			len(args.Content), maxDiffInputBytes))
	}
	if !inProject(t.workspace, t.workspace.CanonPath(args.Path)) {
		return errorResult(fmt.Sprintf("Error: %s is outside the project root", args.Path))
	}

	if err := t.workspace.WriteFile(args.Path, args.Content); err != nil {
		return errorResult(fmt.Sprintf("Error: %v", err))
	}

	t.cache.Set(t.workspace.CanonPath(args.Path), args.Content)
	t.creator.FileCreated(ctx, args.Path)

	return textResult(fmt.Sprintf("File created: %s", args.Path))
}
