package llm

import "testing"

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"", 0},
		{"a", 1},
		{"abcd", 1},
		{"abcde", 2},
		{"hello world", 3}, // 11 chars → ceil(11/4) = 3
		{"func main() {\n\tfmt.Println(\"hello\")\n}", 10},
	}
	for _, tt := range tests {
		got := EstimateTokens(tt.input)
		if got != tt.want {
			t.Errorf("EstimateTokens(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestEstimateMessageTokens_Empty(t *testing.T) {
	est := EstimateMessageTokens(nil, nil)
	if est.Total != 0 {
		t.Errorf("Total = %d, want 0", est.Total)
	}
}

func TestEstimateMessageTokens_SystemOnly(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "You are helpful."},
	}
	est := EstimateMessageTokens(msgs, nil)
	if est.System == 0 {
		t.Error("System estimate should be non-zero")
	}
	if est.History != 0 {
		t.Errorf("History = %d, want 0", est.History)
	}
	if est.Total != est.System {
		t.Errorf("Total = %d, want System=%d", est.Total, est.System)
	}
}

func TestEstimateMessageTokens_FirstTurn(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "You are a coding assistant."},
		{Role: "user", Content: "Fix this bug in main.go"},
	}
	tools := []ToolDef{
		{Type: "function", Function: FunctionDef{Name: "read_file", Description: "Read a file"}},
	}
	est := EstimateMessageTokens(msgs, tools)
	if est.System == 0 {
		t.Error("System should be non-zero")
	}
	if est.Tools == 0 {
		t.Error("Tools should be non-zero")
	}
	if est.History != 0 {
		t.Errorf("History = %d, want 0 (first turn)", est.History)
	}
	if est.New == 0 {
		t.Error("New should be non-zero (user message)")
	}
}

func TestEstimateMessageTokens_MultiTurn(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "You are a coding assistant."},
		{Role: "user", Content: "Fix the bug"},
		{Role: "assistant", Content: "I'll read the file first."},
		{Role: "user", Content: "Now edit it"},
	}
	est := EstimateMessageTokens(msgs, nil)
	if est.History == 0 {
		t.Error("History should be non-zero (user+assistant from prior turn)")
	}
	if est.New == 0 {
		t.Error("New should be non-zero (latest user message)")
	}
}

func TestEstimateMessageTokens_ToolCalls(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "System prompt"},
		{Role: "user", Content: "Do something"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "tc1", Type: "function", Function: FunctionCall{Name: "read_file", Arguments: `{"path":"main.go"}`}},
		}},
		{Role: "tool", ToolCallID: "tc1", Content: "package main\n\nfunc main() {}"},
		{Role: "assistant", Content: "I see the file."},
		{Role: "user", Content: "Edit it"},
	}
	est := EstimateMessageTokens(msgs, nil)
	if est.History == 0 {
		t.Error("History should include prior turns")
	}
	if est.New == 0 {
		t.Error("New should be the latest user message")
	}
}

func TestEstimateMessageTokensTotalMatchesParts(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "Long system prompt with many details about behavior."},
		{Role: "user", Content: "First question"},
		{Role: "assistant", Content: "First answer with some detail."},
		{Role: "user", Content: "Follow up question"},
		{Role: "assistant", Content: "Another response."},
		{Role: "user", Content: "Final question"},
	}
	tools := []ToolDef{
		{Type: "function", Function: FunctionDef{Name: "read_file", Description: "Read file content"}},
		{Type: "function", Function: FunctionDef{Name: "edit_file", Description: "Edit a file"}},
	}

	est := EstimateMessageTokens(msgs, tools)
	sum := est.System + est.Tools + est.History + est.New
	if est.Total != sum {
		t.Errorf("Total = %d, sum of parts = %d (sys:%d tools:%d hist:%d new:%d)",
			est.Total, sum, est.System, est.Tools, est.History, est.New)
	}
}
