package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func newThinkingState() *anthropicStreamState {
	return &anthropicStreamState{blocks: make(map[int]*anthropicBlockState)}
}

func TestAnthropicStream_CapturesThinking(t *testing.T) {
	s := newThinkingState()

	// A thinking block, then a tool_use block — reasoning must be captured
	// alongside the tool call, in order.
	if err := s.handleBlockStart(`{"index":0,"content_block":{"type":"thinking"}}`); err != nil {
		t.Fatalf("handleBlockStart(thinking): %v", err)
	}
	if tok, _ := s.handleBlockDelta(`{"index":0,"delta":{"type":"thinking_delta","thinking":"step one "}}`); tok != "" {
		t.Errorf("thinking_delta surfaced a token %q; reasoning must not be streamed as output", tok)
	}
	if tok, _ := s.handleBlockDelta(`{"index":0,"delta":{"type":"thinking_delta","thinking":"step two"}}`); tok != "" {
		t.Errorf("thinking_delta surfaced a token %q", tok)
	}
	_, _ = s.handleBlockDelta(`{"index":0,"delta":{"type":"signature_delta","signature":"SIG=="}}`)
	_ = s.handleBlockStart(`{"index":1,"content_block":{"type":"tool_use","id":"t1","name":"bash"}}`)
	_, _ = s.handleBlockDelta(`{"index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}`)

	ev := s.finalEvent()
	if ev.Reasoning == nil || len(ev.Reasoning.Blocks) != 1 {
		t.Fatalf("Reasoning = %+v; want one block", ev.Reasoning)
	}
	b := ev.Reasoning.Blocks[0]
	if b.Type != "thinking" || b.Text != "step one step two" || b.Signature != "SIG==" {
		t.Errorf("captured block = %+v; want thinking/'step one step two'/'SIG=='", b)
	}
	if len(ev.ToolCalls) != 1 || ev.ToolCalls[0].ID != "t1" {
		t.Errorf("tool call not captured alongside reasoning: %+v", ev.ToolCalls)
	}
}

func TestAnthropicStream_CapturesRedactedThinking(t *testing.T) {
	s := newThinkingState()
	_ = s.handleBlockStart(`{"index":0,"content_block":{"type":"redacted_thinking","data":"ENCRYPTED"}}`)
	ev := s.finalEvent()
	if ev.Reasoning == nil || len(ev.Reasoning.Blocks) != 1 {
		t.Fatalf("Reasoning = %+v; want one redacted block", ev.Reasoning)
	}
	b := ev.Reasoning.Blocks[0]
	if b.Type != "redacted_thinking" || b.Data != "ENCRYPTED" {
		t.Errorf("redacted block = %+v; want redacted_thinking/'ENCRYPTED'", b)
	}
}

func TestAnthropicStream_ThinkingOverflowAborts(t *testing.T) {
	// A thinking block whose text exceeds the cap must terminate the stream,
	// not silently truncate — a capped text would mismatch the signature and
	// 400 on the next replay turn.
	s := newThinkingState()
	if err := s.handleBlockStart(`{"index":0,"content_block":{"type":"thinking"}}`); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("x", maxThinkingBytes+1)
	payload, _ := json.Marshal(map[string]any{
		"index": 0,
		"delta": map[string]string{"type": "thinking_delta", "thinking": big},
	})
	_, err := s.handleBlockDelta(string(payload))
	if err == nil {
		t.Fatal("oversized thinking_delta did not abort the stream")
	}
	if !strings.Contains(err.Error(), "thinking") {
		t.Errorf("abort error = %q, want it to name the thinking overflow", err)
	}
}

func TestAnthropicStream_RedactedThinkingOverflowAborts(t *testing.T) {
	s := newThinkingState()
	big := strings.Repeat("x", maxThinkingBytes+1)
	payload, _ := json.Marshal(map[string]any{
		"index":         0,
		"content_block": map[string]string{"type": "redacted_thinking", "data": big},
	})
	err := s.handleBlockStart(string(payload))
	if err == nil {
		t.Fatal("oversized redacted_thinking block did not abort the stream")
	}
	if !strings.Contains(err.Error(), "redacted") {
		t.Errorf("abort error = %q, want it to name the redacted overflow", err)
	}
}

func TestAnthropicStream_NoThinkingLeavesReasoningNil(t *testing.T) {
	s := newThinkingState()
	_ = s.handleBlockStart(`{"index":0,"content_block":{"type":"text"}}`)
	_, _ = s.handleBlockDelta(`{"index":0,"delta":{"type":"text_delta","text":"hi"}}`)
	if ev := s.finalEvent(); ev.Reasoning != nil {
		t.Errorf("Reasoning = %+v; want nil for a non-thinking response", ev.Reasoning)
	}
}

