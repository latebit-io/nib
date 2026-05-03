package agent

import (
	"context"
	"testing"
	"time"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/event"
)

// TestClose_NoSendOnClosedChannelPanic locks the contract the
// reviewer flagged: kit.Close must synchronously wait for kit's
// translator to exit before coding.Close calls close(a.kitEvents),
// otherwise the translator's drain loop races the close and panics
// on send-on-closed-channel. The failure mode of a missing wait is
// an immediate runtime panic that fails the test outright.
//
// We script a multi-event provider so kit's translator has buffered
// foundation events to drain at Close time, and we deliberately let
// some of those events sit in the buffer so the race window is open
// when we Close.
func TestClose_NoSendOnClosedChannelPanic(t *testing.T) {
	t.Parallel()

	// Provider produces a turn with content tokens + usage. Foundation
	// emits MessageUpdate × N + TurnUsage + (eventually) AgentEnd
	// during the abort triggered by Close. Multiple events maximize
	// the chance that some are still buffered when Close fires.
	provider := &multiTurnProvider{
		turns: [][]llm.StreamEvent{
			{
				{Token: "a"},
				{Token: "b"},
				{Token: "c"},
				{Token: "d"},
				{Token: "e"},
				{Done: true, Usage: &llm.Usage{PromptTokens: 100, CompletionTokens: 5}},
			},
		},
	}

	events := make(chan event.Event, 256)
	ag := New(provider, stubWorkspace{}, events, nil)

	// Drain the frontend channel in the background so the forwarder
	// can flush — Close blocks if the consumer stops draining (hard
	// contract documented on Close).
	drainerDone := make(chan struct{})
	go func() {
		defer close(drainerDone)
		for ev := range events {
			if fb, ok := ev.(event.FlushBuffers); ok {
				fb.Result <- event.FlushResult{}
			}
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ag.RunWithMode(ctx, "main.go", "", "go", nil, event.ModeExecution)

	// Don't wait for AgentWaiting — Close mid-run is the harsher test.
	// kit.Close aborts, foundation emits AgentEnd, kit translator
	// drains, coding.Close closes kitEvents, forwarder drains, exits.
	// If any link in that chain races a closed channel, Go panics.
	ag.Close()

	close(events)
	<-drainerDone
}

// TestClose_BlocksUntilForwarderExits is the symmetric guarantee.
// After Close returns, the forwarder goroutine must be gone — the
// caller is then safe to close the frontend events channel without
// racing a final write. Asserting on the forwardDone channel
// (closed by the forwarder's deferred close) is parallel-safe;
// runtime.NumGoroutine is unreliable under -parallel because other
// tests' goroutines pollute the count.
func TestClose_BlocksUntilForwarderExits(t *testing.T) {
	t.Parallel()

	provider := &multiTurnProvider{
		turns: [][]llm.StreamEvent{
			{
				{Token: "x"},
				{Done: true, Usage: &llm.Usage{PromptTokens: 50, CompletionTokens: 1}},
			},
		},
	}

	events := make(chan event.Event, 64)
	ag := New(provider, stubWorkspace{}, events, nil)

	drainerDone := make(chan struct{})
	go func() {
		defer close(drainerDone)
		for ev := range events {
			if fb, ok := ev.(event.FlushBuffers); ok {
				fb.Result <- event.FlushResult{}
			}
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ag.RunWithMode(ctx, "main.go", "", "go", nil, event.ModeExecution)
	ag.Close()

	// forwardDone is closed by [Agent.forwardKitEvents] on return;
	// Close blocks on it. A non-blocking receive after Close MUST
	// observe the closed channel — otherwise Close returned before
	// the forwarder finished, breaking the contract that callers
	// rely on to close the frontend events channel safely.
	select {
	case _, ok := <-ag.forwardDone:
		if ok {
			t.Error("forwardDone was not closed after Close returned")
		}
	default:
		t.Error("Close returned but forwardDone is not closed — forwarder still running")
	}

	close(events)
	<-drainerDone
}

// TestClose_Idempotent verifies repeated Close calls are safe.
// closeOnce guards both the kit close-once and the channel close;
// without that guard the second close(kitEvents) would panic.
func TestClose_Idempotent(t *testing.T) {
	t.Parallel()

	events := make(chan event.Event, 16)
	ag := New(&multiTurnProvider{}, stubWorkspace{}, events, nil)

	drainerDone := make(chan struct{})
	go func() {
		defer close(drainerDone)
		for range events {
		}
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		ag.Close()
		ag.Close()
		ag.Close()
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("repeated Close calls did not return within 2s")
	}

	close(events)
	<-drainerDone
}
