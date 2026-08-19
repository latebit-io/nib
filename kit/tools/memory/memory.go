// Package memory provides kit-generic LLM tools for structured,
// versioned memory access. The four tools (Fetch, Publish, Append,
// List) operate against any [memory.Store] adapter; coding-specific
// schema enforcement (e.g. /project.md task tree) is layered on top
// via the [Validator] hook on Publish and Append rather than being
// hardcoded into the tool itself, so non-coding kit consumers
// (research agents, ops agents) can reuse the tools without paying
// for coding's path conventions.
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit/memory"
)

// Validator is called with (path, body) before a write operation
// reaches the [memory.Store]. Returning a non-nil error blocks the
// write and surfaces err.Error() to the LLM as the tool result body;
// returning nil allows the operation. Used by [PublishTool] and
// [AppendTool] to enforce path-specific schemas or block forbidden
// operations on canonical paths (a coding agent's /project.md is the
// canonical example).
//
// The validator runs on the agent's run goroutine, synchronously
// before the store is called; it must not block on external I/O.
type Validator func(path, body string) error

// Option configures a memory write tool ([PublishTool] or
// [AppendTool]) at construction time. Use [WithValidator] to install
// a write-blocking hook.
type Option func(*options)

type options struct {
	validator Validator
}

// WithValidator installs a [Validator] that runs before each write.
// Passing nil clears any previously-set validator.
func WithValidator(v Validator) Option { return func(o *options) { o.validator = v } }

