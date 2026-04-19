package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/latebit-io/junto/engine/llm"
)

func execRequestInput(t *testing.T, args string) ToolResult {
	t.Helper()
	tool := NewRequestInputTool()
	return tool.Execute(context.Background(), llm.ToolCall{
		ID:   "call-1",
		Type: "function",
		Function: llm.FunctionCall{
			Name:      "request_input",
			Arguments: args,
		},
	})
}

func TestRequestInput_ValidWithOptions(t *testing.T) {
	result := execRequestInput(t, `{
		"prompt": "Fix all or only new?",
		"reason": "Lint found pre-existing violations.",
		"options": [
			{"id": "fix-all", "label": "Fix every violation"},
			{"id": "fix-new", "label": "Only fix new ones"}
		]
	}`)
	if result.Effect != EffectAwaitingInput {
		t.Fatalf("Effect = %d, want EffectAwaitingInput", result.Effect)
	}
	payload, ok := result.Payload.(AwaitingInputPayload)
	if !ok {
		t.Fatalf("Payload type = %T, want AwaitingInputPayload", result.Payload)
	}
	if payload.Prompt != "Fix all or only new?" {
		t.Errorf("Prompt = %q", payload.Prompt)
	}
	if payload.Reason != "Lint found pre-existing violations." {
		t.Errorf("Reason = %q", payload.Reason)
	}
	if payload.CallID != "call-1" {
		t.Errorf("CallID = %q, want call-1", payload.CallID)
	}
	if len(payload.Options) != 2 {
		t.Fatalf("Options len = %d, want 2", len(payload.Options))
	}
	if payload.Options[0].ID != "fix-all" || payload.Options[0].Label != "Fix every violation" {
		t.Errorf("Options[0] = %+v", payload.Options[0])
	}
}

func TestRequestInput_FreeFormNoOptions(t *testing.T) {
	result := execRequestInput(t, `{"prompt": "What module should this live in?"}`)
	if result.Effect != EffectAwaitingInput {
		t.Fatalf("Effect = %d, want EffectAwaitingInput", result.Effect)
	}
	payload := result.Payload.(AwaitingInputPayload)
	if len(payload.Options) != 0 {
		t.Errorf("Options should be empty, got %d", len(payload.Options))
	}
}

func TestRequestInput_EmptyPromptRejected(t *testing.T) {
	result := execRequestInput(t, `{"prompt": ""}`)
	if result.Effect != EffectNone {
		t.Errorf("empty prompt should return EffectNone, got %d", result.Effect)
	}
	if !strings.Contains(result.Content, "prompt is required") {
		t.Errorf("expected prompt-required error, got %q", result.Content)
	}
}

func TestRequestInput_WhitespaceOnlyPromptRejected(t *testing.T) {
	result := execRequestInput(t, `{"prompt": "   \t  "}`)
	if result.Effect != EffectNone {
		t.Errorf("whitespace prompt should return EffectNone, got %d", result.Effect)
	}
	if !strings.Contains(result.Content, "prompt is required") {
		t.Errorf("expected prompt-required error, got %q", result.Content)
	}
}

func TestRequestInput_TooManyOptions(t *testing.T) {
	var opts strings.Builder
	opts.WriteString(`{"prompt": "pick", "options": [`)
	for i := 0; i < maxRequestInputOptions+1; i++ {
		if i > 0 {
			opts.WriteString(",")
		}
		fmt.Fprintf(&opts, `{"id":"a%d","label":"L"}`, i)
	}
	opts.WriteString(`]}`)
	result := execRequestInput(t, opts.String())
	if result.Effect != EffectNone {
		t.Errorf("too many options should return EffectNone, got %d", result.Effect)
	}
	if !strings.Contains(result.Content, "at most") {
		t.Errorf("expected too-many-options error, got %q", result.Content)
	}
}

func TestRequestInput_DuplicateOptionID(t *testing.T) {
	result := execRequestInput(t, `{"prompt": "pick", "options": [
		{"id":"a","label":"A"},
		{"id":"a","label":"B"}
	]}`)
	if !strings.Contains(result.Content, "duplicate option id") {
		t.Errorf("expected duplicate-id error, got %q", result.Content)
	}
}

