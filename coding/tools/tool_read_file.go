package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit/tools/truncate"
)

// ReadFileTool lets the LLM read any file in the project.
// Supports optional line-range parameters to read specific sections,
// reducing token usage on large files.
type ReadFileTool struct {
	workspace FileReader
	cache     *FileCache
	stash     truncate.Sink
}

// NewReadFileTool creates a ReadFileTool with the given dependencies.
func NewReadFileTool(ws FileReader, cache *FileCache) *ReadFileTool {
	return &ReadFileTool{workspace: ws, cache: cache}
}

// SetStash attaches an optional [truncate.Sink] for over-cap full-file
// reads. Nil clears any previously-attached sink. Composition root only.
func (t *ReadFileTool) SetStash(sink truncate.Sink) {
	t.stash = sink
}

// Definition returns the tool schema for the LLM.
func (t *ReadFileTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        "read_file",
			Description: "Read file contents. Path is project-relative. Use offset+limit for large files.",
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
	if args.Offset < 0 || args.Limit < 0 {
		return textResult("Error: offset and limit must be non-negative")
	}

	content, err := t.loadContent(args.Path)
	if err != nil {
		return textResult(fmt.Sprintf("Error: %v", err))
	}

	if args.Offset > 0 || args.Limit > 0 {
		return textResult(sliceLines(content, args.Path, args.Offset, args.Limit))
	}
	// Full-file reads cap at the truncate contract so a 100 KiB file
	// doesn't poison every subsequent turn. The marker steers the LLM
	// toward offset/limit on re-query and includes a stash path when
	// one is wired.
	out, _ := truncate.Bytes("read_"+sanitizeReadLabel(args.Path), content, truncate.DefaultMaxBytes, t.stash)
	return textResult(out)
}

// sanitizeReadLabel produces a short filesystem-safe suffix from a
// project-relative path so concurrent stashes for different files are
// easy to tell apart on disk. ProjectStash sanitizes further; this
// pre-pass just keeps the prefix readable.
func sanitizeReadLabel(path string) string {
	// Keep only the basename; drop directory separators and dots.
	base := path
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimPrefix(base, ".")
	return base
}

// maxFileSize is the largest file we'll read and cache. Matches the
// maxDiffInputBytes cap used by edit_file. Files beyond this are too
// large for useful LLM context and risk unbounded memory growth.
const maxFileSize = 10 * 1024 * 1024 // 10 MB

// loadContent returns file content from cache or disk, populating the cache on miss.
// The closure captures the original relative path so ReadFile receives the
// documented relative-path input while the cache is keyed by canonical path.
func (t *ReadFileTool) loadContent(path string) (string, error) {
	canon := t.workspace.CanonPath(path)
	content, err := t.cache.LoadOrRead(canon, func() (string, error) {
		return t.workspace.ReadFile(path)
	})
	if err != nil {
		return "", err
	}
	if len(content) > maxFileSize {
		t.cache.Invalidate(canon) // don't retain oversized files in cache
		return "", fmt.Errorf("file too large (%d bytes, max %d)", len(content), maxFileSize)
	}
	return content, nil
}

// sliceLines extracts a line range from content and formats it with line numbers.
// Output is capped at maxContentPreview bytes to stay consistent with other tools.
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
	fmt.Fprintf(&b, "Lines %d–%d of %d in %s\n", start, endIdx, total, path)
	for i := startIdx; i < endIdx; i++ {
		fmt.Fprintf(&b, "%4d\t%s\n", i+1, lines[i])
		if b.Len() > maxContentPreview {
			fmt.Fprintf(&b, "\n... truncated at %d bytes (use a smaller range)\n", b.Len())
			break
		}
	}
	return b.String()
}
