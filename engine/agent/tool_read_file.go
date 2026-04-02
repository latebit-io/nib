package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/latebit-io/junto/engine/llm"
)

// ReadFileTool lets the LLM read any file in the project.
// Supports optional line-range parameters to read specific sections,
// reducing token usage on large files.
type ReadFileTool struct {
	workspace Workspace
	cache     *FileCache
}

// NewReadFileTool creates a ReadFileTool with the given dependencies.
func NewReadFileTool(ws Workspace, cache *FileCache) *ReadFileTool {
	return &ReadFileTool{workspace: ws, cache: cache}
}

// Definition returns the tool schema for the LLM.
func (t *ReadFileTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name: "read_file",
			Description: "Read the contents of a file. Use a path relative to the project root. " +
				"Use this before editing to see the exact current state. " +
				"For large files, use offset and limit to read specific sections.",
			Parameters: llm.FunctionParams{
				Type: "object",
				Properties: map[string]llm.FunctionParam{
					"path": {
						Type:        "string",
						Description: "File path relative to project root (e.g. \"internal/auth/middleware.go\").",
					},
					"offset": {
						Type:        "integer",
						Description: "Line number to start reading from (1-indexed). Only use when the file is too large to read at once.",
					},
					"limit": {
						Type:        "integer",
						Description: "Number of lines to read from the offset. Only use when the file is too large to read at once.",
					},
				},
				Required: []string{"path"},
			},
		},
	}
}

type readArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

// Execute reads a file from cache or disk and returns its content.
// When offset or limit are provided, returns a line-numbered slice
// with a metadata header showing the range and total line count.
func (t *ReadFileTool) Execute(_ context.Context, call llm.ToolCall) ToolResult {
	var args readArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return textResult(fmt.Sprintf("Error: invalid arguments: %v", err))
	}
	if args.Path == "" {
		return textResult("Error: path is required")
	}

	content, err := t.loadContent(args.Path)
	if err != nil {
		return textResult(fmt.Sprintf("Error: %v", err))
	}

	if args.Offset > 0 || args.Limit > 0 {
		return textResult(sliceLines(content, args.Path, args.Offset, args.Limit))
	}
	return textResult(content)
}

// loadContent returns file content from cache or disk, populating the cache on miss.
func (t *ReadFileTool) loadContent(path string) (string, error) {
	canon := t.workspace.CanonPath(path)

	if content, ok := t.cache.Get(canon); ok {
		slog.Debug("read_file: cache hit", "path", path, "content_len", len(content))
		return content, nil
	}

	content, err := t.workspace.ReadFile(path)
	if err != nil {
		return "", err
	}

	slog.Debug("read_file: read from disk", "path", path, "content_len", len(content))
	t.cache.Set(canon, content)
	return content, nil
}

// sliceLines extracts a line range from content and formats it with line numbers.
// offset is 1-indexed (0 is treated as 1). limit is the number of lines to return.
func sliceLines(content, path string, offset, limit int) string {
	lines := strings.Split(content, "\n")
	total := len(lines)

	start := offset
	if start <= 0 {
		start = 1
	}
	startIdx := start - 1
	if startIdx >= total {
		return fmt.Sprintf("Error: offset %d exceeds file length (%d lines)", start, total)
	}

	endIdx := total
	if limit > 0 {
		endIdx = startIdx + limit
		if endIdx > total {
			endIdx = total
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Lines %d–%d of %d in %s\n", start, startIdx+(endIdx-startIdx), total, path)
	for i := startIdx; i < endIdx; i++ {
		fmt.Fprintf(&b, "%4d\t%s\n", i+1, lines[i])
	}
	return b.String()
}
