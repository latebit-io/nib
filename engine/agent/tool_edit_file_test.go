package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/llm"
)

type testWorkspace struct {
	root            string // project root; empty means CanonPath is identity
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
func (w *testWorkspace) CanonPath(p string) string {
	if w.root != "" && !filepath.IsAbs(p) {
		return filepath.Clean(filepath.Join(w.root, p))
	}
	return p
}

func (w *testWorkspace) InContext(path string) bool {
	return w.inContext[w.CanonPath(path)]
}

func (w *testWorkspace) AddContext(path string) {
	w.addContextCalls++
	w.inContext[w.CanonPath(path)] = true
}

func (w *testWorkspace) ProjectRoot() string { return w.root }

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

func TestEditFileTool_ReturnsEditProposal(t *testing.T) {
	ws := &testWorkspace{
		files:     map[string]string{"src/main.go": "package main"},
		inContext: map[string]bool{},
	}
	cache := NewFileCache()
	tool := NewEditFileTool(ws, cache)

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

	result := tool.Execute(context.Background(), call)

	if result.Effect != EffectEditProposed {
		t.Fatalf("expected EffectEditProposed, got %d", result.Effect)
	}

	proposal, ok := result.Payload.(EditProposal)
	if !ok {
		t.Fatalf("expected EditProposal payload, got %T", result.Payload)
	}

	if proposal.Edit.Path != "src/main.go" {
		t.Errorf("expected path src/main.go, got %q", proposal.Edit.Path)
	}
	if proposal.Edit.Search != "package main" {
		t.Errorf("expected search 'package main', got %q", proposal.Edit.Search)
	}
	if proposal.Edit.Replace != "package foo" {
		t.Errorf("expected replace 'package foo', got %q", proposal.Edit.Replace)
	}
	if proposal.ExpectedContent != "package foo" {
		t.Errorf("expected content 'package foo', got %q", proposal.ExpectedContent)
	}
}

func TestEditFileTool_RootedCanonPath(t *testing.T) {
	// Exercises the root-aware CanonPath branch: relative paths are resolved
	// against the workspace root, and the canonical key is used for cache
	// lookups and context tracking.
	ws := &testWorkspace{
		root: "/repo",
		// File keyed by original relative path — the closure in resolveContent
		// passes the original path to ReadFile, not the canonical key.
		files:     map[string]string{"src/main.go": "package main"},
		inContext: map[string]bool{},
	}
	cache := NewFileCache()
	tool := NewEditFileTool(ws, cache)

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

	result := tool.Execute(context.Background(), call)
	if result.Effect != EffectEditProposed {
		t.Fatalf("expected EffectEditProposed, got %d", result.Effect)
	}

	proposal := result.Payload.(EditProposal)
	if proposal.CanonPath != "/repo/src/main.go" {
		t.Errorf("expected canon path /repo/src/main.go, got %q", proposal.CanonPath)
	}

	// Cache should be populated under the canonical key.
	if _, ok := cache.Get("/repo/src/main.go"); !ok {
		t.Error("expected cache hit for canonical path /repo/src/main.go")
	}

}

func TestEditFileTool_ValidationFailsNoMatch(t *testing.T) {
	ws := &testWorkspace{
		files:     map[string]string{"src/main.go": "package main"},
		inContext: map[string]bool{},
	}
	cache := NewFileCache()
	tool := NewEditFileTool(ws, cache)

	args := mustMarshal(t, editArgs{
		Path:    "src/main.go",
		Search:  "nonexistent text",
		Replace: "replacement",
		Reason:  "test",
	})
	call := llm.ToolCall{
		ID:       "1",
		Function: llm.FunctionCall{Name: "edit_file", Arguments: string(args)},
	}

	result := tool.Execute(context.Background(), call)
	if result.Effect != EffectNone {
		t.Errorf("expected EffectNone on validation failure, got %d", result.Effect)
	}
	if !strings.Contains(result.Content, "Error:") {
		t.Errorf("expected error message, got %q", result.Content)
	}
}

func TestEditFileTool_ValidationFailsAmbiguous(t *testing.T) {
	ws := &testWorkspace{
		files:     map[string]string{"src/main.go": "foo\nfoo\nbar"},
		inContext: map[string]bool{},
	}
	cache := NewFileCache()
	tool := NewEditFileTool(ws, cache)

	args := mustMarshal(t, editArgs{
		Path:    "src/main.go",
		Search:  "foo",
		Replace: "baz",
		Reason:  "test",
	})
	call := llm.ToolCall{
		ID:       "1",
		Function: llm.FunctionCall{Name: "edit_file", Arguments: string(args)},
	}

	result := tool.Execute(context.Background(), call)
	if result.Effect != EffectNone {
		t.Errorf("expected EffectNone on ambiguous match, got %d", result.Effect)
	}
	if !strings.Contains(result.Content, "matches 2 locations") {
		t.Errorf("expected ambiguity error, got %q", result.Content)
	}
}

func TestEditFileTool_EmptySearchOnNonEmptyFile(t *testing.T) {
	ws := &testWorkspace{
		files:     map[string]string{"src/main.go": "package main"},
		inContext: map[string]bool{},
	}
	cache := NewFileCache()
	tool := NewEditFileTool(ws, cache)

	args := mustMarshal(t, editArgs{
		Path:    "src/main.go",
		Search:  "",
		Replace: "new content",
		Reason:  "test",
	})
	call := llm.ToolCall{
		ID:       "1",
		Function: llm.FunctionCall{Name: "edit_file", Arguments: string(args)},
	}

	result := tool.Execute(context.Background(), call)
	if !strings.Contains(result.Content, "search field cannot be empty") {
		t.Errorf("expected empty search error, got %q", result.Content)
	}
}

func TestEditFileTool_ProposalContainsCallID(t *testing.T) {
	ws := &testWorkspace{
		files:     map[string]string{"src/main.go": "package main"},
		inContext: map[string]bool{},
	}
	cache := NewFileCache()
	tool := NewEditFileTool(ws, cache)

	args := mustMarshal(t, editArgs{
		Path:    "src/main.go",
		Search:  "package main",
		Replace: "package foo",
		Reason:  "rename",
	})
	call := llm.ToolCall{
		ID:       "call-42",
		Function: llm.FunctionCall{Name: "edit_file", Arguments: string(args)},
	}

	result := tool.Execute(context.Background(), call)
	proposal := result.Payload.(EditProposal)
	if proposal.Edit.ID != "call-42" {
		t.Errorf("expected edit ID 'call-42', got %q", proposal.Edit.ID)
	}
}

func TestEditFileTool_ProposalPendingEditFields(t *testing.T) {
	ws := &testWorkspace{
		files:     map[string]string{"main.go": "hello world"},
		inContext: map[string]bool{},
	}
	cache := NewFileCache()
	tool := NewEditFileTool(ws, cache)

	args := mustMarshal(t, editArgs{
		Path:    "main.go",
		Search:  "hello",
		Replace: "goodbye",
		Reason:  "farewell",
	})
	call := llm.ToolCall{
		ID:       "1",
		Function: llm.FunctionCall{Name: "edit_file", Arguments: string(args)},
	}

	result := tool.Execute(context.Background(), call)
	proposal := result.Payload.(EditProposal)

	want := event.PendingEdit{
		ID:      "1",
		Path:    "main.go",
		Search:  "hello",
		Replace: "goodbye",
		Reason:  "farewell",
	}
	if proposal.Edit != want {
		t.Errorf("PendingEdit mismatch:\n got  %+v\n want %+v", proposal.Edit, want)
	}
}
