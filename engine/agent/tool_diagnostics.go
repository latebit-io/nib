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
	workspace FileReader
}

// NewDiagnosticsTool creates a diagnostics tool. Only register if the language
// service supports diagnostics (checked via type assertion in main.go).
func NewDiagnosticsTool(provider lang.DiagnosticProvider, workspace FileReader) *DiagnosticsTool {
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
						Description: "File path (absolute or relative to the project root).",
					},
				},
				Required: []string{"path"},
			},
		},
	}
}

// Execute queries diagnostics for the specified file.
func (t *DiagnosticsTool) Execute(_ context.Context, call llm.ToolCall) ToolResult {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		slog.Debug("diagnostics: bad arguments", "err", err)
		return textResult("Error: invalid arguments")
	}

	path := args.Path
	displayPath := path // keep the user-provided (relative) path for display
	if path != "" {
		path = t.workspace.CanonPath(path)
	}

	return textResult(formatDiagnostics(t.provider, path, displayPath))
}

// formatDiagnostics queries and formats diagnostics for a file path.
// lookupPath is the canonical path for the provider query.
// displayPath is the project-relative path shown in output (avoids leaking absolute paths).
func formatDiagnostics(provider lang.DiagnosticProvider, lookupPath, displayPath string) string {
	if lookupPath == "" {
		return "No file specified."
	}
	if displayPath == "" {
		displayPath = lookupPath
	}

	diags := provider.Diagnostics(lookupPath)
	if diags == nil {
		return "No diagnostics available yet — the file may not have been analyzed."
	}
	if len(diags) == 0 {
		return "No diagnostics — code is clean."
	}

	const maxDiagBytes = 8192 // cap output to avoid drowning the LLM context

	// Count totals first so the summary is accurate even if output is truncated.
	totalErrors, totalWarnings := 0, 0
	for _, d := range diags {
		switch d.Severity {
		case lang.SeverityError:
			totalErrors++
		case lang.SeverityWarning:
			totalWarnings++
		}
	}

	var sb strings.Builder
	truncated := false
	for _, d := range diags {
		var severity string
		switch d.Severity {
		case lang.SeverityError:
			severity = "error"
		case lang.SeverityWarning:
			severity = "warning"
		default:
			severity = "info"
		}
		line := fmt.Sprintf("  %s:%d:%d: %s: %s\n",
			displayPath, d.StartLine+1, d.StartCol+1, severity, d.Message)
		if sb.Len()+len(line) > maxDiagBytes {
			truncated = true
			break
		}
		sb.WriteString(line)
	}

	summary := fmt.Sprintf("%d error(s), %d warning(s)\n", totalErrors, totalWarnings)
	if truncated {
		sb.WriteString("  ... diagnostics truncated\n")
	}
	return summary + sb.String()
}

// hasDiagnosticErrors reports whether a file has any error-severity diagnostics.
func hasDiagnosticErrors(provider lang.DiagnosticProvider, path string) bool {
	diags := provider.Diagnostics(path)
	for _, d := range diags {
		if d.Severity == lang.SeverityError {
			return true
		}
	}
	return false
}
