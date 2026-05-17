// Package search provides a kit-generic project search tool. The tool
// wraps a caller-supplied [SearchFunc] so the search backend is
// injected at construction — kit carries no engine/ dependency.
// Any kit-based agent can register it with whatever search
// implementation fits (ripgrep wrapper, LSP workspace/symbol, etc.).
package search

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/ai/llm"
)

// maxPreviewBytes caps the result text returned to the LLM.
const maxPreviewBytes = 8 * 1024

// DefaultMaxResults is the default cap on search results.
const DefaultMaxResults = 200

// Result represents a single search match.
type Result struct {
	Path string
	Line int
	Text string
}

// Options controls search behavior.
type Options struct {
	CaseSensitive bool
	Regex         bool
	MaxResults    int
	FileGlob      string
}

// SearchFunc performs a text search under root for the given pattern.
type SearchFunc func(ctx context.Context, root, pattern string, opts Options) ([]Result, error)

// Tool lets the LLM search for text patterns across the project.
type Tool struct {
	projectRoot string
	search      SearchFunc
}

// New creates a search tool rooted at projectRoot using the given
// search backend.
func New(projectRoot string, fn SearchFunc) *Tool {
	if fn == nil {
		panic("search.New: nil SearchFunc")
	}
	return &Tool{projectRoot: projectRoot, search: fn}
}

// Definition returns the tool schema for the LLM.
func (t *Tool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        "search_project",
			Description: "Text or regex search across project files. Returns matching lines with paths + line numbers. Uses ripgrep when available.",
			Parameters: llm.FunctionParams{
				Type: "object",
				Properties: map[string]llm.FunctionParam{
					"pattern": {
						Type:        "string",
						Description: "The text or regex pattern to search for.",
					},
					"regex": {
						Type:        "boolean",
						Description: "Treat pattern as a regular expression. Default: false (literal match).",
					},
					"case_sensitive": {
						Type:        "boolean",
						Description: "Case-sensitive matching. Default: false.",
					},
					"file_glob": {
						Type:        "string",
						Description: "Filter files by glob pattern (e.g. \"*.go\", \"*.ts\"). Default: all files.",
					},
				},
				Required: []string{"pattern"},
			},
		},
	}
}

type searchArgs struct {
	Pattern       string `json:"pattern"`
	Regex         bool   `json:"regex"`
	CaseSensitive bool   `json:"case_sensitive"`
	FileGlob      string `json:"file_glob"`
}

// Execute runs the search and returns formatted results.
func (t *Tool) Execute(ctx context.Context, call llm.ToolCall) agent.ToolResult {
	var args searchArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("Error: invalid arguments: %v", err), IsError: true}
	}
	if args.Pattern == "" {
		return agent.ToolResult{Content: "Error: pattern is required", IsError: true}
	}

	results, err := t.search(ctx, t.projectRoot, args.Pattern, Options{
		CaseSensitive: args.CaseSensitive,
		Regex:         args.Regex,
		MaxResults:    DefaultMaxResults,
		FileGlob:      args.FileGlob,
	})
	if err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("Error: %v", err), IsError: true}
	}
	if len(results) == 0 {
		return agent.ToolResult{Content: "No matches found."}
	}

	const truncSuffix = "\n\n[... truncated — use read_file for full content]"
	limit := maxPreviewBytes - len(truncSuffix)

	var sb strings.Builder
	fmt.Fprintf(&sb, "%d match(es) found:\n\n", len(results))
	for _, r := range results {
		line := fmt.Sprintf("%s:%d: %s\n", r.Path, r.Line, r.Text)
		if sb.Len()+len(line) > limit {
			sb.WriteString(truncSuffix)
			return agent.ToolResult{Content: sb.String()}
		}
		sb.WriteString(line)
	}

	return agent.ToolResult{Content: sb.String()}
}
