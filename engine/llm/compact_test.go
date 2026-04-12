package llm

import (
	"strings"
	"testing"
)

func TestCompactMessages_NoCompactionWhenFewTurns(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "you are helpful"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
	}
	got := CompactMessages(msgs, 2, 100)
	if len(got) != len(msgs) {
		t.Fatalf("expected %d messages, got %d", len(msgs), len(got))
	}
	// Should return original slice when nothing to compact.
	if &got[0] != &msgs[0] {
		t.Error("expected original slice returned when no compaction needed")
	}
}

func TestCompactMessages_TruncatesOldToolResults(t *testing.T) {
	largeContent := strings.Repeat("x", 500)
	msgs := []Message{
		{Role: "system", Content: "system"},
		// Turn 1 (old — should be compacted)
		{Role: "user", Content: "do something"},
		{Role: "assistant", Content: "ok", ToolCalls: []ToolCall{
			{ID: "tc1", Type: "function", Function: FunctionCall{Name: "read_file", Arguments: `{"path":"main.go"}`}},
		}},
		{Role: "tool", ToolCallID: "tc1", Content: largeContent},
		{Role: "assistant", Content: "done"},
		// Turn 2 (recent — kept)
		{Role: "user", Content: "next"},
		{Role: "assistant", Content: "working", ToolCalls: []ToolCall{
			{ID: "tc2", Type: "function", Function: FunctionCall{Name: "bash", Arguments: `{"cmd":"ls"}`}},
		}},
		{Role: "tool", ToolCallID: "tc2", Content: largeContent},
		{Role: "assistant", Content: "here"},
		// Turn 3 (recent — kept)
		{Role: "user", Content: "more"},
		{Role: "assistant", Content: "sure"},
	}

	got := CompactMessages(msgs, 2, 100)
	if len(got) != len(msgs) {
		t.Fatalf("expected %d messages, got %d", len(msgs), len(got))
	}

	// Old tool result (tc1) should be truncated.
	oldTool := got[3]
	if oldTool.ToolCallID != "tc1" {
		t.Errorf("expected ToolCallID tc1, got %s", oldTool.ToolCallID)
	}
	if strings.Contains(oldTool.Content, strings.Repeat("x", 100)) {
		t.Error("old tool result should be truncated")
	}
	if !strings.Contains(oldTool.Content, "read_file") {
		t.Error("truncated stub should include tool name")
	}
	if !strings.Contains(oldTool.Content, "truncated") {
		t.Error("truncated stub should include 'truncated' label")
	}

	// Recent tool result (tc2) should be preserved.
	recentTool := got[7]
	if recentTool.Content != largeContent {
		t.Error("recent tool result should be preserved verbatim")
	}
}

func TestCompactMessages_PreservesSmallToolResults(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "system"},
		// Turn 1
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "ok", ToolCalls: []ToolCall{
			{ID: "tc1", Type: "function", Function: FunctionCall{Name: "go_to_line"}},
		}},
		{Role: "tool", ToolCallID: "tc1", Content: "navigated to line 5"},
		{Role: "assistant", Content: "done"},
		// Turn 2
		{Role: "user", Content: "next"},
		{Role: "assistant", Content: "sure"},
	}

	got := CompactMessages(msgs, 1, 100)
	// Small tool result (19 bytes < 100) should not be truncated.
	if got[3].Content != "navigated to line 5" {
		t.Errorf("small tool result should be preserved, got %q", got[3].Content)
	}
	// Returns original slice since nothing was actually compacted.
	if &got[0] != &msgs[0] {
		t.Error("should return original slice when no content was truncated")
	}
}

