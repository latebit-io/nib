package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/latebit-io/junto/engine/glob"
	"github.com/latebit-io/junto/engine/llm"
)

// maxGlobResults caps the number of files returned by the glob tool.
// Large result sets waste tokens — the agent should narrow the pattern.
const maxGlobResults = 100

// GlobTool finds files matching a glob pattern in the project tree.
// It walks the project (respecting .gitignore) and filters with the
// same glob engine used for gitignore matching (supports *, **, ?).
type GlobTool struct {
	workspace Workspace
}

// NewGlobTool creates a GlobTool backed by the given workspace.
func NewGlobTool(ws Workspace) *GlobTool {
	return &GlobTool{workspace: ws}
}

// globArgs holds the JSON-decoded arguments for the glob tool.
type globArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

// Definition returns the tool schema for the LLM.
func (t *GlobTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        "glob",
			Description: "Find files by glob pattern (respects .gitignore). Supports * (single segment), ** (any depth), and ? (single char). Use this to discover files by name or extension before reading them. Examples: **/*_test.go, engine/**/*.go, *.md",
			Parameters: llm.FunctionParams{
				Type: "object",
				Properties: map[string]llm.FunctionParam{
					"pattern": {
						Type:        "string",
						Description: "Glob pattern to match file paths. If path is omitted, matched against project-relative paths. If path is set, matched against paths relative to that subdirectory. Use ** for recursive matching across directories.",
					},
					"path": {
						Type:        "string",
						Description: "Optional subdirectory scope (relative to project root). When set, pattern is evaluated within this directory. Omit to search the entire project.",
					},
				},
				Required: []string{"pattern"},
			},
		},
	}
}

// normalizeScopePath cleans a user-supplied subdirectory path into a prefix
// suitable for strings.HasPrefix filtering. Handles leading slashes, dot
// prefixes, and doubled slashes (common LLM output artifacts).
// Returns "" when the path is empty or resolves to the project root.
func normalizeScopePath(p string) string {
	if p == "" {
		return ""
	}
	clean := strings.TrimPrefix(path.Clean(p), "/")
	if clean == "" || clean == "." {
		return ""
	}
	return clean + "/"
}

// Execute walks the project tree and returns files matching the glob pattern.
func (t *GlobTool) Execute(_ context.Context, call llm.ToolCall) ToolResult {
	var args globArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return textResult(fmt.Sprintf("Error: invalid arguments: %v", err))
	}

	if args.Pattern == "" {
		return textResult("Error: pattern is required")
	}

	files, err := t.workspace.ListFiles()
	if err != nil {
		return textResult(fmt.Sprintf("Error listing files: %v", err))
	}

	prefix := normalizeScopePath(args.Path)

	var matched []string
	total := 0
	for _, f := range files {
		// If a subdirectory is specified, only consider files under it.
		if prefix != "" && !strings.HasPrefix(f, prefix) {
			continue
		}

		// Match against the path relative to the scoped directory so that
		// basename patterns like *.go work when path is set.
		matchTarget := f
		if prefix != "" {
			matchTarget = f[len(prefix):]
		}

		if glob.Match(args.Pattern, matchTarget) {
			total++
			if len(matched) < maxGlobResults {
				matched = append(matched, f)
			}
		}
	}

	if total == 0 {
		return textResult("No files found matching pattern: " + args.Pattern)
	}

	var sb strings.Builder
	for _, f := range matched {
		sb.WriteString(f)
		sb.WriteByte('\n')
	}
	if total > maxGlobResults {
		fmt.Fprintf(&sb, "\n(%d files matched, showing first %d. Use a more specific pattern or path to narrow results.)", total, maxGlobResults)
	} else {
		fmt.Fprintf(&sb, "\n%d file(s) found.", total)
	}

	return textResult(sb.String())
}
