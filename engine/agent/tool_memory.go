package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/latebit-io/junto/engine/llm"
	"github.com/latebit-io/junto/engine/memory"
)

// --- memory_fetch ---

// MemoryFetchTool retrieves a memory document by path.
type MemoryFetchTool struct {
	store memory.Store
}

// NewMemoryFetchTool creates a MemoryFetchTool backed by the given store.
func NewMemoryFetchTool(store memory.Store) *MemoryFetchTool {
	return &MemoryFetchTool{store: store}
}

type memoryFetchArgs struct {
	Path string `json:"path"`
}

// Definition returns the tool schema for the LLM.
func (t *MemoryFetchTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name: "memory_fetch",
			Description: "Fetch a memory document by path. Use this to retrieve project context, " +
				"architecture decisions, session history, or any structured knowledge persisted across sessions.",
			Parameters: llm.FunctionParams{
				Type: "object",
				Properties: map[string]llm.FunctionParam{
					"path": {
						Type:        "string",
						Description: "Document path, e.g. /index.md, /architecture.md",
					},
				},
				Required: []string{"path"},
			},
		},
	}
}

// Execute fetches a memory document and returns its content.
func (t *MemoryFetchTool) Execute(ctx context.Context, call llm.ToolCall) ToolResult {
	if ctx.Err() != nil {
		return textResult("Error: agent canceled")
	}
	var args memoryFetchArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return textResult(fmt.Sprintf("Error: invalid arguments: %v", err))
	}
	if args.Path == "" {
		return textResult("Error: path is required")
	}

	doc, err := t.store.Fetch(args.Path)
	if err != nil {
		return textResult(fmt.Sprintf("Error: %v", err))
	}
	return textResult(fmt.Sprintf("version=%d modified=%s\n\n%s", doc.Version, doc.Modified, doc.Body))
}

// --- memory_publish ---

// MemoryPublishTool creates or updates a memory document.
type MemoryPublishTool struct {
	store memory.Store
}

// NewMemoryPublishTool creates a MemoryPublishTool backed by the given store.
func NewMemoryPublishTool(store memory.Store) *MemoryPublishTool {
	return &MemoryPublishTool{store: store}
}

type memoryPublishArgs struct {
	Path            string `json:"path"`
	Body            string `json:"body"`
	ExpectedVersion int    `json:"expected_version"`
}

// Definition returns the tool schema for the LLM.
func (t *MemoryPublishTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name: "memory_publish",
			Description: "Create or update a memory document. Use this to persist architecture decisions, " +
				"design specs, or project state. Requires expected_version for conflict detection " +
				"(0 = create new, N = update version N).",
			Parameters: llm.FunctionParams{
				Type: "object",
				Properties: map[string]llm.FunctionParam{
					"path": {
						Type:        "string",
						Description: "Document path, e.g. /architecture.md",
					},
					"body": {
						Type:        "string",
						Description: "Markdown content",
					},
					"expected_version": {
						Type:        "integer",
						Description: "0 to create, or current version number to update",
					},
				},
				Required: []string{"path", "body", "expected_version"},
			},
		},
	}
}

// Execute publishes a memory document and returns the result.
func (t *MemoryPublishTool) Execute(ctx context.Context, call llm.ToolCall) ToolResult {
	if ctx.Err() != nil {
		return textResult("Error: agent canceled")
	}
	var args memoryPublishArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return textResult(fmt.Sprintf("Error: invalid arguments: %v", err))
	}
	if args.Path == "" {
		return textResult("Error: path is required")
	}
	if args.Body == "" {
		return textResult("Error: body is required")
	}

	doc, err := t.store.Publish(args.Path, args.Body, args.ExpectedVersion)
	if err != nil {
		return textResult(fmt.Sprintf("Error: %v", err))
	}
	return textResult(fmt.Sprintf("Published %s (version=%d)", doc.Path, doc.Version))
}

// --- memory_append ---

// MemoryAppendTool appends content to an existing memory document.
type MemoryAppendTool struct {
	store memory.Store
}

// NewMemoryAppendTool creates a MemoryAppendTool backed by the given store.
func NewMemoryAppendTool(store memory.Store) *MemoryAppendTool {
	return &MemoryAppendTool{store: store}
}

type memoryAppendArgs struct {
	Path            string `json:"path"`
	Body            string `json:"body"`
	ExpectedVersion int    `json:"expected_version"`
}

// Definition returns the tool schema for the LLM.
func (t *MemoryAppendTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name: "memory_append",
			Description: "Append content to an existing memory document. Use this for journal entries, " +
				"incremental notes, or adding to a running log. Requires expected_version >= 1.",
			Parameters: llm.FunctionParams{
				Type: "object",
				Properties: map[string]llm.FunctionParam{
					"path": {
						Type:        "string",
						Description: "Document path, e.g. /journal.md",
					},
					"body": {
						Type:        "string",
						Description: "Content to append",
					},
					"expected_version": {
						Type:        "integer",
						Description: "Current version number (must be >= 1)",
					},
				},
				Required: []string{"path", "body", "expected_version"},
			},
		},
	}
}

// Execute appends content to a memory document and returns the result.
func (t *MemoryAppendTool) Execute(ctx context.Context, call llm.ToolCall) ToolResult {
	if ctx.Err() != nil {
		return textResult("Error: agent canceled")
	}
	var args memoryAppendArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return textResult(fmt.Sprintf("Error: invalid arguments: %v", err))
	}
	if args.Path == "" {
		return textResult("Error: path is required")
	}
	if args.Body == "" {
		return textResult("Error: body is required")
	}
	if args.ExpectedVersion < 1 {
		return textResult("Error: expected_version must be >= 1 (document must exist)")
	}

	doc, err := t.store.Append(args.Path, args.Body, args.ExpectedVersion)
	if err != nil {
		return textResult(fmt.Sprintf("Error: %v", err))
	}
	return textResult(fmt.Sprintf("Appended to %s (version=%d)", doc.Path, doc.Version))
}

// --- memory_list ---

// MemoryListTool lists memory documents under a directory path.
type MemoryListTool struct {
	store memory.Store
}

// NewMemoryListTool creates a MemoryListTool backed by the given store.
func NewMemoryListTool(store memory.Store) *MemoryListTool {
	return &MemoryListTool{store: store}
}

type memoryListArgs struct {
	Path string `json:"path"`
}

// Definition returns the tool schema for the LLM.
func (t *MemoryListTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name: "memory_list",
			Description: "List memory documents under a directory path. " +
				"Use this to discover what knowledge has been persisted.",
			Parameters: llm.FunctionParams{
				Type: "object",
				Properties: map[string]llm.FunctionParam{
					"path": {
						Type:        "string",
						Description: "Directory path, e.g. / or /designs/",
					},
				},
				Required: []string{"path"},
			},
		},
	}
}

// Execute lists memory documents and returns their paths.
func (t *MemoryListTool) Execute(ctx context.Context, call llm.ToolCall) ToolResult {
	if ctx.Err() != nil {
		return textResult("Error: agent canceled")
	}
	var args memoryListArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return textResult(fmt.Sprintf("Error: invalid arguments: %v", err))
	}
	if args.Path == "" {
		return textResult("Error: path is required")
	}

	paths, err := t.store.List(args.Path)
	if err != nil {
		return textResult(fmt.Sprintf("Error: %v", err))
	}
	if len(paths) == 0 {
		return textResult("No documents found.")
	}
	return textResult(strings.Join(paths, "\n"))
}
