package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/event"
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

// fakeNavigator captures navigation events the tool publishes so tests
// can assert on them without standing up an Agent.
type fakeNavigator struct {
	mu       sync.Mutex
	captured []event.AgentNavigate
}

func (f *fakeNavigator) Navigate(_ context.Context, nav event.AgentNavigate) {
	f.mu.Lock()
	f.captured = append(f.captured, nav)
	f.mu.Unlock()
}

func execGoToLine(t *testing.T, ws FileReader, nav *fakeNavigator, args string) ToolResult {
	t.Helper()
	tool := NewGoToLineTool(ws, nav)
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
	nav := &fakeNavigator{}
	result := execGoToLine(t, ws, nav, `{"path":"main.go","line":42}`)
	if result.Content != "Navigated to main.go line 42." {
		t.Errorf("got %q", result.Content)
	}
	if len(nav.captured) != 1 {
		t.Fatalf("expected 1 nav event, got %d", len(nav.captured))
	}
	got := nav.captured[0]
	if got.Path != "main.go" || got.Line != 42 {
		t.Errorf("nav event: got (%q, %d), want (main.go, 42)", got.Path, got.Line)
	}
}

func TestGoToLine_MissingPath(t *testing.T) {
	ws := &mockNavWorkspace{}
	nav := &fakeNavigator{}
	result := execGoToLine(t, ws, nav, `{"line":10}`)
	if !strings.Contains(result.Content, "path is required") {
		t.Errorf("got %q", result.Content)
	}
	if len(nav.captured) != 0 {
		t.Errorf("nav must not fire on validation failure, got %d events", len(nav.captured))
	}
}

func TestGoToLine_LineBelowOne(t *testing.T) {
	ws := &mockNavWorkspace{}
	nav := &fakeNavigator{}
	result := execGoToLine(t, ws, nav, `{"path":"main.go","line":0}`)
	if !strings.Contains(result.Content, "line must be >= 1") {
		t.Errorf("got %q", result.Content)
	}
	if len(nav.captured) != 0 {
		t.Errorf("nav must not fire on validation failure, got %d events", len(nav.captured))
	}
}

func TestGoToLine_InvalidJSON(t *testing.T) {
	ws := &mockNavWorkspace{}
	nav := &fakeNavigator{}
	result := execGoToLine(t, ws, nav, `not json`)
	if !strings.HasPrefix(result.Content, "Error: invalid arguments:") {
		t.Errorf("got %q", result.Content)
	}
	if len(nav.captured) != 0 {
		t.Errorf("nav must not fire on parse failure, got %d events", len(nav.captured))
	}
}

func TestGoToLine_FileNotFound(t *testing.T) {
	ws := &mockNavWorkspace{files: map[string]string{}}
	nav := &fakeNavigator{}
	result := execGoToLine(t, ws, nav, `{"path":"missing.go","line":1}`)
	if !strings.HasPrefix(result.Content, "Error:") {
		t.Errorf("got %q, want error", result.Content)
	}
	if len(nav.captured) != 0 {
		t.Errorf("nav must not fire when file missing, got %d events", len(nav.captured))
	}
}
