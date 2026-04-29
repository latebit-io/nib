package streaming

import (
	"strings"
	"testing"

	"github.com/latebit-io/junto/ai/llm"
	"github.com/latebit-io/junto/engine/event"
)

// TestMaybeCompact_BelowThresholdNoOp verifies that a small history
// is returned unchanged and no AgentCompacted event is emitted.
// Compaction is NOT a no-op math — it builds a new slice — so a
// premature trigger would be a wasted allocation per turn.
func TestMaybeCompact_BelowThresholdNoOp(t *testing.T) {
	t.Parallel()
	in := []llm.Message{
		{Role: "system", Content: "tiny"},
		{Role: "user", Content: "hello"},
	}
	var sent []event.Event
	send := func(ev event.Event) { sent = append(sent, ev) }

	out := MaybeCompact(in, nil, send)

	if len(out) != len(in) {
		t.Errorf("len(out) = %d, want %d (no-op below threshold)", len(out), len(in))
	}
	if len(sent) != 0 {
		t.Errorf("sent %d events, want 0 (no AgentCompacted below threshold)", len(sent))
	}
}

// TestMaybeCompact_AboveThresholdCompactsAndEmits builds a history
// large enough to cross [CompactHistoryThreshold] (via a heavy tool
// result that [llm.CompactMessages] will prune) and asserts that the
// returned slice is shorter in token estimate AND an AgentCompacted
// event fires with the before/after counts.
func TestMaybeCompact_AboveThresholdCompactsAndEmits(t *testing.T) {
	t.Parallel()
	heavy := strings.Repeat("garbage ", 20_000) // ~160 KB of repetitive text

	// Build a history with enough older turns that CompactKeepTurns
	// leaves the heavy tool results compactable.
	msgs := []llm.Message{
		{Role: "system", Content: "system prompt"},
		{Role: "user", Content: "first request"},
		{Role: "assistant", Content: "ok"},
		{Role: "tool", ToolCallID: "old-1", Content: heavy},
		{Role: "user", Content: "next"},
		{Role: "assistant", Content: "ok"},
		{Role: "tool", ToolCallID: "old-2", Content: heavy},
		{Role: "user", Content: "next"},
		{Role: "assistant", Content: "ok"},
		{Role: "tool", ToolCallID: "old-3", Content: heavy},
		{Role: "user", Content: "next"},
		{Role: "assistant", Content: "ok"},
		{Role: "tool", ToolCallID: "old-4", Content: heavy},
		{Role: "user", Content: "recent — must be kept verbatim"},
		{Role: "assistant", Content: "ok"},
	}

	before := llm.EstimateMessageTokens(msgs, nil)
	if before.History < CompactHistoryThreshold {
		t.Skipf("test fixture below threshold: history=%d threshold=%d — adjust heavy text",
			before.History, CompactHistoryThreshold)
	}

	var sent []event.Event
	send := func(ev event.Event) { sent = append(sent, ev) }

	out := MaybeCompact(msgs, nil, send)

	after := llm.EstimateMessageTokens(out, nil)
	if after.History >= before.History {
		t.Errorf("after.History (%d) >= before.History (%d) — compaction did not shrink",
			after.History, before.History)
	}
	if len(sent) != 1 {
		t.Fatalf("sent %d events, want 1 (AgentCompacted)", len(sent))
	}
	ev, ok := sent[0].(event.AgentCompacted)
	if !ok {
		t.Fatalf("sent event = %T, want AgentCompacted", sent[0])
	}
	if ev.BeforeTokens != before.History {
		t.Errorf("AgentCompacted.BeforeTokens = %d, want %d", ev.BeforeTokens, before.History)
	}
	if ev.AfterTokens != after.History {
		t.Errorf("AgentCompacted.AfterTokens = %d, want %d", ev.AfterTokens, after.History)
	}
}

// TestEstimateAndBroadcast_EmitsEventAndReturnsEstimate locks the
// pre-call status update: the frontend's input estimate must update
// BEFORE the LLM call starts, otherwise the status bar lags by one
// turn. Returns the same estimate so the caller can store it.
func TestEstimateAndBroadcast_EmitsEventAndReturnsEstimate(t *testing.T) {
	t.Parallel()
	msgs := []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hi"},
	}
	var sent []event.Event
	send := func(ev event.Event) { sent = append(sent, ev) }

	got := EstimateAndBroadcast(msgs, nil, send)

	if len(sent) != 1 {
		t.Fatalf("sent %d events, want 1 (AgentInputEstimate)", len(sent))
	}
	ev, ok := sent[0].(event.AgentInputEstimate)
	if !ok {
		t.Fatalf("sent event = %T, want AgentInputEstimate", sent[0])
	}
	if ev.System != got.System || ev.History != got.History || ev.New != got.New || ev.Tools != got.Tools {
		t.Errorf("event fields disagree with returned estimate: ev=%+v got=%+v", ev, got)
	}
}
