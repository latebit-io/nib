package agent

import (
	"strings"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/event"
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

	out := maybeCompact(in, nil, send, nil)

	if len(out) != len(in) {
		t.Errorf("len(out) = %d, want %d (no-op below threshold)", len(out), len(in))
	}
	if len(sent) != 0 {
		t.Errorf("sent %d events, want 0 (no AgentCompacted below threshold)", len(sent))
	}
}

// TestMaybeCompact_AboveThresholdCompactsAndEmits builds a history
// large enough to cross [compactHistoryThreshold] (via a heavy tool
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
	if before.History < compactHistoryThreshold {
		t.Skipf("test fixture below threshold: history=%d threshold=%d — adjust heavy text",
			before.History, compactHistoryThreshold)
	}

	var sent []event.Event
	send := func(ev event.Event) { sent = append(sent, ev) }

	out := maybeCompact(msgs, nil, send, nil)

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

// TestMaybeCompact_BeforeCompactCallback verifies the PreCompact emit
// hook fires exactly when compaction runs — once above threshold, never
// below — so PreCompact does not fire on every quiet turn.
func TestMaybeCompact_BeforeCompactCallback(t *testing.T) {
	t.Parallel()
	noopSend := func(event.Event) {}

	// Below threshold: the callback must NOT fire.
	small := []llm.Message{{Role: "system", Content: "tiny"}, {Role: "user", Content: "hi"}}
	calls := 0
	maybeCompact(small, nil, noopSend, func() { calls++ })
	if calls != 0 {
		t.Fatalf("beforeCompact fired %d times below threshold, want 0", calls)
	}

	// Above threshold: the callback fires once.
	heavy := strings.Repeat("garbage ", 20_000)
	msgs := []llm.Message{
		{Role: "system", Content: "system prompt"},
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "ok"},
		{Role: "tool", ToolCallID: "old-1", Content: heavy},
		{Role: "user", Content: "next"},
		{Role: "assistant", Content: "ok"},
		{Role: "tool", ToolCallID: "old-2", Content: heavy},
		{Role: "user", Content: "recent"},
		{Role: "assistant", Content: "ok"},
	}
	if est := llm.EstimateMessageTokens(msgs, nil); est.History < compactHistoryThreshold {
		t.Skipf("fixture below threshold: history=%d threshold=%d", est.History, compactHistoryThreshold)
	}
	calls = 0
	maybeCompact(msgs, nil, noopSend, func() { calls++ })
	if calls != 1 {
		t.Fatalf("beforeCompact fired %d times above threshold, want 1", calls)
	}
}

// AgentInputEstimate emission is now exercised end-to-end in
// TestAgent_TurnUsageSurvivesKitChannelPressure (forwarder_test.go) —
// providerProxy.Stream emits it synchronously before each LLM call.
// The standalone unit test for estimateAndBroadcast was retired
// alongside the helper.
