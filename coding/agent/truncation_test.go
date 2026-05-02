package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/event"
)

func TestEscalateValue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		current int
		want    int
	}{
		{"unset starts at initial escalation", 0, truncationInitialEscalation},
		{"small value jumps to initial", 1024, truncationInitialEscalation},
		{"below-initial doubles below initial → floor to initial", 8000, truncationInitialEscalation},
		{"at initial doubles", truncationInitialEscalation, truncationInitialEscalation * 2},
		{"caps at ceiling", truncationCeiling / 2, truncationCeiling},
		{"beyond ceiling stays at ceiling", truncationCeiling + 10_000, truncationCeiling},
		{"at ceiling does not move", truncationCeiling, truncationCeiling},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := escalateValue(tc.current); got != tc.want {
				t.Errorf("escalateValue(%d) = %d, want %d", tc.current, got, tc.want)
			}
		})
	}
}

// stubEscalator is a minimal LLM provider that satisfies both
// llm.Provider (via a no-op Stream) and Escalator. The real provider
// types in ai/llm satisfy Escalator already; this stub keeps these
// tests free of HTTP machinery.
type stubEscalator struct {
	max int
}

func (s *stubEscalator) Stream(_ context.Context, _ []llm.Message, _ []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent)
	close(ch)
	return ch, nil
}
func (s *stubEscalator) MaxTokens() int     { return s.max }
func (s *stubEscalator) SetMaxTokens(v int) { s.max = v }

func TestEscalate_DoublesUnsetToInitial(t *testing.T) {
	t.Parallel()
	esc := &stubEscalator{max: 0}
	from, to, ok := escalate(esc)
	if !ok {
		t.Fatal("expected escalation to succeed")
	}
	if from != 0 {
		t.Errorf("from = %d, want 0", from)
	}
	if to != truncationInitialEscalation {
		t.Errorf("to = %d, want %d", to, truncationInitialEscalation)
	}
	if esc.max != truncationInitialEscalation {
		t.Errorf("provider max = %d, want %d", esc.max, truncationInitialEscalation)
	}
}

func TestEscalate_NoMoveAttruncationCeiling(t *testing.T) {
	t.Parallel()
	esc := &stubEscalator{max: truncationCeiling}
	from, to, ok := escalate(esc)
	if ok {
		t.Fatal("expected no escalation at ceiling")
	}
	if from != truncationCeiling || to != truncationCeiling {
		t.Errorf("from=%d to=%d, want both %d", from, to, truncationCeiling)
	}
	if esc.max != truncationCeiling {
		t.Errorf("provider max changed unexpectedly: %d", esc.max)
	}
}

func TestAppendRejections_OnePerToolCall(t *testing.T) {
	t.Parallel()
	calls := []llm.ToolCall{
		{ID: "a"}, {ID: "b"}, {ID: "c"},
	}
	got := appendRejections(nil, calls, "rej")
	if len(got) != 3 {
		t.Fatalf("got %d messages, want 3", len(got))
	}
	for i, m := range got {
		if m.Role != "tool" {
			t.Errorf("msg %d role = %q, want tool", i, m.Role)
		}
		if m.ToolCallID != calls[i].ID {
			t.Errorf("msg %d ToolCallID = %q, want %q", i, m.ToolCallID, calls[i].ID)
		}
		if m.Content != "rej" {
			t.Errorf("msg %d Content = %q, want %q", i, m.Content, "rej")
		}
	}
}

func TestAppendRejections_EmptyCallsIsNoop(t *testing.T) {
	t.Parallel()
	in := []llm.Message{{Role: "user", Content: "hi"}}
	got := appendRejections(in, nil, "rej")
	if len(got) != len(in) {
		t.Errorf("len(got) = %d, want %d (no-op when toolCalls empty)", len(got), len(in))
	}
}

func TestRecoveryMessages_AppliedPhrasing(t *testing.T) {
	t.Parallel()
	tool, user, ui := recoveryMessages(8192, 16384, escalationApplied)
	if !strings.Contains(tool, "8192") || !strings.Contains(tool, "16384") {
		t.Errorf("tool message missing from/to numbers: %q", tool)
	}
	if !strings.Contains(user, "raised") {
		t.Errorf("user message should mention the cap was raised: %q", user)
	}
	if !strings.Contains(ui, "bumping max_tokens") {
		t.Errorf("ui message should mention bumping: %q", ui)
	}
}

func TestRecoveryMessages_AttruncationCeilingPhrasing(t *testing.T) {
	t.Parallel()
	tool, user, ui := recoveryMessages(truncationCeiling, truncationCeiling, escalationAtCeiling)
	if !strings.Contains(tool, "ceiling") {
		t.Errorf("tool message should mention ceiling: %q", tool)
	}
	if !strings.Contains(user, "ceiling") {
		t.Errorf("user message should mention ceiling: %q", user)
	}
	if !strings.Contains(ui, "ceiling") {
		t.Errorf("ui message should mention ceiling: %q", ui)
	}
}