func TestConvertAssistantMessage_ReplaysReasoningFirst(t *testing.T) {
	m := Message{
		Role:    "assistant",
		Content: "the answer",
		Reasoning: &ReasoningTrace{Blocks: []ReasoningBlock{
			{Type: "thinking", Text: "reasoned", Signature: "S1"},
			{Type: "redacted_thinking", Data: "R1"},
		}},
		ToolCalls: []ToolCall{{ID: "t1", Type: "function", Function: FunctionCall{Name: "bash", Arguments: `{"command":"ls"}`}}},
	}
	am := convertAssistantMessage(m)
	blocks, ok := am.Content.([]anthropicContent)
	if !ok {
		t.Fatalf("Content is %T, want []anthropicContent", am.Content)
	}
	// Order: thinking, redacted_thinking, text, tool_use.
	if len(blocks) != 4 {
		t.Fatalf("blocks = %d, want 4: %+v", len(blocks), blocks)
	}
	if blocks[0].Type != "thinking" || blocks[0].Thinking != "reasoned" || blocks[0].Signature != "S1" {
		t.Errorf("block[0] = %+v; want thinking/reasoned/S1", blocks[0])
	}
	if blocks[1].Type != "redacted_thinking" || blocks[1].Data != "R1" {
		t.Errorf("block[1] = %+v; want redacted_thinking/R1", blocks[1])
	}
	if blocks[2].Type != "text" || blocks[3].Type != "tool_use" {
		t.Errorf("reasoning must lead text/tool_use; got %q,%q", blocks[2].Type, blocks[3].Type)
	}

	// The wire form must place thinking before text so the API accepts it.
	data, _ := json.Marshal(am)
	if strings.Index(string(data), `"thinking"`) > strings.Index(string(data), `"text"`) {
		t.Errorf("thinking block must serialize before text; got %s", data)
	}
}

func TestConvertAssistantMessage_NoReasoningUnchanged(t *testing.T) {
	am := convertAssistantMessage(Message{Role: "assistant", Content: "plain"})
	blocks := am.Content.([]anthropicContent)
	if len(blocks) != 1 || blocks[0].Type != "text" {
		t.Fatalf("no-reasoning assistant message should be a single text block; got %+v", blocks)
	}
}

func TestAnthropicThinkingBudget(t *testing.T) {
	cases := map[Effort]int{
		EffortLow: 1024, EffortMedium: 4096, EffortHigh: 8192,
		EffortXHigh: 16384, EffortMax: 24576, "": 0, "bogus": 0,
	}
	for e, want := range cases {
		if got := anthropicThinkingBudget(e); got != want {
			t.Errorf("anthropicThinkingBudget(%q) = %d, want %d", e, got, want)
		}
	}
}

func TestAnthropicRequest_ThinkingEnabled(t *testing.T) {
	a := NewAnthropicAPI("https://x", "claude", AnthropicKeyAuth("k"), false, EffortHigh)
	req := a.buildAnthropicRequest([]Message{{Role: "user", Content: "hi"}}, nil)

	if req.Thinking == nil || req.Thinking.Type != "enabled" || req.Thinking.BudgetTokens != 8192 {
		t.Fatalf("Thinking = %+v, want enabled/8192", req.Thinking)
	}
	// max_tokens must exceed the budget (it covers thinking + output).
	if req.MaxTokens != 8192+anthropicDefaultMaxTokens {
		t.Errorf("MaxTokens = %d, want budget+default %d", req.MaxTokens, 8192+anthropicDefaultMaxTokens)
	}
	if req.MaxTokens <= req.Thinking.BudgetTokens {
		t.Errorf("max_tokens (%d) must be > budget_tokens (%d) or Anthropic 400s", req.MaxTokens, req.Thinking.BudgetTokens)
	}

	// The max tier's budget still leaves max_tokens > budget.
	am := NewAnthropicAPI("https://x", "claude", AnthropicKeyAuth("k"), false, EffortMax)
	if r := am.buildAnthropicRequest(nil, nil); r.MaxTokens <= r.Thinking.BudgetTokens {
		t.Errorf("max tier: max_tokens (%d) <= budget (%d)", r.MaxTokens, r.Thinking.BudgetTokens)
	}
}

func TestAnthropicRequest_ThinkingDisabledByDefault(t *testing.T) {
	a := NewAnthropicAPI("https://x", "claude", AnthropicKeyAuth("k"), false, "")
	req := a.buildAnthropicRequest([]Message{{Role: "user", Content: "hi"}}, nil)

	if req.Thinking != nil {
		t.Errorf("Thinking = %+v, want nil when effort unset", req.Thinking)
	}
	if req.MaxTokens != anthropicDefaultMaxTokens {
		t.Errorf("MaxTokens = %d, want unchanged default %d", req.MaxTokens, anthropicDefaultMaxTokens)
	}
	// The thinking field must be omitted entirely so existing runs are
	// byte-identical.
	data, _ := json.Marshal(req)
	if strings.Contains(string(data), "thinking") {
		t.Errorf("request JSON must omit thinking when disabled; got %s", data)
	}
}
