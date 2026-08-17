package tool

import (
	"context"
	"sync"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/contracttest"
	"github.com/latebit-io/nib/kit/dyncontext"
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

// bindableTool is a stubTool that also implements kit.ShellBinder,
// recording the runner it was bound with.
type bindableTool struct {
	stubTool
	bound dyncontext.Runner
}

func (b *bindableTool) BindShell(r dyncontext.Runner) kit.Tool {
	c := &bindableTool{stubTool: stubTool{name: b.name, result: b.result}, bound: r}
	return c
}

type nopRunner struct{}

func (nopRunner) Run(context.Context, string) (string, error) { return "", nil }

// TestWithTracing_ForwardsBindShell: the tracing wrapper must not strip
// the wrapped tool's kit.ShellBinder — otherwise an agent's rebinding
// silently fails and directives fall back to the raw shell runner.
func TestWithTracing_ForwardsBindShell(t *testing.T) {
	inner := &bindableTool{stubTool: stubTool{name: "skill_x"}}
	traced := WithTracing(func(TraceEvent) {})(inner)

	rebound := kit.BindToolShell(traced, nopRunner{})
	tt, ok := rebound.(*tracedTool)
	if !ok {
		t.Fatalf("rebound tool is %T, want *tracedTool (tracing must survive rebinding)", rebound)
	}
	bt, ok := tt.inner.(*bindableTool)
	if !ok || bt.bound == nil {
		t.Fatalf("inner tool not rebound: %T bound=%v", tt.inner, ok && bt.bound != nil)
	}
	if inner.bound != nil {
		t.Fatal("BindShell must not mutate the original wrapped tool")
	}
}

// TestWithTracing_ForwardsOptionalInterfaces runs the shared wrapper
// contract so a new optional kit tool interface fails here if tracing
// forgets to forward it.
func TestWithTracing_ForwardsOptionalInterfaces(t *testing.T) {
	contracttest.ToolWrapper(t, WithTracing(func(TraceEvent) {}))
}
