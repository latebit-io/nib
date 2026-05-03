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

// TestNthAssistantMessage covers the helper in isolation: it must
// 1-index, walk past non-assistant messages, and return nil on
// out-of-range — including n <= 0 (defensive for a future caller
// that forgets the 1-indexed contract).
func TestNthAssistantMessage(t *testing.T) {
	t.Parallel()

	msgs := []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "u1"},
		{Role: "assistant", Content: "a1"},
		{Role: "tool", Content: "t1"},
		{Role: "assistant", Content: "a2"},
		{Role: "user", Content: "u2"},
		{Role: "assistant", Content: "a3"},
	}

	cases := []struct {
		n    int
		want string
	}{
		{1, "a1"},
		{2, "a2"},
		{3, "a3"},
	}
	for _, tc := range cases {
		got := nthAssistantMessage(msgs, tc.n)
		if got == nil {
			t.Errorf("n=%d: got nil, want %q", tc.n, tc.want)
			continue
		}
		if got.Content != tc.want {
			t.Errorf("n=%d: got %q, want %q", tc.n, got.Content, tc.want)
		}
	}

	if got := nthAssistantMessage(msgs, 4); got != nil {
		t.Errorf("n=4 (out of range) = %+v, want nil", got)
	}
	if got := nthAssistantMessage(msgs, 0); got != nil {
		t.Errorf("n=0 (defensive) = %+v, want nil", got)
	}
	if got := nthAssistantMessage(nil, 1); got != nil {
		t.Errorf("nil msgs = %+v, want nil", got)
	}
}

// TestAugmentAndAccumulate_PopsEstimateQueueInFIFOOrder is the unit-
// level guard that prevents the bug the reviewer flagged. Three
// estimates are queued (simulating three TransformContext calls), then
// three AgentTurnUsage events are fed through augmentAndAccumulate
// (simulating the forwarder draining the kit channel after the
// foundation has advanced multiple turns ahead). Each augmented event
// must carry the estimate from the matching turn — not the latest one.
//
// Without the FIFO queue, all three augmented events would be stamped
// with the queue's tail (the last estimate set), losing per-turn
// fidelity for the cost panel.
func TestAugmentAndAccumulate_PopsEstimateQueueInFIFOOrder(t *testing.T) {
	t.Parallel()

	a := &Agent{
		estimateQueue: []llm.InputEstimate{
			{System: 100, Tools: 10, History: 1000, New: 50},
			{System: 100, Tools: 10, History: 1500, New: 30},
			{System: 100, Tools: 10, History: 2000, New: 20},
		},
	}

	usages := []event.AgentTurnUsage{
		{PromptTokens: 1100, CompletionTokens: 50},
		{PromptTokens: 1600, CompletionTokens: 40},
		{PromptTokens: 2100, CompletionTokens: 30},
	}

	wantHistory := []int{1000, 1500, 2000}
	wantNew := []int{50, 30, 20}

	for i, u := range usages {
		got := a.augmentAndAccumulate(u)
		if got.Turn != i+1 {
			t.Errorf("turn %d: got Turn=%d, want %d", i+1, got.Turn, i+1)
		}
		if got.HistoryEst != wantHistory[i] {
			t.Errorf("turn %d: HistoryEst = %d, want %d (queue out of order or stale)",
				i+1, got.HistoryEst, wantHistory[i])
		}
		if got.NewEst != wantNew[i] {
			t.Errorf("turn %d: NewEst = %d, want %d (queue out of order or stale)",
				i+1, got.NewEst, wantNew[i])
		}
		if got.SystemEst != 100 || got.ToolsEst != 10 {
			t.Errorf("turn %d: SystemEst/ToolsEst lost during augmentation: %+v", i+1, got)
		}
	}

	// After three pops the queue must be empty.
	if len(a.estimateQueue) != 0 {
		t.Errorf("estimateQueue length after three pops = %d, want 0", len(a.estimateQueue))
	}

	// turnCounter advances 1:1 with augmentAndAccumulate calls.
	// sessionUsage accumulation is providerProxy's job (synchronous on
	// each Stream's Done event), not augmentAndAccumulate's — covered
	// in TestProviderProxy_Stream_AccumulatesUsageOnDone.
	if a.turnCounter != 3 {
		t.Errorf("turnCounter = %d, want 3", a.turnCounter)
	}
}

