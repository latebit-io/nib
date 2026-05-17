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
	"github.com/latebit-io/nib/kit/tools/truncate"
)

// maxPreviewBytes caps the result text returned to the LLM. Aligned
// with [truncate.DefaultMaxBytes] (50 KiB) so the search tool follows
// the same per-tool-result contract the agent-token-efficiency plan
// locks in for every tool.
const maxPreviewBytes = truncate.DefaultMaxBytes

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
	stash       truncate.Sink
}

// New creates a search tool rooted at projectRoot using the given
// search backend. The optional [truncate.Sink] is attached via
// [Tool.SetStash]; a nil sink (the default) keeps the truncation
// marker free of a "Full output:" path.
func New(projectRoot string, fn SearchFunc) *Tool {
	if fn == nil {
		panic("search.New: nil SearchFunc")
	}
	return &Tool{projectRoot: projectRoot, search: fn}
}

// SetStash attaches an optional [truncate.Sink] so over-cap results
// can be written somewhere the LLM can re-read in full. Calling with
// nil clears any previously-attached sink. Intended for the
// composition root; not part of the call-time contract.
func (t *Tool) SetStash(sink truncate.Sink) {
	t.stash = sink
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

	var sb strings.Builder
	fmt.Fprintf(&sb, "%d match(es) found:\n\n", len(results))
	for _, r := range results {
		fmt.Fprintf(&sb, "%s:%d: %s\n", r.Path, r.Line, r.Text)
	}

	out, _ := truncate.Bytes("search", sb.String(), maxPreviewBytes, t.stash)
	return agent.ToolResult{Content: out}
}
