package llm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// assertSystemCached checks the system message has cache_control content blocks.
func assertSystemCached(t *testing.T, cms []cachingMessage, origContent string) {
	t.Helper()
	if len(cms) == 0 {
		t.Fatal("expected messages but got none")
	}
	blocks, ok := cms[0].Content.([]contentBlock)
	if !ok {
		t.Fatalf("system content type = %T, want []contentBlock", cms[0].Content)
	}
	if len(blocks) != 1 || blocks[0].CacheControl == nil {
		t.Error("system message should have cache_control")
	}
	if blocks[0].Text != origContent {
		t.Errorf("system text = %q, want %q", blocks[0].Text, origContent)
	}
}

// assertPenultimateCached checks the second-to-last message has cache_control.
func assertPenultimateCached(t *testing.T, cms []cachingMessage) {
	t.Helper()
	idx := len(cms) - 2
	blocks, ok := cms[idx].Content.([]contentBlock)
	if !ok {
		t.Fatalf("penultimate content type = %T, want []contentBlock", cms[idx].Content)
	}
	if len(blocks) != 1 || blocks[0].CacheControl == nil {
		t.Error("penultimate message should have cache_control")
	}
}

// assertLastNotCached checks the last message is a plain string, not content blocks.
func assertLastNotCached(t *testing.T, cms []cachingMessage) {
	t.Helper()
	last := cms[len(cms)-1]
	if _, ok := last.Content.([]contentBlock); ok {
		t.Error("last message should not have content blocks (should be plain string)")
	}
}

// assertToolsCached checks only the last tool has cache_control.
func assertToolsCached(t *testing.T, cts []cachingToolDef) {
	t.Helper()
	if len(cts) == 0 {
		t.Fatal("expected tools but got none")
	}
	if cts[len(cts)-1].CacheControl == nil {
		t.Error("last tool should have cache_control")
	}
	for i := 0; i < len(cts)-1; i++ {
		if cts[i].CacheControl != nil {
			t.Errorf("tool[%d] should not have cache_control", i)
		}
	}
}

func TestAnnotateCacheBreakpoints(t *testing.T) {
	tests := []struct {
		name           string
		messages       []Message
		tools          []ToolDef
		wantSysCached  bool
		wantPenCached  bool
		wantToolCached bool
	}{
		{
			name: "system and penultimate cached in multi-turn",
			messages: []Message{
				{Role: "system", Content: "You are helpful."},
				{Role: "user", Content: "Hello"},
				{Role: "assistant", Content: "Hi there!"},
				{Role: "user", Content: "New question"},
			},
			tools: []ToolDef{
				{Type: "function", Function: FunctionDef{Name: "read_file"}},
				{Type: "function", Function: FunctionDef{Name: "edit_file"}},
			},
			wantSysCached:  true,
			wantPenCached:  true,
			wantToolCached: true,
		},
		{
			name: "two messages: system cached, no penultimate",
			messages: []Message{
				{Role: "system", Content: "System prompt"},
				{Role: "user", Content: "First message"},
			},
			wantSysCached: true,
		},
		{
			name: "single message: system cached only",
			messages: []Message{
				{Role: "system", Content: "System prompt"},
			},
			wantSysCached: true,
		},
		{
			name: "empty messages and tools",
		},
		{
			name: "tools cached even with minimal messages",
			messages: []Message{
				{Role: "system", Content: "Sys"},
				{Role: "user", Content: "Hi"},
			},
			tools: []ToolDef{
				{Type: "function", Function: FunctionDef{Name: "only_tool"}},
			},
			wantSysCached:  true,
			wantToolCached: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cms, cts := annotateCacheBreakpoints(tt.messages, tt.tools)

			if tt.wantSysCached {
				assertSystemCached(t, cms, tt.messages[0].Content)
			}
			if tt.wantPenCached && len(cms) >= 3 {
				assertPenultimateCached(t, cms)
			}
			if len(cms) > 1 {
				assertLastNotCached(t, cms)
			}
			if tt.wantToolCached {
				assertToolsCached(t, cts)
			}
		})
	}
}

