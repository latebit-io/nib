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
// translator to exit before coding.Close calls bus.close, otherwise
// the translator's drain loop races the close and panics on
// send-on-closed-channel. The failure mode of a missing wait is an
// immediate runtime panic that fails the test outright.
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

	ag := New(provider, stubWorkspace{}, nil)

	// Drain the subscription channel in the background so the forwarder
	// can flush — Close blocks if the consumer stops draining (hard
	// contract documented on Close).
	sub, err := ag.Subscribe(SubscribeOptions{BufferSize: 256})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	drainerDone := make(chan struct{})
	go func() {
		defer close(drainerDone)
		for range sub.Events() {
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ag.RunWithMode(ctx, "main.go", "", "go", event.ModeExecution)

	// Don't wait for AgentWaiting — Close mid-run is the harsher test.
	// kit.Close aborts, foundation emits AgentEnd, kit translator
	// drains, coding.Close closes the bus, forwarder + subscriber
	// inboxes drain, goroutines exit. If any link in that chain races
	// a closed channel, Go panics.
	ag.Close()

	<-drainerDone
}

// TestClose_BlocksUntilForwarderExits is the symmetric guarantee.
// After Close returns, the forwarder goroutine must be gone and the
// bus must be shut, so subscribers' inboxes close and reader loops
// exit. Asserting on the forwardDone channel (closed by the
// forwarder's deferred close) is parallel-safe; runtime.NumGoroutine
// is unreliable under -parallel because other tests' goroutines
// pollute the count.
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

	ag := New(provider, stubWorkspace{}, nil)

	sub, err := ag.Subscribe(SubscribeOptions{BufferSize: 64})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	drainerDone := make(chan struct{})
	go func() {
		defer close(drainerDone)
		for range sub.Events() {
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ag.RunWithMode(ctx, "main.go", "", "go", event.ModeExecution)
	ag.Close()

	// forwardDone is closed by [Agent.forwardKitEvents] on return;
	// Close blocks on it. A non-blocking receive after Close MUST
	// observe the closed channel — otherwise Close returned before
	// the forwarder finished, breaking the contract that callers
	// rely on to release subscriber resources safely.
	select {
	case _, ok := <-ag.forwardDone:
		if ok {
			t.Error("forwardDone was not closed after Close returned")
		}
	default:
		t.Error("Close returned but forwardDone is not closed — forwarder still running")
	}

	<-drainerDone
}

// TestClose_Idempotent verifies repeated Close calls are safe.
// closeOnce guards both the kit close-once and the bus close;
// without that guard the second bus.close would panic on a
// previously-closed inbox.
func TestClose_Idempotent(t *testing.T) {
	t.Parallel()

	ag := New(&multiTurnProvider{}, stubWorkspace{}, nil)

	sub, err := ag.Subscribe(SubscribeOptions{BufferSize: 16})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	drainerDone := make(chan struct{})
	go func() {
		defer close(drainerDone)
		for range sub.Events() {
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

	<-drainerDone
}
