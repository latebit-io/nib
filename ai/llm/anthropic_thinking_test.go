package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

// newAnthropicState returns an empty Anthropic stream state.
func newAnthropicState() *anthropicStreamState {
	return &anthropicStreamState{blocks: make(map[int]*anthropicBlockState)}
}

func TestAnthropicStream_CapturesThinking(t *testing.T) {
	s := newAnthropicState()

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
	s := newAnthropicState()
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
	s := newAnthropicState()
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
	s := newAnthropicState()
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
	s := newAnthropicState()
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
	if blocks[0].Type != "thinking" || blocks[0].Thinking == nil || *blocks[0].Thinking != "reasoned" || blocks[0].Signature != "S1" {
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

// Adaptive thinking with the default display returns thinking blocks with
// empty text; the replay must still carry "thinking":"" next to the signature.
func TestConvertAssistantMessage_ReplaysEmptyThinkingText(t *testing.T) {
	am := convertAssistantMessage(Message{
		Role:      "assistant",
		Content:   "done",
		Reasoning: &ReasoningTrace{Blocks: []ReasoningBlock{{Type: "thinking", Text: "", Signature: "S1"}}},
	})
	data, err := json.Marshal(am)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"thinking":""`) {
		t.Errorf("replayed thinking block dropped its empty thinking key: %s", data)
	}
	// Non-thinking blocks must not grow a thinking key.
	if n := strings.Count(string(data), `"thinking":`); n != 1 {
		t.Errorf("thinking key count = %d, want 1 (thinking block only): %s", n, data)
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

func TestAnthropicRequest_BudgetThinkingOnPre46Models(t *testing.T) {
	a := NewAnthropicAPI("https://x", "claude-sonnet-4-5", AnthropicKeyAuth("k"), false, EffortHigh)
	req := a.buildAnthropicRequest([]Message{{Role: "user", Content: "hi"}}, nil)

	if req.Thinking == nil || req.Thinking.Type != "enabled" || req.Thinking.BudgetTokens != 8192 {
		t.Fatalf("Thinking = %+v, want enabled/8192", req.Thinking)
	}
	if req.OutputConfig != nil {
		t.Errorf("OutputConfig = %+v, want nil on the budget path", req.OutputConfig)
	}
	// max_tokens must exceed the budget (it covers thinking + output).
	if req.MaxTokens != 8192+anthropicDefaultMaxTokens {
		t.Errorf("MaxTokens = %d, want budget+default %d", req.MaxTokens, 8192+anthropicDefaultMaxTokens)
	}

	// The max tier's budget still leaves max_tokens > budget.
	am := NewAnthropicAPI("https://x", "claude-haiku-4-5", AnthropicKeyAuth("k"), false, EffortMax)
	if r := am.buildAnthropicRequest(nil, nil); r.MaxTokens <= r.Thinking.BudgetTokens {
		t.Errorf("max tier: max_tokens (%d) <= budget (%d)", r.MaxTokens, r.Thinking.BudgetTokens)
	}
}

// Current models reject budget_tokens with a 400; effort must go through
// adaptive thinking and output_config.effort.
func TestAnthropicRequest_AdaptiveThinkingOnCurrentModels(t *testing.T) {
	a := NewAnthropicAPI("https://x", "claude-sonnet-5-5", AnthropicKeyAuth("k"), false, EffortHigh)
	req := a.buildAnthropicRequest([]Message{{Role: "user", Content: "hi"}}, nil)

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(data)
	for _, want := range []string{`"thinking":{"type":"adaptive"}`, `"output_config":{"effort":"high"}`} {
		if !strings.Contains(body, want) {
			t.Errorf("request JSON missing %s; got %s", want, body)
		}
	}
	if strings.Contains(body, "budget_tokens") {
		t.Errorf("request JSON must not send budget_tokens to a current model; got %s", body)
	}
	if req.MaxTokens != 8192+anthropicDefaultMaxTokens {
		t.Errorf("MaxTokens = %d, want headroom+default %d", req.MaxTokens, 8192+anthropicDefaultMaxTokens)
	}
}

func TestAnthropicBudgetThinking(t *testing.T) {
	cases := map[string]bool{
		"claude-3-7-sonnet-20250219": true, "claude-sonnet-4-20250514": true, "claude-opus-4-1-20250805": true,
		"claude-sonnet-4-5": true, "claude-haiku-4-5-20251001": true, "claude-opus-4-5": true,
		"claude-opus-4-6": false, "claude-sonnet-4-6": false, "claude-opus-4-8": false,
		"claude-sonnet-5-5": false, "claude-opus-5-5": false, "claude-haiku-5-5": false, "claude-fable-5-1": false,
	}
	for model, want := range cases {
		if got := anthropicBudgetThinking(model); got != want {
			t.Errorf("anthropicBudgetThinking(%q) = %v, want %v", model, got, want)
		}
	}
}

func TestAnthropicEffortStepsDownXHighOn46(t *testing.T) {
	cases := []struct {
		model string
		e     Effort
		want  string
	}{
		{"claude-opus-4-6", EffortXHigh, "high"},
		{"claude-sonnet-4-6", EffortXHigh, "high"},
		{"claude-opus-4-7", EffortXHigh, "xhigh"},
		{"claude-sonnet-5-5", EffortMax, "max"},
	}
	for _, tc := range cases {
		if got := anthropicEffort(tc.model, tc.e); got != tc.want {
			t.Errorf("anthropicEffort(%q, %q) = %q, want %q", tc.model, tc.e, got, tc.want)
		}
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
	if strings.Contains(string(data), "thinking") || strings.Contains(string(data), "output_config") {
		t.Errorf("request JSON must omit thinking and output_config when effort is unset; got %s", data)
	}
}