func TestRequestInput_MissingOptionID(t *testing.T) {
	result := execRequestInput(t, `{"prompt": "pick", "options": [{"label":"no id"}]}`)
	if !strings.Contains(result.Content, "option[0].id is required") {
		t.Errorf("expected missing-id error, got %q", result.Content)
	}
}

func TestRequestInput_MissingOptionLabel(t *testing.T) {
	result := execRequestInput(t, `{"prompt": "pick", "options": [{"id":"a"}]}`)
	if !strings.Contains(result.Content, "option[0].label is required") {
		t.Errorf("expected missing-label error, got %q", result.Content)
	}
}

func TestRequestInput_InvalidJSON(t *testing.T) {
	result := execRequestInput(t, `not json`)
	if !strings.HasPrefix(result.Content, "Error: invalid arguments:") {
		t.Errorf("got %q", result.Content)
	}
}

func TestRequestInput_CanceledContext(t *testing.T) {
	tool := NewRequestInputTool()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := tool.Execute(ctx, llm.ToolCall{
		Function: llm.FunctionCall{Name: "request_input", Arguments: `{"prompt":"x"}`},
	})
	if !strings.Contains(result.Content, "canceled") {
		t.Errorf("expected cancel error, got %q", result.Content)
	}
}

func TestRequestInput_OversizedPromptRejected(t *testing.T) {
	big := strings.Repeat("x", maxRequestInputPromptBytes+1)
	result := execRequestInput(t, fmt.Sprintf(`{"prompt": %q}`, big))
	if result.Effect != EffectNone {
		t.Errorf("oversized prompt should return EffectNone, got %d", result.Effect)
	}
	if !strings.Contains(result.Content, "prompt exceeds") {
		t.Errorf("expected prompt-oversize error, got %q", result.Content)
	}
}

func TestRequestInput_OversizedReasonRejected(t *testing.T) {
	big := strings.Repeat("x", maxRequestInputReasonBytes+1)
	result := execRequestInput(t, fmt.Sprintf(`{"prompt": "ok", "reason": %q}`, big))
	if !strings.Contains(result.Content, "reason exceeds") {
		t.Errorf("expected reason-oversize error, got %q", result.Content)
	}
}

func TestRequestInput_OversizedOptionIDRejected(t *testing.T) {
	big := strings.Repeat("a", maxRequestInputOptionIDLen+1)
	args := fmt.Sprintf(`{"prompt":"ok","options":[{"id":%q,"label":"L"}]}`, big)
	result := execRequestInput(t, args)
	if !strings.Contains(result.Content, "option[0].id exceeds") {
		t.Errorf("expected option-id-oversize error, got %q", result.Content)
	}
}

func TestRequestInput_OversizedOptionLabelRejected(t *testing.T) {
	big := strings.Repeat("x", maxRequestInputLabelBytes+1)
	args := fmt.Sprintf(`{"prompt":"ok","options":[{"id":"a","label":%q}]}`, big)
	result := execRequestInput(t, args)
	if !strings.Contains(result.Content, "option[0].label exceeds") {
		t.Errorf("expected option-label-oversize error, got %q", result.Content)
	}
}

func TestRequestInput_DefinitionSchema(t *testing.T) {
	tool := NewRequestInputTool()
	def := tool.Definition()
	if def.Function.Name != "request_input" {
		t.Errorf("tool name = %q, want request_input", def.Function.Name)
	}
	if len(def.Function.Parameters.Required) != 1 || def.Function.Parameters.Required[0] != "prompt" {
		t.Errorf("Required = %v, want [prompt]", def.Function.Parameters.Required)
	}
	if _, ok := def.Function.Parameters.Properties["prompt"]; !ok {
		t.Error("missing prompt property")
	}
	if _, ok := def.Function.Parameters.Properties["options"]; !ok {
		t.Error("missing options property")
	}
	if _, ok := def.Function.Parameters.Properties["reason"]; !ok {
		t.Error("missing reason property")
	}
}