func applyOptions(opts []Option) options {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// validateMemoryPath trims whitespace and requires a leading slash.
// Returns the cleaned path or an error result.
func validateMemoryPath(raw string) (string, *agent.ToolResult) {
	p := strings.TrimSpace(raw)
	if p == "" {
		r := errorResult("Error: path is required")
		return "", &r
	}
	if p[0] != '/' {
		r := errorResult("Error: path must be absolute (start with /)")
		return "", &r
	}
	return p, nil
}

// textResult builds a successful tool result whose body is the given
// string. Local helper so the package depends only on agent.ToolResult.
func textResult(content string) agent.ToolResult {
	return agent.ToolResult{Content: content}
}

// errorResult builds a failed tool result so the agent loop can tell a
// refused or failed call from a normal reply (matches bash/git/search).
func errorResult(content string) agent.ToolResult {
	return agent.ToolResult{Content: content, IsError: true}
}

// --- memory_fetch ---

// FetchTool retrieves a memory document by path.
type FetchTool struct {
	store memory.Store
}

// NewFetchTool creates a FetchTool backed by the given store.
func NewFetchTool(store memory.Store) *FetchTool {
	return &FetchTool{store: store}
}

type memoryFetchArgs struct {
	Path    string `json:"path"`
	Section string `json:"section"`
}

// Definition returns the tool schema for the LLM.
func (t *FetchTool) Definition() llm.ToolDef {
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
func (t *FetchTool) Execute(ctx context.Context, call llm.ToolCall) agent.ToolResult {
	if ctx.Err() != nil {
		return errorResult("Error: agent canceled")
	}
	var args memoryFetchArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return errorResult(fmt.Sprintf("Error: invalid arguments: %v", err))
	}
	path, errResult := validateMemoryPath(args.Path)
	if errResult != nil {
		return *errResult
	}

	doc, err := t.store.Fetch(ctx, path)
	if err != nil {
		return errorResult(fmt.Sprintf("Error: %v", err))
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

// PublishTool creates or updates a memory document. An optional
// [Validator] (set via [WithValidator]) runs synchronously before the
// store is called; non-nil errors block the write.
type PublishTool struct {
	store     memory.Store
	validator Validator
}

// NewPublishTool creates a PublishTool backed by the given store.
// Pass [WithValidator] to install a path-specific schema check or
// write block.
func NewPublishTool(store memory.Store, opts ...Option) *PublishTool {
	o := applyOptions(opts)
	return &PublishTool{store: store, validator: o.validator}
}

// memoryWriteArgs holds the JSON-decoded arguments shared by memory_publish
// and memory_append (identical field sets).
type memoryWriteArgs struct {
	Path            string `json:"path"`
	Body            string `json:"body"`
	ExpectedVersion int    `json:"expected_version"`
}

// Definition returns the tool schema for the LLM.
func (t *PublishTool) Definition() llm.ToolDef {
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
func (t *PublishTool) Execute(ctx context.Context, call llm.ToolCall) agent.ToolResult {
	if ctx.Err() != nil {
		return errorResult("Error: agent canceled")
	}
	var args memoryWriteArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return errorResult(fmt.Sprintf("Error: invalid arguments: %v", err))
	}
	path, errResult := validateMemoryPath(args.Path)
	if errResult != nil {
		return *errResult
	}
	if args.Body == "" {
		return errorResult("Error: body is required")
	}
	if args.ExpectedVersion < 0 {
		return errorResult("Error: expected_version must be >= 0 (0 = create, >0 = update)")
	}

	if t.validator != nil {
		if err := t.validator(path, args.Body); err != nil {
			return errorResult(err.Error())
		}
	}

	doc, err := t.store.Publish(ctx, path, args.Body, args.ExpectedVersion)
	if err != nil {
		return errorResult(fmt.Sprintf("Error: %v", err))
	}
	return textResult(fmt.Sprintf("Published %s (version=%d)", doc.Path, doc.Version))
}

// --- memory_append ---

// AppendTool appends content to an existing memory document. An
// optional [Validator] (set via [WithValidator]) runs synchronously
// before the store is called; non-nil errors block the append.
type AppendTool struct {
	store     memory.Store
	validator Validator
}

// NewAppendTool creates an AppendTool backed by the given store.
// Pass [WithValidator] to install a path-specific schema check or
// write block.
func NewAppendTool(store memory.Store, opts ...Option) *AppendTool {
	o := applyOptions(opts)
	return &AppendTool{store: store, validator: o.validator}
}

// Definition returns the tool schema for the LLM.
func (t *AppendTool) Definition() llm.ToolDef {
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
func (t *AppendTool) Execute(ctx context.Context, call llm.ToolCall) agent.ToolResult {
	if ctx.Err() != nil {
		return errorResult("Error: agent canceled")
	}
	var args memoryWriteArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return errorResult(fmt.Sprintf("Error: invalid arguments: %v", err))
	}
	path, errResult := validateMemoryPath(args.Path)
	if errResult != nil {
		return *errResult
	}
	if args.Body == "" {
		return errorResult("Error: body is required")
	}
	if args.ExpectedVersion < 1 {
		return errorResult("Error: expected_version must be >= 1 (document must exist)")
	}

	if t.validator != nil {
		if err := t.validator(path, args.Body); err != nil {
			return errorResult(err.Error())
		}
	}

	doc, err := t.store.Append(ctx, path, args.Body, args.ExpectedVersion)
	if err != nil {
		return errorResult(fmt.Sprintf("Error: %v", err))
	}
	return textResult(fmt.Sprintf("Appended to %s (version=%d)", doc.Path, doc.Version))
}

// --- memory_list ---

// ListTool lists memory documents under a directory path.
type ListTool struct {
	store memory.Store
}

// NewListTool creates a ListTool backed by the given store.
func NewListTool(store memory.Store) *ListTool {
	return &ListTool{store: store}
}

type memoryListArgs struct {
	Path string `json:"path"`
}

// Definition returns the tool schema for the LLM.
func (t *ListTool) Definition() llm.ToolDef {
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
func (t *ListTool) Execute(ctx context.Context, call llm.ToolCall) agent.ToolResult {
	if ctx.Err() != nil {
		return errorResult("Error: agent canceled")
	}
	var args memoryListArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return errorResult(fmt.Sprintf("Error: invalid arguments: %v", err))
	}
	path, errResult := validateMemoryPath(args.Path)
	if errResult != nil {
		return *errResult
	}

	paths, err := t.store.List(ctx, path)
	if err != nil {
		return errorResult(fmt.Sprintf("Error: %v", err))
	}
	if len(paths) == 0 {
		return textResult("No documents found.")
	}
	return textResult(strings.Join(paths, "\n"))
}
