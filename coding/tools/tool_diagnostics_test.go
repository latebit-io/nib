package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/engine/lang"
)

// mockDiagProvider returns canned diagnostics.
type mockDiagProvider struct {
	diags map[string][]lang.Diagnostic
}

func (m *mockDiagProvider) Diagnostics(path string) []lang.Diagnostic {
	return m.diags[path]
}

func TestFormatDiagnostics(t *testing.T) {
	t.Run("not analyzed yet", func(t *testing.T) {
		p := &mockDiagProvider{diags: map[string][]lang.Diagnostic{}}
		result := FormatDiagnostics(p, "/test.go", "test.go")
		if !strings.Contains(result, "not have been analyzed") {
			t.Errorf("expected not-analyzed message, got: %s", result)
		}
	})

	t.Run("clean file", func(t *testing.T) {
		p := &mockDiagProvider{diags: map[string][]lang.Diagnostic{
			"/test.go": {}, // explicitly empty = gopls checked and found nothing
		}}
		result := FormatDiagnostics(p, "/test.go", "test.go")
		if !strings.Contains(result, "clean") {
			t.Errorf("expected clean message, got: %s", result)
		}
	})

	t.Run("with errors and warnings", func(t *testing.T) {
		p := &mockDiagProvider{diags: map[string][]lang.Diagnostic{
			"/test.go": {
				{StartLine: 5, StartCol: 10, Severity: lang.SeverityError, Message: "undefined: foo"},
				{StartLine: 8, StartCol: 0, Severity: lang.SeverityWarning, Message: "unused var"},
			},
		}}
		result := FormatDiagnostics(p, "/test.go", "test.go")
		if !strings.Contains(result, "1 error(s)") {
			t.Errorf("expected error count, got: %s", result)
		}
		if !strings.Contains(result, "1 warning(s)") {
			t.Errorf("expected warning count, got: %s", result)
		}
		if !strings.Contains(result, "undefined: foo") {
			t.Errorf("expected error message, got: %s", result)
		}
		if !strings.Contains(result, "unused var") {
			t.Errorf("expected warning message, got: %s", result)
		}
	})

	t.Run("empty path", func(t *testing.T) {
		p := &mockDiagProvider{}
		result := FormatDiagnostics(p, "", "")
		if !strings.Contains(result, "No file") {
			t.Errorf("expected no-file message, got: %s", result)
		}
	})
}

func TestDiagnosticsToolExecute(t *testing.T) {
	p := &mockDiagProvider{diags: map[string][]lang.Diagnostic{
		"/project/main.go": {
			{StartLine: 2, StartCol: 0, Severity: lang.SeverityError, Message: "syntax error"},
		},
	}}
	ws := &stubWorkspace{}
	tool := NewDiagnosticsTool(p, ws)

	t.Run("with path argument", func(t *testing.T) {
		call := llm.ToolCall{
			Function: llm.FunctionCall{
				Arguments: `{"path": "/project/main.go"}`,
			},
		}
		result := tool.Execute(context.Background(), call)
		if !strings.Contains(result.Content, "syntax error") {
			t.Errorf("expected diagnostic, got: %s", result.Content)
		}
	})

	t.Run("invalid arguments", func(t *testing.T) {
		call := llm.ToolCall{
			Function: llm.FunctionCall{
				Arguments: `{invalid`,
			},
		}
		result := tool.Execute(context.Background(), call)
		if !strings.Contains(result.Content, "Error") {
			t.Errorf("expected error, got: %s", result.Content)
		}
	})
}

// stubWorkspace for diagnostics tests.
type stubWorkspace struct{}

func (stubWorkspace) ReadFile(_ string) (string, error) { return "", nil }
func (stubWorkspace) ListFiles() ([]string, error)      { return nil, nil }
func (stubWorkspace) WriteFile(_, _ string) error       { return nil }
func (stubWorkspace) CanonPath(p string) string         { return p }
func (stubWorkspace) ProjectRoot() string               { return "" }
