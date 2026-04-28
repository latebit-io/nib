package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/latebit-io/junto/ai/llm"
	"github.com/latebit-io/junto/engine/memory"
	"github.com/latebit-io/junto/engine/project"
)

// ProjectWorkTreePath is the canonical demarkus path of the strict
// project.md document. Writes to this path are validated against the
// schema in [project.Validate] before reaching the store.
const ProjectWorkTreePath = "/project.md"

// validateMemoryPath trims whitespace and requires a leading slash.
// Returns the cleaned path or an error result.
func validateMemoryPath(raw string) (string, *ToolResult) {
	p := strings.TrimSpace(raw)
	if p == "" {
		r := textResult("Error: path is required")
		return "", &r
	}
	if p[0] != '/' {
		r := textResult("Error: path must be absolute (start with /)")
		return "", &r
	}
	return p, nil
}

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
							"Matches any heading level (#, ##, ###, etc.) case-insensitively. " +
							"Returns the heading and all content up to the next heading of equal or higher level. " +
							"Omit to fetch the full document.",
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
	path, errResult := validateMemoryPath(args.Path)
	if errResult != nil {
		return *errResult
	}

	doc, err := t.store.Fetch(ctx, path)
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
// Works at any heading level (#, ##, ###, etc.). Returns the heading line and
// all content up to the next heading of equal or higher level (fewer #'s),
// or end of document. Subsections are included in the result.
func extractSection(body, name string) (string, bool) {
	target := strings.ToLower(strings.TrimSpace(name))
	var result strings.Builder
	found := false
	matchLevel := 0

	for line := range strings.Lines(body) {
		level := headingLevel(line)
		if level > 0 {
			if found && level <= matchLevel {
				// Hit a heading at same or higher level — stop collecting.
				break
			}
			if !found {
				headingText := strings.TrimSpace(strings.TrimLeft(line, "#"))
				if strings.ToLower(headingText) == target {
					found = true
					matchLevel = level
				}
			}
		}
		if found {
			result.WriteString(line)
			result.WriteByte('\n')
		}
	}
	return result.String(), found
}

// listSections returns all markdown headings found in the document body,
// indented by level to show hierarchy.
func listSections(body string) string {
	var sections strings.Builder
	for line := range strings.Lines(body) {
		level := headingLevel(line)
		if level == 0 {
			continue
		}
		heading := strings.TrimSpace(strings.TrimLeft(line, "#"))
		// Indent subsections: # = no indent, ## = 2 spaces, ### = 4 spaces, etc.
		for range level - 1 {
			sections.WriteString("  ")
		}
		sections.WriteString("- ")
		sections.WriteString(heading)
		sections.WriteByte('\n')
	}
	if sections.Len() == 0 {
		return "(no sections found)"
	}
	return sections.String()
}

// headingLevel returns the markdown heading level (1 for #, 2 for ##, etc.)
// or 0 if the line is not a heading.
func headingLevel(line string) int {
	level := 0
	for _, c := range line {
		if c == '#' {
			level++
		} else {
			break
		}
	}
	// Must have at least one # followed by a space.
	if level > 0 && len(line) > level && line[level] == ' ' {
		return level
	}
	return 0
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

// memoryWriteArgs holds the JSON-decoded arguments shared by memory_publish
// and memory_append (identical field sets).
type memoryWriteArgs struct {
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
	var args memoryWriteArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return textResult(fmt.Sprintf("Error: invalid arguments: %v", err))
	}
	path, errResult := validateMemoryPath(args.Path)
	if errResult != nil {
		return *errResult
	}
	if args.Body == "" {
		return textResult("Error: body is required")
	}
	if args.ExpectedVersion < 0 {
		return textResult("Error: expected_version must be >= 0 (0 = create, >0 = update)")
	}

	if path == ProjectWorkTreePath {
		if errs := project.Validate(project.Parse(args.Body)); errs != nil {
			return textResult(fmt.Sprintf(
				"Error: %s rejected — schema violations below. Fix all and retry.\n%s",
				ProjectWorkTreePath,
				errs.Error(),
			))
		}
	}

	doc, err := t.store.Publish(ctx, path, args.Body, args.ExpectedVersion)
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
	var args memoryWriteArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return textResult(fmt.Sprintf("Error: invalid arguments: %v", err))
	}
	path, errResult := validateMemoryPath(args.Path)
	if errResult != nil {
		return *errResult
	}
	if args.Body == "" {
		return textResult("Error: body is required")
	}
	if args.ExpectedVersion < 1 {
		return textResult("Error: expected_version must be >= 1 (document must exist)")
	}

	if path == ProjectWorkTreePath {
		return textResult(fmt.Sprintf(
			"Error: raw append to %s is not allowed — it would break the strict schema. "+
				"Use project_task_add (for new tasks) or update_task (to activate/complete) instead.",
			ProjectWorkTreePath,
		))
	}

	doc, err := t.store.Append(ctx, path, args.Body, args.ExpectedVersion)
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
	path, errResult := validateMemoryPath(args.Path)
	if errResult != nil {
		return *errResult
	}

	paths, err := t.store.List(ctx, path)
	if err != nil {
		return textResult(fmt.Sprintf("Error: %v", err))
	}
	if len(paths) == 0 {
		return textResult("No documents found.")
	}
	return textResult(strings.Join(paths, "\n"))
}
