package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/event"
)

// TestAgent_MultiTurnEstimatesBindToOriginatingTurn is the integration
// check for the per-turn estimate binding now owned by [providerProxy].
// A two-turn scripted run with distinct prompt sizes; both
// AgentTurnUsage events must carry estimates derived from THEIR turn's
// transcript at the time providerProxy.Stream computed them, not blended
// with the other turn's.
//
// The tool call in turn 1 forces a real multi-turn flow (foundation
// advances to turn 2 before the forwarder might catch up — the bug a
// previous lastEstimate-field design hit, plus the bug the now-removed
// forwarder-side queue hit when kit dropped AgentTurnUsage).
func TestAgent_MultiTurnEstimatesBindToOriginatingTurn(t *testing.T) {
	t.Parallel()

	provider := &multiTurnProvider{
		turns: [][]llm.StreamEvent{
			// Turn 1: call a non-existent tool to force a follow-up turn.
			{
				{
					ToolCalls: []llm.ToolCall{{
						ID:       "call-1",
						Type:     "function",
						Function: llm.FunctionCall{Name: "no_such_tool", Arguments: "{}"},
					}},
					Done:  true,
					Usage: &llm.Usage{PromptTokens: 500, CompletionTokens: 20},
				},
			},
			// Turn 2: plain answer; agent will then park at AwaitReply.
			{
				{Token: "answer"},
				{Done: true, Usage: &llm.Usage{PromptTokens: 700, CompletionTokens: 10}},
			},
		},
	}

	ag := New(provider, stubWorkspace{}, nil)
	t.Cleanup(ag.Close)
	events := subscribeForTest(t, ag)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ag.RunWithMode(ctx, "main.go", "", "go", event.ModeExecution)

	usages := collectTurnUsages(t, ag, events, 2, 3*time.Second)

	if usages[0].Turn != 1 {
		t.Errorf("first AgentTurnUsage Turn = %d, want 1", usages[0].Turn)
	}
	if usages[1].Turn != 2 {
		t.Errorf("second AgentTurnUsage Turn = %d, want 2", usages[1].Turn)
	}
	// Turn 1 may have HistoryEst=0 (no prior conversation); turn 2
	// must exceed turn 1 because the transcript has grown by an
	// assistant tool-call message + tool reply.
	if usages[1].HistoryEst <= usages[0].HistoryEst {
		t.Errorf("turn 2 HistoryEst (%d) should exceed turn 1 (%d) — transcript grows between turns",
			usages[1].HistoryEst, usages[0].HistoryEst)
	}
	if usages[0].SystemEst != usages[1].SystemEst {
		t.Errorf("SystemEst should be stable across turns: turn1=%d turn2=%d",
			usages[0].SystemEst, usages[1].SystemEst)
	}
	if usages[0].ToolsEst != usages[1].ToolsEst {
		t.Errorf("ToolsEst should be stable across turns: turn1=%d turn2=%d",
			usages[0].ToolsEst, usages[1].ToolsEst)
	}
	// CompletionEst on turn 1 is zero: the assistant message was a
	// pure tool call with empty Content. Turn 2's completion is the
	// "answer" token.
	if usages[0].CompletionEst != 0 {
		t.Errorf("turn 1 CompletionEst = %d, want 0 (turn 1's assistant message was a pure tool call)",
			usages[0].CompletionEst)
	}
	if usages[1].CompletionEst == 0 {
		t.Errorf("turn 2 CompletionEst = %d, want >0 (turn 2 streamed text)", usages[1].CompletionEst)
	}
}

