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

func execGoToLine(t *testing.T, ws Workspace, args string) (string, []event.Event) {
	t.Helper()
	var sentEvents []event.Event
	send := func(ev event.Event) { sentEvents = append(sentEvents, ev) }
	tool := NewGoToLineTool(ws, send)
	call := llm.ToolCall{
		ID:   "test",
		Type: "function",
		Function: llm.FunctionCall{
			Name:      "go_to_line",
			Arguments: args,
		},
	}
	return tool.Execute(context.Background(), call), sentEvents
}

func TestGoToLine_ValidNavigation(t *testing.T) {
	ws := &mockNavWorkspace{files: map[string]string{"main.go": "code"}}
	result, events := execGoToLine(t, ws, `{"path":"main.go","line":42}`)
	if result != "Navigated to main.go line 42." {
		t.Errorf("got %q", result)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	nav, ok := events[0].(event.AgentNavigate)
	if !ok {
		t.Fatalf("expected AgentNavigate, got %T", events[0])
	}
	if nav.Path != "main.go" || nav.Line != 42 {
		t.Errorf("event: got (%q, %d), want (main.go, 42)", nav.Path, nav.Line)
	}
}

func TestGoToLine_MissingPath(t *testing.T) {
	ws := &mockNavWorkspace{}
	result, _ := execGoToLine(t, ws, `{"line":10}`)
	if !strings.Contains(result, "path is required") {
		t.Errorf("got %q", result)
	}
}

func TestGoToLine_LineBelowOne(t *testing.T) {
	ws := &mockNavWorkspace{}
	result, _ := execGoToLine(t, ws, `{"path":"main.go","line":0}`)
	if !strings.Contains(result, "line must be >= 1") {
		t.Errorf("got %q", result)
	}
}

func TestGoToLine_InvalidJSON(t *testing.T) {
	ws := &mockNavWorkspace{}
	result, _ := execGoToLine(t, ws, `not json`)
	if !strings.HasPrefix(result, "Error: invalid arguments:") {
		t.Errorf("got %q", result)
	}
}

func TestGoToLine_FileNotFound(t *testing.T) {
	ws := &mockNavWorkspace{files: map[string]string{}}
	result, events := execGoToLine(t, ws, `{"path":"missing.go","line":1}`)
	if !strings.HasPrefix(result, "Error:") {
		t.Errorf("got %q, want error", result)
	}
	if len(events) > 0 {
		t.Error("expected no events on error")
	}
}
