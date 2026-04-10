package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/junto/engine/llm"
)

// mockStreamProvider returns canned stream events for testing.
type mockStreamProvider struct {
	response string
	err      error
}

func (m *mockStreamProvider) Stream(_ context.Context, _ []llm.Message, _ []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	if m.err != nil {
		return nil, m.err
	}
	ch := make(chan llm.StreamEvent, 2)
	ch <- llm.StreamEvent{Token: m.response}
	ch <- llm.StreamEvent{Done: true}
	close(ch)
	return ch, nil
}

func TestStyleEvaluator_violations(t *testing.T) {
	provider := &mockStreamProvider{response: `["Adapter contains business logic", "Missing doc comment"]`}
	eval := NewStyleEvaluator(provider, []string{"rule1", "rule2"}, time.Second)

	violations, ok := eval.Review(context.Background(), "main.go", "old code", "new code")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if len(violations) != 2 {
		t.Fatalf("expected 2 violations, got %d", len(violations))
	}
	if violations[0] != "Adapter contains business logic" {
		t.Errorf("violations[0] = %q, want %q", violations[0], "Adapter contains business logic")
	}
	if violations[1] != "Missing doc comment" {
		t.Errorf("violations[1] = %q, want %q", violations[1], "Missing doc comment")
	}
}

func TestStyleEvaluator_clean(t *testing.T) {
	provider := &mockStreamProvider{response: `[]`}
	eval := NewStyleEvaluator(provider, []string{"rule1"}, time.Second)

	violations, ok := eval.Review(context.Background(), "main.go", "old", "new")
	if !ok {
		t.Fatal("expected ok=true for clean edit")
	}
	if violations != nil {
		t.Errorf("expected nil for clean edit, got %v", violations)
	}
}

func TestStyleEvaluator_providerError(t *testing.T) {
	provider := &mockStreamProvider{err: context.DeadlineExceeded}
	eval := NewStyleEvaluator(provider, []string{"rule1"}, time.Second)

	violations, ok := eval.Review(context.Background(), "main.go", "old", "new")
	if ok {
		t.Error("expected ok=false on provider error")
	}
	if violations != nil {
		t.Errorf("expected nil on provider error, got %v", violations)
	}
}

func TestStyleEvaluator_malformedJSON(t *testing.T) {
	provider := &mockStreamProvider{response: `not json at all`}
	eval := NewStyleEvaluator(provider, []string{"rule1"}, time.Second)

	violations, ok := eval.Review(context.Background(), "main.go", "old", "new")
	if !ok {
		t.Error("expected ok=true (malformed JSON is a completed review, just unparseable)")
	}
	if violations != nil {
		t.Errorf("expected nil on malformed JSON, got %v", violations)
	}
}

func TestStyleEvaluator_markdownFences(t *testing.T) {
	provider := &mockStreamProvider{response: "```json\n[\"violation\"]\n```"}
	eval := NewStyleEvaluator(provider, []string{"rule1"}, time.Second)

	violations, ok := eval.Review(context.Background(), "main.go", "old", "new")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation through markdown fences, got %d", len(violations))
	}
	if violations[0] != "violation" {
		t.Errorf("violation = %q, want %q", violations[0], "violation")
	}
}

func TestStyleEvaluator_emptyRules(t *testing.T) {
	provider := &mockStreamProvider{response: `["should not reach"]`}
	eval := NewStyleEvaluator(provider, nil, time.Second)

	violations, ok := eval.Review(context.Background(), "main.go", "old", "new")
	if !ok {
		t.Error("expected ok=true for empty rules")
	}
	if violations != nil {
		t.Errorf("expected nil when rules are empty, got %v", violations)
	}
}

func TestStyleEvaluator_timeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	provider := &mockStreamProvider{response: `["violation"]`}
	eval := NewStyleEvaluator(provider, []string{"rule1"}, time.Millisecond)

	violations, ok := eval.Review(ctx, "main.go", "old", "new")
	if ok {
		t.Error("expected ok=false on timeout")
	}
	if violations != nil {
		t.Errorf("expected nil on timeout, got %v", violations)
	}
}

func TestParseViolations(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantNil bool
		wantLen int
	}{
		{name: "valid array", input: `["a", "b"]`, wantLen: 2},
		{name: "empty array", input: `[]`, wantNil: true},
		{name: "empty string", input: ``, wantNil: true},
		{name: "not json", input: `hello`, wantNil: true},
		{name: "json object", input: `{"key": "val"}`, wantNil: true},
		{name: "with whitespace", input: `  ["a"]  `, wantLen: 1},
		{name: "markdown fences", input: "```json\n[\"a\"]\n```", wantLen: 1},
		{name: "markdown fences no lang", input: "```\n[\"a\"]\n```", wantLen: 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseViolations(tc.input)
			if tc.wantNil && got != nil {
				t.Errorf("expected nil, got %v", got)
			}
			if !tc.wantNil && len(got) != tc.wantLen {
				t.Errorf("len = %d, want %d", len(got), tc.wantLen)
			}
		})
	}
}

func TestBuildEvaluatorPrompt(t *testing.T) {
	rules := []string{"**SRP** [REQUIRED]: One job per type"}
	prompt := buildEvaluatorPrompt(rules, "main.go", "old code", "new code")

	if !containsAll(prompt, "main.go", "old code", "new code", "SRP", "REQUIRED") {
		t.Errorf("prompt missing expected content:\n%s", prompt)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