func TestRecoveryMessages_UnsupportedPhrasing(t *testing.T) {
	t.Parallel()
	// from/to stay zero when the provider doesn't implement Escalator.
	tool, user, ui := recoveryMessages(0, 0, escalationUnsupported)
	for _, msg := range []string{tool, user, ui} {
		if strings.Contains(msg, "ceiling") {
			t.Errorf("unsupported phrasing must not mention 'ceiling' "+
				"(would mislead the model about the cause): %q", msg)
		}
		if !strings.Contains(msg, "escalation") {
			t.Errorf("unsupported phrasing should mention escalation: %q", msg)
		}
	}
}

func TestRecover_UnderRetryCap_EscalatesAndContinues(t *testing.T) {
	t.Parallel()
	provider := &stubEscalator{max: 0}
	calls := []llm.ToolCall{{ID: "x"}}
	var sent []event.Event
	sender := func(ev event.Event) { sent = append(sent, ev) }

	msgs, retries, err := recoverFromTruncation(nil, calls, 0, provider, sender)
	if err != nil {
		t.Fatalf("Recover err = %v, want nil", err)
	}
	if retries != 1 {
		t.Errorf("retries = %d, want 1 (incremented)", retries)
	}
	if provider.max != truncationInitialEscalation {
		t.Errorf("provider not escalated: max=%d, want %d", provider.max, truncationInitialEscalation)
	}
	if len(msgs) != 1 || msgs[0].ToolCallID != "x" {
		t.Errorf("expected one tool-role rejection for call x, got %+v", msgs)
	}
	if len(sent) != 1 {
		t.Errorf("expected one AgentError event, got %d: %+v", len(sent), sent)
	}
}

func TestRecover_NoToolCalls_AppendsUserNudge(t *testing.T) {
	t.Parallel()
	provider := &stubEscalator{max: truncationInitialEscalation}
	msgs, _, err := recoverFromTruncation(nil, nil, 0, provider, nil)
	if err != nil {
		t.Fatalf("Recover err = %v, want nil", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1 (the user nudge)", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Errorf("expected user-role nudge, got role=%q", msgs[0].Role)
	}
}

func TestRecover_AtRetryCap_ReturnsTerminalError(t *testing.T) {
	t.Parallel()
	provider := &stubEscalator{max: truncationInitialEscalation}
	calls := []llm.ToolCall{{ID: "x"}, {ID: "y"}}
	var sent []event.Event
	sender := func(ev event.Event) { sent = append(sent, ev) }

	msgs, retries, err := recoverFromTruncation(nil, calls, truncationMaxRetries, provider, sender)
	if err == nil {
		t.Fatal("Recover err = nil, want terminal error at retry cap")
	}
	if !strings.Contains(err.Error(), "abandoning turn") {
		t.Errorf("err = %q, want substring 'abandoning turn'", err)
	}
	if retries != truncationMaxRetries {
		t.Errorf("retries = %d, want %d (NOT incremented on abort)", retries, truncationMaxRetries)
	}
	if len(msgs) != len(calls) {
		t.Errorf("got %d rejections, want %d (one per pending call)", len(msgs), len(calls))
	}
	if len(sent) != 1 {
		t.Errorf("expected one AgentError event on abort, got %d", len(sent))
	}
	// Provider should NOT have been escalated on abort — we're abandoning.
	if provider.max != truncationInitialEscalation {
		t.Errorf("provider escalated during abort: max=%d", provider.max)
	}
}

func TestRecover_NonEscalatorProvider_NoOpsEscalation(t *testing.T) {
	t.Parallel()
	// noEscProvider satisfies llm.Provider but NOT Escalator.
	provider := noEscProvider{}
	calls := []llm.ToolCall{{ID: "x"}}
	msgs, retries, err := recoverFromTruncation(nil, calls, 0, provider, nil)
	if err != nil {
		t.Fatalf("Recover err = %v, want nil", err)
	}
	if retries != 1 {
		t.Errorf("retries = %d, want 1", retries)
	}
	// Tool message must use the unsupported phrasing — NOT the
	// ceiling phrasing — because the cause is provider capability,
	// not an exhausted cap. Conflating the two was the pre-fix bug.
	if strings.Contains(msgs[0].Content, "ceiling") {
		t.Errorf("non-Escalator provider must not surface 'ceiling' phrasing: %q", msgs[0].Content)
	}
	if !strings.Contains(msgs[0].Content, "escalation") {
		t.Errorf("non-Escalator provider should mention escalation in its message: %q", msgs[0].Content)
	}
}

func TestRecover_NilSender_DoesNotPanic(t *testing.T) {
	t.Parallel()
	provider := &stubEscalator{max: 0}
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Recover panicked with nil sender: %v", r)
		}
	}()
	if _, _, err := recoverFromTruncation(nil, nil, 0, provider, nil); err != nil {
		t.Errorf("under-cap path err = %v, want nil", err)
	}
	if _, _, err := recoverFromTruncation(nil, nil, truncationMaxRetries, provider, nil); err == nil {
		t.Error("at-cap path err = nil, want non-nil terminal error")
	}
}

// noEscProvider implements llm.Provider without satisfying Escalator,
// so Recover takes the "no escalation possible" branch.
type noEscProvider struct{}

func (noEscProvider) Stream(_ context.Context, _ []llm.Message, _ []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent)
	close(ch)
	return ch, nil
}
