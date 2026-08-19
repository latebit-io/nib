package wire

import (
	"context"
	"slices"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/agent"
)

// namedTool is the minimal agent.Tool for asserting merge order.
type namedTool struct{ name string }

func (n namedTool) Definition() llm.ToolDef {
	return llm.ToolDef{Function: llm.FunctionDef{Name: n.name}}
}
func (namedTool) Execute(context.Context, llm.ToolCall) agent.ToolResult { return agent.ToolResult{} }

func toolNames(tools []agent.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Definition().Function.Name)
	}
	return names
}

func TestMCPResult_Merge(t *testing.T) {
	var calls []string
	a := MCPResult{
		Tools:       []agent.Tool{namedTool{"a1"}, namedTool{"a2"}},
		ServerNames: []string{"a"},
		Cleanup:     func() { calls = append(calls, "a") },
	}
	b := MCPResult{
		Tools:       []agent.Tool{namedTool{"b1"}},
		ServerNames: []string{"b"},
		Cleanup:     func() { calls = append(calls, "b") },
	}

	m := a.Merge(b)
	if got := toolNames(m.Tools); !slices.Equal(got, []string{"a1", "a2", "b1"}) {
		t.Errorf("Tools = %v, want [a1 a2 b1]", got)
	}
	if !slices.Equal(m.ServerNames, []string{"a", "b"}) {
		t.Errorf("ServerNames = %v, want [a b]", m.ServerNames)
	}
	m.Cleanup()
	if !slices.Equal(calls, []string{"a", "b"}) {
		t.Errorf("cleanup order = %v, want [a b]", calls)
	}
	// Operands untouched.
	if len(a.ServerNames) != 1 || len(b.ServerNames) != 1 || len(a.Tools) != 2 || len(b.Tools) != 1 {
		t.Errorf("Merge mutated an operand: a=%v b=%v", a.ServerNames, b.ServerNames)
	}
}

func TestMCPResult_Merge_ZeroValueCleanup(t *testing.T) {
	m := MCPResult{}.Merge(MCPResult{})
	m.Cleanup() // must not panic on nil cleanups
}