func TestAnnotateCacheBreakpointsPreservesFields(t *testing.T) {
	messages := []Message{
		{Role: "system", Content: "sys"},
		{Role: "assistant", Content: "response", ToolCalls: []ToolCall{
			{ID: "tc1", Type: "function", Function: FunctionCall{Name: "read_file", Arguments: "{}"}},
		}},
		{Role: "tool", ToolCallID: "tc1", Content: "file contents"},
		{Role: "user", Content: "next question"},
	}
	tools := []ToolDef{
		{Type: "function", Function: FunctionDef{Name: "read_file", Description: "Read a file"}},
	}

	cms, cts := annotateCacheBreakpoints(messages, tools)

	if len(cms[1].ToolCalls) != 1 || cms[1].ToolCalls[0].ID != "tc1" {
		t.Error("assistant message tool calls not preserved")
	}
	if cms[2].ToolCallID != "tc1" {
		t.Errorf("tool message ToolCallID = %q, want tc1", cms[2].ToolCallID)
	}
	if cts[0].Function.Name != "read_file" {
		t.Errorf("tool name = %q, want read_file", cts[0].Function.Name)
	}
	if cts[0].Function.Description != "Read a file" {
		t.Errorf("tool description = %q", cts[0].Function.Description)
	}
}

func TestCachingChatRequestJSON(t *testing.T) {
	messages := []Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "Hello"},
		{Role: "assistant", Content: "Hi!"},
		{Role: "user", Content: "Question"},
	}
	tools := []ToolDef{
		{Type: "function", Function: FunctionDef{Name: "test_tool"}},
	}

	cms, cts := annotateCacheBreakpoints(messages, tools)
	req := cachingChatRequest{
		Model:    "anthropic/claude-sonnet-4",
		Messages: cms,
		Stream:   true,
		Tools:    cts,
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}

	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(raw["messages"], &msgs); err != nil {
		t.Fatalf("unmarshal messages: %v", err)
	}

	// System message content should be an array of content blocks.
	var sysContent []map[string]any
	if err := json.Unmarshal(msgs[0]["content"], &sysContent); err != nil {
		t.Fatalf("system content should be array: %v", err)
	}
	if len(sysContent) != 1 {
		t.Fatalf("system content blocks = %d, want 1", len(sysContent))
	}
	if sysContent[0]["type"] != "text" {
		t.Errorf("system block type = %v, want text", sysContent[0]["type"])
	}
	cc, ok := sysContent[0]["cache_control"].(map[string]any)
	if !ok {
		t.Fatal("system block missing cache_control")
	}
	if cc["type"] != "ephemeral" {
		t.Errorf("cache_control type = %v, want ephemeral", cc["type"])
	}

	// Last message content should be a plain string.
	var lastContent string
	if err := json.Unmarshal(msgs[3]["content"], &lastContent); err != nil {
		t.Fatalf("last message content should be string: %v", err)
	}
	if lastContent != "Question" {
		t.Errorf("last content = %q, want Question", lastContent)
	}

	// Tool should have cache_control.
	var rawTools []map[string]json.RawMessage
	if err := json.Unmarshal(raw["tools"], &rawTools); err != nil {
		t.Fatalf("unmarshal tools: %v", err)
	}
	var toolCC map[string]string
	if err := json.Unmarshal(rawTools[0]["cache_control"], &toolCC); err != nil {
		t.Fatalf("tool cache_control: %v", err)
	}
	if toolCC["type"] != "ephemeral" {
		t.Errorf("tool cache_control type = %q", toolCC["type"])
	}
}

