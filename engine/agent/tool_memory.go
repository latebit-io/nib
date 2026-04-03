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
	Path    string `json:"path"`
	Section string `json:"section"`
}

// Definition returns the tool schema for the LLM.
func (t *MemoryFetchTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name: "memory_fetch",
			Description: "Fetch a memory document by path. Use this to retrieve project context, " +
				"architecture decisions, session history, or any structured knowledge persisted across sessions. " +
				"Use the optional section parameter to fetch only a specific heading's content " +
				"(e.g. section=\"Current State\" returns only that section). " +
				"This reduces context size when you only need part of a large document.",
			Parameters: llm.FunctionParams{
				Type: "object",
				Properties: map[string]llm.FunctionParam{
					"path": {
						Type:        "string",
						Description: "Document path, e.g. /index.md, /architecture.md",
					},
					"section": {
						Type: "string",
						Description: "Optional heading name to extract a single section (e.g. \"Current State\"). " +
							"Matches ## headings case-insensitively. Omit to fetch the full document.",
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

	doc, err := t.store.Fetch(ctx, args.Path)
	if err != nil {
		return textResult(fmt.Sprintf("Error: %v", err))
	}

	body := doc.Body
	if args.Section != "" {
		section, ok := extractSection(body, args.Section)
		if !ok {
			return textResult(fmt.Sprintf("version=%d modified=%s\n\nSection %q not found. Available sections:\n%s",
				doc.Version, doc.Modified, args.Section, listSections(body)))
		}
		body = section
	}

	return textResult(fmt.Sprintf("version=%d modified=%s\n\n%s", doc.Version, doc.Modified, body))
}

// extractSection finds a markdown section by heading name (case-insensitive).
// Matches ## headings (level 2). Returns the heading line and all content
// up to the next heading of equal or higher level, or end of document.
func extractSection(body, name string) (string, bool) {
	target := strings.ToLower(strings.TrimSpace(name))
	var result strings.Builder
	found := false

	for line := range strings.Lines(body) {
		if isHeading(line) {
			if found {
				// Hit the next heading — stop collecting.
				break
			}
			headingText := strings.TrimSpace(strings.TrimLeft(line, "#"))
			if strings.ToLower(headingText) == target {
				found = true
				result.WriteString(line)
				result.WriteByte('\n')
			}
			continue
		}
		if found {
			result.WriteString(line)
			result.WriteByte('\n')
		}
	}
	return result.String(), found
}

// listSections returns the ## headings found in the document body.
func listSections(body string) string {
	var sections strings.Builder
	for line := range strings.Lines(body) {
		if isHeading(line) {
			heading := strings.TrimSpace(strings.TrimLeft(line, "#"))
			sections.WriteString("- ")
			sections.WriteString(heading)
			sections.WriteByte('\n')
		}
	}
	if sections.Len() == 0 {
		return "(no sections found)"
	}
	return sections.String()
}

// isHeading returns true if the line is a markdown heading (## level 2 or higher).
func isHeading(line string) bool {
	return strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "# ")
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
	if args.ExpectedVersion < 0 {
		return textResult("Error: expected_version must be >= 0 (0 = create, >0 = update)")
	}

	doc, err := t.store.Publish(ctx, args.Path, args.Body, args.ExpectedVersion)
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

	doc, err := t.store.Append(ctx, args.Path, args.Body, args.ExpectedVersion)
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

	paths, err := t.store.List(ctx, args.Path)
	if err != nil {
		return textResult(fmt.Sprintf("Error: %v", err))
	}
	if len(paths) == 0 {
		return textResult("No documents found.")
	}
	return textResult(strings.Join(paths, "\n"))
}
