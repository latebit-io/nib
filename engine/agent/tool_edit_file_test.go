package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/latebit-io/junto/engine/llm"
)

type testWorkspace struct {
	files     map[string]string
	inContext map[string]bool
}

func (w *testWorkspace) ReadFile(path string) (string, error) {
	if c, ok := w.files[path]; ok {
		return c, nil
	}
	return "", nil
}

func (w *testWorkspace) ListFiles() ([]string, error) { return nil, nil }
func (w *testWorkspace) WriteFile(_, _ string) error  { return nil }
func (w *testWorkspace) CanonPath(p string) string    { return p }

func (w *testWorkspace) InContext(path string) bool {
	return w.inContext[w.CanonPath(path)]
}

func (w *testWorkspace) AddContext(path string) {
	w.inContext[w.CanonPath(path)] = true
}

func TestEditFileToolAutoAddsToContext(t *testing.T) {
	ws := &testWorkspace{
		files:     map[string]string{"src/main.go": "package main"},
		inContext: map[string]bool{},
	}
	cache := NewFileCache()
	approveCh := make(chan bool, 1)
	continueCh := make(chan string, 1)
	send := func(_ Event) {}

	tool := NewEditFileTool(ws, cache, approveCh, continueCh, send)

	args, _ := json.Marshal(editArgs{
		Path:    "src/main.go",
		Search:  "package main",
		Replace: "package foo",
		Reason:  "rename",
	})
	call := llm.ToolCall{
		ID:       "1",
		Function: llm.FunctionCall{Name: "edit_file", Arguments: string(args)},
	}

	// Run in goroutine since Execute blocks on approval
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan string, 1)
	go func() {
		done <- tool.Execute(ctx, call)
	}()

	// Reject to unblock (we just care that it got past the context check)
	approveCh <- false
	<-done

	if !ws.InContext("src/main.go") {
		t.Error("edit_file should auto-add file to context")
	}
}

func TestEditFileToolAllowsInContext(t *testing.T) {
	ws := &testWorkspace{
		files:     map[string]string{"src/main.go": "package main"},
		inContext: map[string]bool{"src/main.go": true},
	}
	cache := NewFileCache()
	approveCh := make(chan bool, 1)
	continueCh := make(chan string, 1)

	var events []Event
	send := func(ev Event) { events = append(events, ev) }

	tool := NewEditFileTool(ws, cache, approveCh, continueCh, send)

	args, _ := json.Marshal(editArgs{
		Path:    "src/main.go",
		Search:  "package main",
		Replace: "package foo",
		Reason:  "rename",
	})
	call := llm.ToolCall{
		ID:       "1",
		Function: llm.FunctionCall{Name: "edit_file", Arguments: string(args)},
	}

	// Run in goroutine since Execute blocks on approval
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan string, 1)
	go func() {
		done <- tool.Execute(ctx, call)
	}()

	// Reject to unblock
	approveCh <- false

	result := <-done
	if strings.Contains(result, "not in the context set") {
		t.Errorf("should not get context rejection for in-context file, got: %s", result)
	}
}