func TestCachingChatRequestJSON_AssistantToolCall(t *testing.T) {
	messages := []Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "Read the file"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "tc_1", Type: "function", Function: FunctionCall{Name: "read_file", Arguments: `{"path":"main.go"}`}},
		}},
		{Role: "tool", ToolCallID: "tc_1", Content: "package main"},
		{Role: "user", Content: "Now edit it"},
	}
	tools := []ToolDef{
		{Type: "function", Function: FunctionDef{Name: "read_file"}},
	}

	cms, cts := annotateCacheBreakpoints(messages, tools)
	req := cachingChatRequest{
		Model:    "anthropic/claude-sonnet-4",
		Messages: cms,
		Stream:   true,
		Tools:    cts,
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Assistant message (index 2): empty content + tool_calls present.
	var assistantRaw map[string]json.RawMessage
	if err := json.Unmarshal(raw.Messages[2], &assistantRaw); err != nil {
		t.Fatalf("unmarshal assistant: %v", err)
	}

	// Content field should be omitted entirely (matching Message's omitempty behavior).
	if _, hasContent := assistantRaw["content"]; hasContent {
		t.Errorf("assistant message should omit content field, got: %s", assistantRaw["content"])
	}

	// tool_calls must be present with the correct structure.
	tcRaw, ok := assistantRaw["tool_calls"]
	if !ok {
		t.Fatal("assistant message missing tool_calls field")
	}
	var toolCalls []map[string]any
	if err := json.Unmarshal(tcRaw, &toolCalls); err != nil {
		t.Fatalf("unmarshal tool_calls: %v", err)
	}
	if len(toolCalls) != 1 {
		t.Fatalf("tool_calls count = %d, want 1", len(toolCalls))
	}
	if toolCalls[0]["id"] != "tc_1" {
		t.Errorf("tool_call id = %v, want tc_1", toolCalls[0]["id"])
	}

	// cache_control should NOT appear on the assistant message
	// (empty content is not converted to content blocks).
	if _, hasCacheControl := assistantRaw["cache_control"]; hasCacheControl {
		t.Error("assistant message should not have top-level cache_control")
	}

	// Tool result message (index 3) is penultimate — should be cached.
	var toolRaw map[string]json.RawMessage
	if err := json.Unmarshal(raw.Messages[3], &toolRaw); err != nil {
		t.Fatalf("unmarshal tool msg: %v", err)
	}
	var toolContent []map[string]any
	if err := json.Unmarshal(toolRaw["content"], &toolContent); err != nil {
		t.Fatalf("penultimate tool content should be array: %v", err)
	}
	if len(toolContent) != 1 {
		t.Fatalf("tool content blocks = %d, want 1", len(toolContent))
	}
	if _, ok := toolContent[0]["cache_control"]; !ok {
		t.Error("penultimate tool message should have cache_control")
	}
}

func TestNonCachingRequestOmitsCacheControl(t *testing.T) {
	req := chatRequest{
		Model: "test",
		Messages: []Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "hi"},
		},
		Stream: true,
		Tools: []ToolDef{
			{Type: "function", Function: FunctionDef{Name: "t"}},
		},
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if s := string(data); strings.Contains(s, "cache_control") {
		t.Errorf("non-caching request should not contain cache_control: %s", s)
	}
}

func TestParseUsage(t *testing.T) {
	t.Run("nil input", func(t *testing.T) {
		if got := parseUsage(nil); got != nil {
			t.Errorf("parseUsage(nil) = %+v, want nil", got)
		}
	})

	t.Run("basic usage", func(t *testing.T) {
		u := &sseUsage{PromptTokens: 100, CompletionTokens: 50}
		got := parseUsage(u)
		if got == nil {
			t.Fatal("parseUsage returned nil")
		}
		if got.PromptTokens != 100 {
			t.Errorf("PromptTokens = %d, want 100", got.PromptTokens)
		}
		if got.CompletionTokens != 50 {
			t.Errorf("CompletionTokens = %d, want 50", got.CompletionTokens)
		}
		if got.CachedTokens != 0 {
			t.Errorf("CachedTokens = %d, want 0", got.CachedTokens)
		}
	})

	t.Run("with cached tokens", func(t *testing.T) {
		// Parse from JSON to correctly populate the anonymous struct with JSON tags.
		raw := `{"prompt_tokens":1000,"completion_tokens":200,"prompt_tokens_details":{"cached_tokens":800}}`
		var u sseUsage
		if err := json.Unmarshal([]byte(raw), &u); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		got := parseUsage(&u)
		if got == nil {
			t.Fatal("parseUsage returned nil")
		}
		if got.CachedTokens != 800 {
			t.Errorf("CachedTokens = %d, want 800", got.CachedTokens)
		}
	})
}

