package tool

import (
	"context"
	"sync"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit"
)

// stubTool is a minimal [kit.Tool] for tests. Records call count and
// returns a fixed result.
type stubTool struct {
	name   string
	result kit.ToolResult

	mu    sync.Mutex
	calls int
}

func (s *stubTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        s.name,
			Description: "stub",
		},
	}
}

func (s *stubTool) Execute(_ context.Context, _ llm.ToolCall) kit.ToolResult {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return s.result
}

func TestWithTracing_EmitsBeforeAndAfter(t *testing.T) {
	inner := &stubTool{
		name:   "list_files",
		result: kit.ToolResult{Content: "a\nb\nc"},
	}
	var events []TraceEvent
	tool := kit.DecorateTool(inner, WithTracing(func(ev TraceEvent) {
		events = append(events, ev)
	}))

	call := llm.ToolCall{
		ID:       "1",
		Type:     "function",
		Function: llm.FunctionCall{Name: "list_files", Arguments: `{"path":"."}`},
	}
	got := tool.Execute(context.Background(), call)
	if got.Content != "a\nb\nc" {
		t.Fatalf("decorator altered result: got %q", got.Content)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 trace events, got %d", len(events))
	}
	if events[0].Phase != PhaseBefore {
		t.Fatalf("event 0 phase: got %q", events[0].Phase)
	}
	if events[0].Tool != "list_files" {
		t.Fatalf("event 0 tool name: got %q", events[0].Tool)
	}
	if events[0].Call.ID != "1" {
		t.Fatalf("event 0 call ID lost: %+v", events[0].Call)
	}
	if events[1].Phase != PhaseAfter {
		t.Fatalf("event 1 phase: got %q", events[1].Phase)
	}
	if events[1].Result.Content != "a\nb\nc" {
		t.Fatalf("event 1 result not captured: %+v", events[1].Result)
	}
}

func TestWithTracing_DefinitionPassesThrough(t *testing.T) {
	inner := &stubTool{name: "x"}
	tool := kit.DecorateTool(inner, WithTracing(func(TraceEvent) {}))
	def := tool.Definition()
	if def.Function.Name != "x" {
		t.Fatalf("Definition() altered: %+v", def)
	}
}

func TestWithTracing_NilEmitIsNoop(t *testing.T) {
	inner := &stubTool{name: "x", result: kit.ToolResult{Content: "ok"}}
	tool := kit.DecorateTool(inner, WithTracing(nil))
	got := tool.Execute(context.Background(), llm.ToolCall{})
	if got.Content != "ok" {
		t.Fatalf("nil-emit decorator altered result: %q", got.Content)
	}
}
