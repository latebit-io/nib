package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/event"
)

// drainBus subscribes to a bus-backed agent's stream and returns every
// event published while running fn. Closes the subscription on return.
// Tests use it to assert exactly which compaction events fired.
func drainBus(t *testing.T, a *Agent, fn func()) []event.Event {
	t.Helper()
	sub, err := a.Subscribe(SubscribeOptions{})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Close()
	fn()
	// Bus delivery is per-subscriber serialized: by the time fn returns,
	// every Send invocation has completed deliverWG.Done, so events are
	// observable on the channel without further synchronization.
	var out []event.Event
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
		default:
			return out
		}
	}
}

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
	a := &Agent{bus: newBus()}
	var out []llm.Message
	sent := drainBus(t, a, func() {
		out = a.maybeCompact(context.Background(), in, nil)
	})

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
// event fires with the before/after counts. Tier-2 stays out of the
// way because Tier-1 already brings history below the summarization
// threshold (heavy results stub down to a short marker).
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

	a := &Agent{bus: newBus()}
	var out []llm.Message
	sent := drainBus(t, a, func() {
		out = a.maybeCompact(context.Background(), msgs, nil)
	})

	after := llm.EstimateMessageTokens(out, nil)
	if after.History >= before.History {
		t.Errorf("after.History (%d) >= before.History (%d) — compaction did not shrink",
			after.History, before.History)
	}
	if len(sent) != 1 {
		t.Fatalf("sent %d events, want 1 (AgentCompacted only); got %d", len(sent), len(sent))
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

// AgentInputEstimate emission is now exercised end-to-end in
// TestAgent_TurnUsageSurvivesKitChannelPressure (forwarder_test.go) —
// providerProxy.Stream emits it synchronously before each LLM call.
// The standalone unit test for estimateAndBroadcast was retired
// alongside the helper.
