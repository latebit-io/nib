package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/llm"
)

type mockNavWorkspace struct {
	files map[string]string
	err   error
}

func (m *mockNavWorkspace) ProjectRoot() string          { return "/test" }
func (m *mockNavWorkspace) ListFiles() ([]string, error) { return nil, nil }
func (m *mockNavWorkspace) WriteFile(_, _ string) error  { return nil }
func (m *mockNavWorkspace) CanonPath(p string) string    { return p }
func (m *mockNavWorkspace) InContext(_ string) bool      { return true }
func (m *mockNavWorkspace) AddContext(_ string)          {}

func (m *mockNavWorkspace) ReadFile(path string) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	if c, ok := m.files[path]; ok {
		return c, nil
	}
	return "", fmt.Errorf("file not found: %s", path)
}

func execGoToLine(t *testing.T, ws FileReader, args string) ToolResult {
	t.Helper()
	tool := NewGoToLineTool(ws)
	call := llm.ToolCall{
		ID:   "test",
		Type: "function",
		Function: llm.FunctionCall{
			Name:      "go_to_line",
			Arguments: args,
		},
	}
	return tool.Execute(context.Background(), call)
}

func TestGoToLine_ValidNavigation(t *testing.T) {
	ws := &mockNavWorkspace{files: map[string]string{"main.go": "code"}}
	result := execGoToLine(t, ws, `{"path":"main.go","line":42}`)
	if result.Content != "Navigated to main.go line 42." {
		t.Errorf("got %q", result.Content)
	}
	if result.Effect != EffectNavigate {
		t.Errorf("expected EffectNavigate, got %d", result.Effect)
	}
	nav, ok := result.Payload.(event.AgentNavigate)
	if !ok {
		t.Fatalf("expected AgentNavigate payload, got %T", result.Payload)
	}
	if nav.Path != "main.go" || nav.Line != 42 {
		t.Errorf("payload: got (%q, %d), want (main.go, 42)", nav.Path, nav.Line)
	}
}

func TestGoToLine_MissingPath(t *testing.T) {
	ws := &mockNavWorkspace{}
	result := execGoToLine(t, ws, `{"line":10}`)
	if !strings.Contains(result.Content, "path is required") {
		t.Errorf("got %q", result.Content)
	}
}

func TestGoToLine_LineBelowOne(t *testing.T) {
	ws := &mockNavWorkspace{}
	result := execGoToLine(t, ws, `{"path":"main.go","line":0}`)
	if !strings.Contains(result.Content, "line must be >= 1") {
		t.Errorf("got %q", result.Content)
	}
}

func TestGoToLine_InvalidJSON(t *testing.T) {
	ws := &mockNavWorkspace{}
	result := execGoToLine(t, ws, `not json`)
	if !strings.HasPrefix(result.Content, "Error: invalid arguments:") {
		t.Errorf("got %q", result.Content)
	}
}

func TestGoToLine_FileNotFound(t *testing.T) {
	ws := &mockNavWorkspace{files: map[string]string{}}
	result := execGoToLine(t, ws, `{"path":"missing.go","line":1}`)
	if !strings.HasPrefix(result.Content, "Error:") {
		t.Errorf("got %q, want error", result.Content)
	}
	if result.Effect != EffectNone {
		t.Errorf("expected EffectNone on error, got %d", result.Effect)
	}
}