func TestStreamOptionsInRequest(t *testing.T) {
	t.Run("non-caching request includes stream_options", func(t *testing.T) {
		req := chatRequest{
			Model:         "test",
			Messages:      []Message{{Role: "user", Content: "hi"}},
			Stream:        true,
			StreamOptions: includeUsage,
		}
		data, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(data), `"stream_options"`) {
			t.Error("request should include stream_options")
		}
		if !strings.Contains(string(data), `"include_usage":true`) {
			t.Error("stream_options should have include_usage:true")
		}
	})

	t.Run("caching request includes stream_options", func(t *testing.T) {
		req := cachingChatRequest{
			Model:         "test",
			Messages:      []cachingMessage{{Role: "user", Content: "hi"}},
			Stream:        true,
			StreamOptions: includeUsage,
		}
		data, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(data), `"include_usage":true`) {
			t.Error("caching request should have include_usage:true")
		}
	})
}

func TestSSEUsageParsing(t *testing.T) {
	// Simulate an SSE chunk with usage data embedded.
	raw := `{"choices":[{"delta":{"content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":1234,"completion_tokens":567,"prompt_tokens_details":{"cached_tokens":1000}}}`

	var chunk sseChunk
	if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if chunk.Usage == nil {
		t.Fatal("Usage should be parsed from SSE chunk")
	}
	if chunk.Usage.PromptTokens != 1234 {
		t.Errorf("PromptTokens = %d, want 1234", chunk.Usage.PromptTokens)
	}
	if chunk.Usage.CompletionTokens != 567 {
		t.Errorf("CompletionTokens = %d, want 567", chunk.Usage.CompletionTokens)
	}
	if chunk.Usage.PromptDetails == nil {
		t.Fatal("PromptDetails should be parsed")
	}
	if chunk.Usage.PromptDetails.CachedTokens != 1000 {
		t.Errorf("CachedTokens = %d, want 1000", chunk.Usage.PromptDetails.CachedTokens)
	}

	usage := parseUsage(chunk.Usage)
	if usage.CachedTokens != 1000 {
		t.Errorf("parsed CachedTokens = %d, want 1000", usage.CachedTokens)
	}
}

// collectStream drains s.handleChunk for a list of JSON chunk strings and
// returns the first Done event emitted. Tests that only care about the
// terminal event use this to skip over token deltas.
func collectFinalEvent(t *testing.T, chunks []string) StreamEvent {
	t.Helper()
	ch := make(chan StreamEvent, len(chunks)+1)
	state := &sseStreamState{}
	for _, c := range chunks {
		state.handleChunk(context.Background(), c, ch)
	}
	close(ch)
	for ev := range ch {
		if ev.Done {
			return ev
		}
	}
	t.Fatal("no Done event emitted")
	return StreamEvent{}
}

func TestSSEChunk_TruncatedFinishReason(t *testing.T) {
	tests := []struct {
		name         string
		finishReason string
		wantTrunc    bool
	}{
		{"length triggers truncated flag", "length", true},
		{"stop is a clean finish", "stop", false},
		{"tool_calls is a clean finish", "tool_calls", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			chunk := `{"choices":[{"delta":{"content":"partial"},"finish_reason":"` + tc.finishReason + `"}]}`
			ev := collectFinalEvent(t, []string{chunk})
			if ev.Truncated != tc.wantTrunc {
				t.Errorf("Truncated = %v, want %v", ev.Truncated, tc.wantTrunc)
			}
		})
	}
}
