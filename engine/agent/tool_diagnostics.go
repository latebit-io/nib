package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/latebit-io/junto/engine/lang"
	"github.com/latebit-io/junto/engine/llm"
)

// DiagnosticsTool exposes language service diagnostics to the agent.
// Depends on lang.DiagnosticProvider (port interface), not any concrete LSP type.
type DiagnosticsTool struct {
	provider  lang.DiagnosticProvider
	workspace Workspace
}

// NewDiagnosticsTool creates a diagnostics tool. Only register if the language
// service supports diagnostics (checked via type assertion in main.go).
func NewDiagnosticsTool(provider lang.DiagnosticProvider, workspace Workspace) *DiagnosticsTool {
	return &DiagnosticsTool{provider: provider, workspace: workspace}
}

// Definition returns the tool schema for the LLM.
func (t *DiagnosticsTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        "diagnostics",
			Description: "Get compiler errors, warnings, and hints for a file. Use after editing to check for problems.",
			Parameters: llm.FunctionParams{
				Type: "object",
				Properties: map[string]llm.FunctionParam{
					"path": {
						Type:        "string",
						Description: "File path relative to the project root. Leave empty to check the active file.",
					},
				},
			},
		},
	}
}

// Execute queries diagnostics for the specified file.
func (t *DiagnosticsTool) Execute(_ context.Context, call llm.ToolCall) string {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		slog.Debug("diagnostics: bad arguments", "err", err)
		return "Error: invalid arguments"
	}

	path := args.Path
	if path != "" {
		path = t.workspace.CanonPath(path)
	}

	return FormatDiagnostics(t.provider, path)
}

// FormatDiagnostics queries and formats diagnostics for a file path.
// Exported so tool_edit_file can reuse it for auto-injection.
// If path is empty, returns diagnostics for all known files (not implemented — returns empty).
func FormatDiagnostics(provider lang.DiagnosticProvider, path string) string {
	if path == "" {
		return "No file specified."
	}

	diags := provider.Diagnostics(path)
	if len(diags) == 0 {
		return "No diagnostics — code is clean."
	}

	var sb strings.Builder
	errors, warnings := 0, 0
	for _, d := range diags {
		var severity string
		switch d.Severity {
		case lang.SeverityError:
			severity = "error"
			errors++
		case lang.SeverityWarning:
			severity = "warning"
			warnings++
		default:
			severity = "info"
		}
		sb.WriteString(fmt.Sprintf("  %s:%d:%d: %s: %s\n",
			path, d.StartLine+1, d.StartCol+1, severity, d.Message))
	}

	summary := fmt.Sprintf("%d error(s), %d warning(s)\n", errors, warnings)
	return summary + sb.String()
}
