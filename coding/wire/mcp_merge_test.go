package wire

import (
	"slices"
	"testing"
)

func TestMCPResult_Merge(t *testing.T) {
	var calls []string
	a := MCPResult{ServerNames: []string{"a"}, Cleanup: func() { calls = append(calls, "a") }}
	b := MCPResult{ServerNames: []string{"b"}, Cleanup: func() { calls = append(calls, "b") }}

	m := a.Merge(b)
	if !slices.Equal(m.ServerNames, []string{"a", "b"}) {
		t.Errorf("ServerNames = %v, want [a b]", m.ServerNames)
	}
	m.Cleanup()
	if !slices.Equal(calls, []string{"a", "b"}) {
		t.Errorf("cleanup order = %v, want [a b]", calls)
	}
	// Operands untouched.
	if len(a.ServerNames) != 1 || len(b.ServerNames) != 1 {
		t.Errorf("Merge mutated an operand: a=%v b=%v", a.ServerNames, b.ServerNames)
	}
}

func TestMCPResult_Merge_ZeroValueCleanup(t *testing.T) {
	m := MCPResult{}.Merge(MCPResult{})
	m.Cleanup() // must not panic on nil cleanups
}
