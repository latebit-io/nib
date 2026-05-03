package tools

import (
	"strings"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
)

// assertContains is a shared test helper used across the tools tests
// to verify that a tool result body contains an expected substring.
func assertContains(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("got %q, want substring %q", got, want)
	}
}

// toolCall builds an llm.ToolCall with the given ID, name and JSON
// arguments. Convenience for table-driven tool tests.
func toolCall(id, name, args string) llm.ToolCall {
	return llm.ToolCall{
		ID: id,
		Function: llm.FunctionCall{
			Name:      name,
			Arguments: args,
		},
	}
}
