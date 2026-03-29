package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/llm"
)

type testWorkspace struct {
	files           map[string]string
	inContext       map[string]bool
	addContextCalls int
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
	w.addContextCalls++
	w.inContext[w.CanonPath(path)] = true
}

func (w *testWorkspace) ProjectRoot() string { return "" }

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

func TestEditFileToolAutoAddsToContextOnApproval(t *testing.T) {
	ws := &testWorkspace{
		files:     map[string]string{"src/main.go": "package main"},
		inContext: map[string]bool{},
	}
	cache := NewFileCache()
	approveCh := make(chan bool, 1)
	continueCh := make(chan string, 1)
	send := func(_ event.Event) {}

	tool := NewEditFileTool(ws, cache, approveCh, continueCh, send, nil)

	args := mustMarshal(t, editArgs{
		Path:    "src/main.go",
		Search:  "package main",
		Replace: "package foo",
		Reason:  "rename",
	})
	call := llm.ToolCall{
		ID:       "1",
		Function: llm.FunctionCall{Name: "edit_file", Arguments: string(args)},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan string, 1)
	go func() {
		done <- tool.Execute(ctx, call)
	}()

	// Approve, then send continue with new content
	approveCh <- true
	continueCh <- "package foo"
	<-done

	if !ws.InContext("src/main.go") {
		t.Error("edit_file should auto-add file to context after approval")
	}
}

func TestEditFileToolNoContextAddOnRejection(t *testing.T) {
	ws := &testWorkspace{
		files:     map[string]string{"src/main.go": "package main"},
		inContext: map[string]bool{},
	}
	cache := NewFileCache()
	approveCh := make(chan bool, 1)
	continueCh := make(chan string, 1)
	send := func(_ event.Event) {}

	tool := NewEditFileTool(ws, cache, approveCh, continueCh, send, nil)

	args := mustMarshal(t, editArgs{
		Path:    "src/main.go",
		Search:  "package main",
		Replace: "package foo",
		Reason:  "rename",
	})
	call := llm.ToolCall{
		ID:       "1",
		Function: llm.FunctionCall{Name: "edit_file", Arguments: string(args)},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan string, 1)
	go func() {
		done <- tool.Execute(ctx, call)
	}()

	approveCh <- false
	<-done

	if ws.InContext("src/main.go") {
		t.Error("edit_file should not add to context on rejection")
	}
}

func TestEditFileToolSkipsAddWhenAlreadyInContext(t *testing.T) {
	ws := &testWorkspace{
		files:     map[string]string{"src/main.go": "package main"},
		inContext: map[string]bool{"src/main.go": true},
	}
	cache := NewFileCache()
	approveCh := make(chan bool, 1)
	continueCh := make(chan string, 1)
	send := func(_ event.Event) {}

	tool := NewEditFileTool(ws, cache, approveCh, continueCh, send, nil)

	args := mustMarshal(t, editArgs{
		Path:    "src/main.go",
		Search:  "package main",
		Replace: "package foo",
		Reason:  "rename",
	})
	call := llm.ToolCall{
		ID:       "1",
		Function: llm.FunctionCall{Name: "edit_file", Arguments: string(args)},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan string, 1)
	go func() {
		done <- tool.Execute(ctx, call)
	}()

	approveCh <- false
	<-done

	if ws.addContextCalls != 0 {
		t.Errorf("AddContext called %d times, want 0 (file already in context)", ws.addContextCalls)
	}
}