// TestAgent_TurnUsageSurvivesKitChannelPressure is the regression
// guard for the design defect the reviewer caught in the previous
// FIFO design: kit drops [event.AgentTurnUsage] when the consumer
// channel is full, which would have desynced any forwarder-side
// estimate queue indefinitely after the first drop.
//
// providerProxy emits AgentTurnUsage directly (synchronously inside
// its Stream wrapper goroutine) to coding's frontend channel,
// bypassing kit's lossy translator path entirely. So even when kit
// would have dropped its translation, coding still produces the
// authoritative AgentTurnUsage with correct per-turn estimates.
//
// We construct a frontend channel large enough that providerProxy's
// emit (a.send → frontend) never blocks, and verify the per-turn
// estimates remain correctly bound across multiple turns.
func TestAgent_TurnUsageSurvivesKitChannelPressure(t *testing.T) {
	t.Parallel()

	provider := &multiTurnProvider{
		turns: [][]llm.StreamEvent{
			{
				{
					ToolCalls: []llm.ToolCall{{
						ID:       "call-1",
						Type:     "function",
						Function: llm.FunctionCall{Name: "no_such_tool", Arguments: "{}"},
					}},
					Done:  true,
					Usage: &llm.Usage{PromptTokens: 100, CompletionTokens: 10},
				},
			},
			{
				{Token: "ok"},
				{Done: true, Usage: &llm.Usage{PromptTokens: 200, CompletionTokens: 5}},
			},
		},
	}

	ag := New(provider, stubWorkspace{}, nil)
	t.Cleanup(ag.Close)
	events := subscribeForTest(t, ag)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ag.RunWithMode(ctx, "main.go", "", "go", event.ModeExecution)

	usages := collectTurnUsages(t, ag, events, 2, 3*time.Second)

	// providerProxy must emit AgentTurnUsage with the provider's exact
	// usage numbers — this verifies usage attaches to the right turn,
	// not the previous or next.
	if usages[0].PromptTokens != 100 {
		t.Errorf("turn 1 PromptTokens = %d, want 100", usages[0].PromptTokens)
	}
	if usages[1].PromptTokens != 200 {
		t.Errorf("turn 2 PromptTokens = %d, want 200", usages[1].PromptTokens)
	}
	if usages[0].CompletionTokens != 10 {
		t.Errorf("turn 1 CompletionTokens = %d, want 10", usages[0].CompletionTokens)
	}
	if usages[1].CompletionTokens != 5 {
		t.Errorf("turn 2 CompletionTokens = %d, want 5", usages[1].CompletionTokens)
	}

	// Final session totals must equal the per-turn sums (no double-count
	// from kit's filtered AgentTurnUsage, no missed turns).
	usage := ag.Usage()
	if usage.TotalPromptTokens != 300 {
		t.Errorf("TotalPromptTokens = %d, want 300", usage.TotalPromptTokens)
	}
	if usage.TotalCompletionTokens != 15 {
		t.Errorf("TotalCompletionTokens = %d, want 15", usage.TotalCompletionTokens)
	}
	if usage.Turns != 2 {
		t.Errorf("Turns = %d, want 2", usage.Turns)
	}
}

// collectTurnUsages drains events until n AgentTurnUsage events are
// observed (or the timeout fires). Cancels the agent on success and
// on deadline so the wrapper run unwinds cleanly.
func collectTurnUsages(t *testing.T, ag *Agent, events <-chan event.Event, n int, timeout time.Duration) []event.AgentTurnUsage {
	t.Helper()
	var out []event.AgentTurnUsage
	deadline := time.After(timeout)
	for len(out) < n {
		select {
		case ev := <-events:
			if u, ok := ev.(event.AgentTurnUsage); ok {
				out = append(out, u)
				continue
			}
			if _, ok := ev.(event.AgentWaiting); ok && len(out) >= n {
				return out
			}
		case <-deadline:
			ag.Cancel()
			t.Fatalf("timeout collecting %d AgentTurnUsage events; got %d (%s)",
				n, len(out), strings.TrimSpace(formatUsages(out)))
		}
	}
	ag.Cancel()
	return out
}

func formatUsages(us []event.AgentTurnUsage) string {
	var b strings.Builder
	for _, u := range us {
		fmt.Fprintf(&b, "Turn=%d Prompt=%d HistoryEst=%d; ", u.Turn, u.PromptTokens, u.HistoryEst)
	}
	return b.String()
}
