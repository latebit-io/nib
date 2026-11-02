package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/kit/eventbus"
)

// Bus mechanics (ordering, policies, close, races, ctx fan-out) are
// covered by kit/eventbus contract tests. These tests cover what coding
// owns: its streaming classifier, the facade's sentinel mapping, the
// sendCritical deadline, and Agent-level fan-out.

func TestCodingIsStreamingEvent(t *testing.T) {
	tests := []struct {
		ev   event.Event
		want bool
	}{
		{event.AgentToken{}, true},
		{event.AgentStatus{}, true},
		{event.AgentTurnUsage{}, true},
		{event.AgentInputEstimate{}, true},
		{event.AgentCompacted{}, true},
		{event.AgentDone{}, false},
		{event.AgentError{}, false},
		{event.AgentWaiting{}, false},
		{event.AgentEditProposed{}, false},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("%T", tc.ev), func(t *testing.T) {
			if got := isStreamingEvent(tc.ev); got != tc.want {
				t.Errorf("isStreamingEvent = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCodingStatusDropsUnderDefaultPolicy verifies newBus injects the
// coding classifier: AgentStatus (streaming in coding, control in kit)
// drops on a full inbox instead of blocking the publisher.
func TestCodingStatusDropsUnderDefaultPolicy(t *testing.T) {
	a := &Agent{bus: newBus()}
	t.Cleanup(a.bus.Close)
	sub, err := a.Subscribe(SubscribeOptions{BufferSize: 1})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	a.send(event.AgentStatus{Status: event.StatusThinking}) // accepted
	a.send(event.AgentStatus{Status: event.StatusPlanning}) // dropped
	if got := sub.Drops(); got != 1 {
		t.Errorf("Drops on AgentStatus over-publish: got %d, want 1", got)
	}
}

func TestCodingSubscribeAfterCloseReturnsErrAgentClosed(t *testing.T) {
	a := &Agent{bus: newBus()}
	a.bus.Close()
	if _, err := a.Subscribe(SubscribeOptions{}); !errors.Is(err, ErrAgentClosed) {
		t.Errorf("Subscribe after close: got %v, want ErrAgentClosed", err)
	}
}

func TestCodingSubscribeRejectsInvalidPolicy(t *testing.T) {
	a := &Agent{bus: newBus()}
	t.Cleanup(a.bus.Close)
	if _, err := a.Subscribe(SubscribeOptions{OnControlFull: DropPolicy(99)}); !errors.Is(err, eventbus.ErrInvalidPolicy) {
		t.Errorf("Subscribe with invalid policy: got %v, want ErrInvalidPolicy", err)
	}
}

func TestCodingSubscriptionFacadeLifecycle(t *testing.T) {
	a := &Agent{bus: newBus()}
	t.Cleanup(a.bus.Close)
	keep, err := a.Subscribe(SubscribeOptions{})
	if err != nil {
		t.Fatalf("Subscribe keep: %v", err)
	}
	gone, err := a.Subscribe(SubscribeOptions{})
	if err != nil {
		t.Fatalf("Subscribe gone: %v", err)
	}
	if got := a.SubscriberCount(); got != 2 {
		t.Fatalf("SubscriberCount = %d, want 2", got)
	}

	gone.Close()
	gone.Close() // idempotent
	if _, open := <-gone.Events(); open {
		t.Error("closed subscription inbox still open")
	}
	if got := a.SubscriberCount(); got != 1 {
		t.Errorf("SubscriberCount after Close = %d, want 1", got)
	}

	a.send(event.AgentDone{Success: true})
	select {
	case ev := <-keep.Events():
		if ev != (event.AgentDone{Success: true}) {
			t.Errorf("keep got %#v, want AgentDone", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("remaining subscriber did not receive the event")
	}
}

func TestSendCriticalAfterCloseReturnsErrAgentClosed(t *testing.T) {
	a := &Agent{bus: newBus()}
	a.bus.Close()
	if err := a.sendCritical(context.Background(), event.AgentEditProposed{}); !errors.Is(err, ErrAgentClosed) {
		t.Errorf("sendCritical after close: got %v, want ErrAgentClosed", err)
	}
}

// TestSendCriticalHonorsDeadlineWithBlockedSubscriber is the P12
// regression: a Block-policy subscriber with a full inbox must not wedge
// sendCritical past the caller's deadline, or the orchestrator's
// edit-proposal wait deadlocks.
func TestSendCriticalHonorsDeadlineWithBlockedSubscriber(t *testing.T) {
	t.Parallel()

	a := &Agent{bus: newBus()}
	t.Cleanup(a.bus.Close)
	// Default options: control events Block on full. Fill the only slot.
	sub, err := a.Subscribe(SubscribeOptions{BufferSize: 1})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Close)
	a.send(event.AgentDone{Success: true})

	deadline := 150 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- a.sendCritical(ctx, event.AgentEditProposed{})
	}()

	select {
	case err := <-done:
		elapsed := time.Since(start)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("sendCritical returned %v; want context.DeadlineExceeded", err)
		}
		// Generous slack: -race scheduling jitter stretches elapsed.
		if elapsed > deadline+500*time.Millisecond {
			t.Errorf("sendCritical took %s; deadline was %s", elapsed, deadline)
		}
	case <-time.After(deadline + 2*time.Second):
		t.Fatal("sendCritical did not return; Block-policy subscriber wedged it past the deadline (P12 regression)")
	}
}

// TestSubscribeMultipleSubscribersObserveSameStream is the post-channel
// P10 verification at the coding layer: two independent Subscriptions
// against the same [Agent] each observe the same event-type sequence in
// the same per-subscriber order. Mirrors P9 at the kit layer
// (`kit/kit_test.go::TestSubscribeProbeObservesAlongsideConsumer`).
//
// Concrete use case: a developer attaches a probe Subscription to an
// already-running coding agent — for observability, metrics, or a
// secondary frontend — alongside the primary consumer Subscription that
// the flagship binary's bus→events forwarder reads. The probe must see
// the same event types in the same order as the primary consumer.
//
// Before commit 4 of the kit-event-bus arc this test compared the
// legacy [Agent.events] channel with a probe; commit 4 removed the
// channel and reshaped this into a two-Subscription comparison.
func TestSubscribeMultipleSubscribersObserveSameStream(t *testing.T) {
	t.Parallel()

	provider := &multiTurnProvider{
		turns: [][]llm.StreamEvent{
			// Single-turn run: stream a token then end the turn so the
			// foundation parks at AwaitReply and AgentWaiting fires.
			{
				{Token: "hello"},
				{Done: true, Usage: &llm.Usage{PromptTokens: 50, CompletionTokens: 5}},
			},
		},
	}

	ag := New(provider, stubWorkspace{}, nil)
	t.Cleanup(ag.Close)

	// Both subscribers attach BEFORE the run starts so they see every
	// event from the first publish onward. A generous buffer keeps the
	// test from depending on drop policy under burst — the assertion is
	// "both subscribers see the same stream," not "drop behavior."
	primary, err := ag.Subscribe(SubscribeOptions{BufferSize: 256})
	if err != nil {
		t.Fatalf("Subscribe primary: %v", err)
	}
	t.Cleanup(primary.Close)
	probe, err := ag.Subscribe(SubscribeOptions{BufferSize: 256})
	if err != nil {
		t.Fatalf("Subscribe probe: %v", err)
	}
	t.Cleanup(probe.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ag.RunWithMode(ctx, "main.go", "", "go", event.ModeExecution)

	// Drain both subscriptions in parallel until each has observed an
	// AgentWaiting (the run parks after one turn).
	primarySeq := drainCodingProbeUntilWaiting(t, primary.Events(), 3*time.Second)
	probeSeq := drainCodingProbeUntilWaiting(t, probe.Events(), 3*time.Second)

	ag.Cancel()

	got := eventTypeSeqCoding(primarySeq)
	want := eventTypeSeqCoding(probeSeq)

	if !slices.Equal(got, want) {
		t.Errorf("primary and probe sequences diverged\n  primary: %v\n    probe: %v", got, want)
	}
	if len(want) == 0 {
		t.Fatal("probe observed no events at all — Subscribe not wired into a.send")
	}
}

// drainCodingProbeUntilWaiting reads from a probe Subscription until
// AgentWaiting arrives or the deadline fires.
func drainCodingProbeUntilWaiting(t *testing.T, ch <-chan event.Event, timeout time.Duration) []event.Event {
	t.Helper()
	out := make([]event.Event, 0, 32)
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
			if _, ok := ev.(event.AgentWaiting); ok {
				return out
			}
		case <-deadline:
			t.Fatalf("probe drain: deadline before AgentWaiting; got %d events", len(out))
			return out
		}
	}
}

// eventTypeSeqCoding returns the type names of events in evs.
func eventTypeSeqCoding(evs []event.Event) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, fmt.Sprintf("%T", ev))
	}
	return out
}