func TestCompactMessages_KeepTurnsOne(t *testing.T) {
	large := strings.Repeat("data\n", 200)
	msgs := []Message{
		{Role: "system", Content: "system"},
		// Turn 1 (old)
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "a1", ToolCalls: []ToolCall{
			{ID: "t1", Type: "function", Function: FunctionCall{Name: "bash"}},
		}},
		{Role: "tool", ToolCallID: "t1", Content: large},
		{Role: "assistant", Content: "a2"},
		// Turn 2 (old)
		{Role: "user", Content: "second"},
		{Role: "assistant", Content: "a3", ToolCalls: []ToolCall{
			{ID: "t2", Type: "function", Function: FunctionCall{Name: "read_file"}},
		}},
		{Role: "tool", ToolCallID: "t2", Content: large},
		{Role: "assistant", Content: "a4"},
		// Turn 3 (kept)
		{Role: "user", Content: "third"},
		{Role: "assistant", Content: "a5"},
	}

	got := CompactMessages(msgs, 1, 100)

	// Both old tool results should be truncated.
	if !strings.Contains(got[3].Content, "truncated") {
		t.Error("turn 1 tool result should be truncated")
	}
	if !strings.Contains(got[7].Content, "truncated") {
		t.Error("turn 2 tool result should be truncated")
	}

	// Recent turn should be intact.
	if got[9].Content != "third" {
		t.Error("recent user message should be preserved")
	}
}

func TestCompactMessages_MultipleToolCallsPerTurn(t *testing.T) {
	large := strings.Repeat("line\n", 100)
	msgs := []Message{
		{Role: "system", Content: "sys"},
		// Turn 1 (old) — two tool calls
		{Role: "user", Content: "go"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "a", Type: "function", Function: FunctionCall{Name: "read_file"}},
			{ID: "b", Type: "function", Function: FunctionCall{Name: "search_project"}},
		}},
		{Role: "tool", ToolCallID: "a", Content: large},
		{Role: "tool", ToolCallID: "b", Content: large},
		{Role: "assistant", Content: "found it"},
		// Turn 2 (kept)
		{Role: "user", Content: "apply"},
		{Role: "assistant", Content: "done"},
	}

	got := CompactMessages(msgs, 1, 50)
	if !strings.Contains(got[3].Content, "read_file") {
		t.Error("tool a stub should mention read_file")
	}
	if !strings.Contains(got[4].Content, "search_project") {
		t.Error("tool b stub should mention search_project")
	}
}

func TestCompactMessages_PreservesAssistantContent(t *testing.T) {
	large := strings.Repeat("x", 500)
	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "detailed reasoning about the problem"},
		// Turn 2
		{Role: "user", Content: "q2"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "t1", Type: "function", Function: FunctionCall{Name: "bash"}},
		}},
		{Role: "tool", ToolCallID: "t1", Content: large},
		{Role: "assistant", Content: "result"},
		// Turn 3 (kept)
		{Role: "user", Content: "q3"},
		{Role: "assistant", Content: "answer"},
	}

	got := CompactMessages(msgs, 1, 100)
	// Old assistant content should be preserved (not truncated).
	if got[2].Content != "detailed reasoning about the problem" {
		t.Error("assistant content should be preserved")
	}
}

func TestCompactMessages_EmptyOrMinimal(t *testing.T) {
	tests := []struct {
		name string
		msgs []Message
	}{
		{"nil", nil},
		{"empty", []Message{}},
		{"system only", []Message{{Role: "system", Content: "hi"}}},
		{"system+user", []Message{{Role: "system"}, {Role: "user", Content: "hi"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CompactMessages(tt.msgs, 2, 100)
			if len(got) != len(tt.msgs) {
				t.Errorf("expected %d messages, got %d", len(tt.msgs), len(got))
			}
		})
	}
}

func TestTruncateToolResult_IncludesFirstLine(t *testing.T) {
	content := "package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"hello\") }\n"
	stub := truncateToolResult("read_file", content)
	if !strings.Contains(stub, "package main") {
		t.Error("stub should include first line as preview")
	}
	if !strings.Contains(stub, "read_file") {
		t.Error("stub should include tool name")
	}
}

func TestTruncateToolResult_UnknownTool(t *testing.T) {
	stub := truncateToolResult("", "some content\nmore content")
	if !strings.Contains(stub, "tool result") {
		t.Error("stub for unknown tool should use generic label")
	}
}