// TestAugmentAndAccumulate_EmptyQueueZeroEstimate guards the
// defensive path: if the forwarder receives an AgentTurnUsage with
// no matching estimate queued (e.g. foundation Stream-error path
// without TransformContext, or a future hook ordering change), the
// augmented event's *Est fields fall back to zero rather than
// panicking on slice out-of-range.
func TestAugmentAndAccumulate_EmptyQueueZeroEstimate(t *testing.T) {
	t.Parallel()

	a := &Agent{} // no queued estimates

	got := a.augmentAndAccumulate(event.AgentTurnUsage{
		PromptTokens:     100,
		CompletionTokens: 50,
	})

	if got.SystemEst != 0 || got.ToolsEst != 0 || got.HistoryEst != 0 || got.NewEst != 0 {
		t.Errorf("empty queue: *Est fields not zeroed: %+v", got)
	}
	if got.Turn != 1 {
		t.Errorf("Turn = %d, want 1", got.Turn)
	}
}

// TestAgent_MultiTurnEstimatesBindToOriginatingTurn is the
// integration check for the per-turn estimate binding. A two-turn
// scripted run with distinct prompt sizes; both AgentTurnUsage events
// must carry estimates derived from THEIR turn's transcript at the
// time TransformContext fired, not blended with the other turn's.
//
// The tool call in turn 1 forces a real multi-turn flow (foundation
// advances to turn 2 before the forwarder might catch up).
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

	events := make(chan event.Event, 256)
	ag := New(provider, stubWorkspace{}, events, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ag.RunWithMode(ctx, "main.go", "", "go", nil, event.ModeExecution)

	usages := collectTurnUsages(t, ag, events, 2, 3*time.Second)

	// Turn 1: HistoryEst should be small (initial transcript). Turn 2:
	// HistoryEst should have grown (turn 1's assistant + tool reply
	// added). The assertion is direction, not exact bytes — the prompt
	// template details are not the test's concern.
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
	// SystemEst + ToolsEst are turn-independent — both turns should
	// see the same values (modulo prompt template). Asserting equality
	// confirms the per-turn binding does not blend stale estimates.
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
	// "answer" token. Both must be non-blended (turn 1 must NOT carry
	// turn 2's completion).
	if usages[0].CompletionEst != 0 {
		t.Errorf("turn 1 CompletionEst = %d, want 0 (turn 1's assistant message was a pure tool call)",
			usages[0].CompletionEst)
	}
	if usages[1].CompletionEst == 0 {
		t.Errorf("turn 2 CompletionEst = %d, want >0 (turn 2 streamed text)", usages[1].CompletionEst)
	}
}

// collectTurnUsages drains events until n AgentTurnUsage events are
// observed (or the timeout fires). Cancels the agent on success and
// on deadline so the wrapper run unwinds cleanly.
func collectTurnUsages(t *testing.T, ag *Agent, events chan event.Event, n int, timeout time.Duration) []event.AgentTurnUsage {
	t.Helper()
	var out []event.AgentTurnUsage
	deadline := time.After(timeout)
	for len(out) < n {
		select {
		case ev := <-events:
			if fb, ok := ev.(event.FlushBuffers); ok {
				fb.Result <- event.FlushResult{}
				continue
			}
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
		fmt.Fprintf(&b, "Turn=%d HistoryEst=%d; ", u.Turn, u.HistoryEst)
	}
	return b.String()
}
