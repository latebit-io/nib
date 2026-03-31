package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/latebit-io/junto/engine/llm"
	"github.com/latebit-io/junto/engine/search"
)

// SearchProjectTool lets the LLM search for text patterns across the project.
type SearchProjectTool struct {
	projectRoot string
}

// NewSearchProjectTool creates a SearchProjectTool.
func NewSearchProjectTool(projectRoot string) *SearchProjectTool {
	return &SearchProjectTool{projectRoot: projectRoot}
}

// Definition returns the tool schema for the LLM.
func (t *SearchProjectTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        "search_project",
			Description: "Search for a text pattern across all project files. Returns matching lines with file paths and line numbers. Use this to find function definitions, string occurrences, imports, TODOs, or any text pattern across the codebase. Prefers ripgrep when available.",
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
func (t *SearchProjectTool) Execute(_ context.Context, call llm.ToolCall) string {
	var args searchArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("Error: invalid arguments: %v", err)
	}
	if args.Pattern == "" {
		return "Error: pattern is required"
	}

	results, err := search.Search(t.projectRoot, args.Pattern, search.Options{
		CaseSensitive: args.CaseSensitive,
		Regex:         args.Regex,
		MaxResults:    search.DefaultMaxResults,
		FileGlob:      args.FileGlob,
	})
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	if len(results) == 0 {
		return "No matches found."
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%d match(es) found:\n\n", len(results))
	for _, r := range results {
		fmt.Fprintf(&sb, "%s:%d: %s\n", r.Path, r.Line, r.Text)
	}

	// Cap output to avoid token blow-up.
	out := sb.String()
	if len(out) > maxContentPreview {
		out = out[:maxContentPreview] + "\n... (truncated)"
	}
	return out
}
