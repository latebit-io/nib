package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/latebit-io/junto/ai/llm"
	"github.com/latebit-io/junto/engine/lang"
)

// defWorkspace returns a testWorkspace configured with a project root.
func defWorkspace(root string) *testWorkspace {
	return &testWorkspace{
		root:      root,
		files:     make(map[string]string),
		inContext: make(map[string]bool),
	}
}

// mockDefinitionProvider is a stub that returns a fixed location or error.
type mockDefinitionProvider struct {
	loc lang.Location
	err error
}

func (m *mockDefinitionProvider) Definition(_ context.Context, _ string, _, _ int) (lang.Location, error) {
	return m.loc, m.err
}

func makeDefCall(t *testing.T, args posArgs) llm.ToolCall {
	t.Helper()
	return llm.ToolCall{
		ID:       "1",
		Function: llm.FunctionCall{Name: "go_to_definition", Arguments: string(mustMarshal(t, args))},
	}
}

func TestGoToDefinitionTool_Success(t *testing.T) {
	ws := &testWorkspace{
		files:     map[string]string{},
		inContext: map[string]bool{},
	}

	provider := &mockDefinitionProvider{
		loc: lang.Location{Path: "/project/engine/agent/tool.go", Line: 41, Col: 5},
	}

	tool := NewGoToDefinitionTool(ws, provider)
	result := tool.Execute(context.Background(), makeDefCall(t, posArgs{
		Path: "engine/agent/agent.go",
		Line: 10,
		Col:  5,
	}))

	// Location is 0-indexed from provider, output should be 1-indexed line.
	if !strings.Contains(result.Content, "engine/agent/tool.go:42:5") {
		t.Errorf("expected definition location in output, got:\n%s", result.Content)
	}
	if !strings.HasPrefix(result.Content, "Definition: ") {
		t.Errorf("expected 'Definition: ' prefix, got:\n%s", result.Content)
	}
}

func TestGoToDefinitionTool_NotFound(t *testing.T) {
	ws := &testWorkspace{
		files:     map[string]string{},
		inContext: map[string]bool{},
	}

	provider := &mockDefinitionProvider{
		loc: lang.Location{}, // empty path = not found
	}

	tool := NewGoToDefinitionTool(ws, provider)
	result := tool.Execute(context.Background(), makeDefCall(t, posArgs{
		Path: "main.go",
		Line: 1,
		Col:  0,
	}))

	if !strings.Contains(result.Content, "No definition found") {
		t.Errorf("expected 'No definition found', got:\n%s", result.Content)
	}
}

func TestGoToDefinitionTool_ProviderError(t *testing.T) {
	ws := &testWorkspace{
		files:     map[string]string{},
		inContext: map[string]bool{},
	}

	provider := &mockDefinitionProvider{
		err: fmt.Errorf("server not ready"),
	}

	tool := NewGoToDefinitionTool(ws, provider)
	result := tool.Execute(context.Background(), makeDefCall(t, posArgs{
		Path: "main.go",
		Line: 1,
		Col:  0,
	}))

	if !strings.Contains(result.Content, "server not ready") {
		t.Errorf("expected error propagated, got:\n%s", result.Content)
	}
}

func TestGoToDefinitionTool_MissingPath(t *testing.T) {
	ws := &testWorkspace{
		files:     map[string]string{},
		inContext: map[string]bool{},
	}

	provider := &mockDefinitionProvider{}
	tool := NewGoToDefinitionTool(ws, provider)
	result := tool.Execute(context.Background(), makeDefCall(t, posArgs{
		Path: "",
		Line: 1,
		Col:  0,
	}))

	if !strings.Contains(result.Content, "path is required") {
		t.Errorf("expected path validation error, got:\n%s", result.Content)
	}
}

func TestGoToDefinitionTool_InvalidPosition(t *testing.T) {
	ws := &testWorkspace{
		files:     map[string]string{},
		inContext: map[string]bool{},
	}
	provider := &mockDefinitionProvider{}
	tool := NewGoToDefinitionTool(ws, provider)

	result := tool.Execute(context.Background(), makeDefCall(t, posArgs{
		Path: "main.go",
		Line: 0,
		Col:  0,
	}))
	if !strings.Contains(result.Content, "line must be >= 1") {
		t.Errorf("expected line validation error, got:\n%s", result.Content)
	}

	result = tool.Execute(context.Background(), makeDefCall(t, posArgs{
		Path: "main.go",
		Line: 1,
		Col:  -1,
	}))
	if !strings.Contains(result.Content, "col must be >= 0") {
		t.Errorf("expected col validation error, got:\n%s", result.Content)
	}
}

func TestGoToDefinitionTool_InvalidJSON(t *testing.T) {
	ws := &testWorkspace{
		files:     map[string]string{},
		inContext: map[string]bool{},
	}

	provider := &mockDefinitionProvider{}
	tool := NewGoToDefinitionTool(ws, provider)
	call := llm.ToolCall{
		ID:       "1",
		Function: llm.FunctionCall{Name: "go_to_definition", Arguments: "not json"},
	}
	result := tool.Execute(context.Background(), call)

	if !strings.Contains(result.Content, "invalid arguments") {
		t.Errorf("expected invalid arguments error, got:\n%s", result.Content)
	}
}

func TestGoToDefinitionTool_PathTraversal(t *testing.T) {
	ws := defWorkspace("/project")
	provider := &mockDefinitionProvider{}
	tool := NewGoToDefinitionTool(ws, provider)

	result := tool.Execute(context.Background(), makeDefCall(t, posArgs{
		Path: "../../etc/passwd",
		Line: 1,
		Col:  0,
	}))

	if !strings.Contains(result.Content, "outside the project root") {
		t.Errorf("expected traversal rejection, got:\n%s", result.Content)
	}
}

func TestGoToDefinitionTool_SiblingDirTraversal(t *testing.T) {
	ws := defWorkspace("/project")
	provider := &mockDefinitionProvider{}
	tool := NewGoToDefinitionTool(ws, provider)

	// "/project-secrets/file.txt" shares the prefix "/project" but is
	// a sibling directory — must be rejected.
	result := tool.Execute(context.Background(), makeDefCall(t, posArgs{
		Path: "/project-secrets/file.txt",
		Line: 1,
		Col:  0,
	}))

	if !strings.Contains(result.Content, "outside the project root") {
		t.Errorf("sibling directory should be rejected, got:\n%s", result.Content)
	}
}

func TestGoToDefinitionTool_Definition(t *testing.T) {
	ws := &testWorkspace{
		files:     map[string]string{},
		inContext: map[string]bool{},
	}

	provider := &mockDefinitionProvider{}
	tool := NewGoToDefinitionTool(ws, provider)
	def := tool.Definition()

	if def.Function.Name != "go_to_definition" {
		t.Errorf("expected tool name 'go_to_definition', got %q", def.Function.Name)
	}
	for _, req := range []string{"path", "line", "col"} {
		if _, ok := def.Function.Parameters.Properties[req]; !ok {
			t.Errorf("expected %q in parameters", req)
		}
	}
	if len(def.Function.Parameters.Required) != 3 {
		t.Errorf("expected 3 required params, got %d", len(def.Function.Parameters.Required))
	}
}
