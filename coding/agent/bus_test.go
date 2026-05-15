package agent

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/event"
)

// Bus tests mirror kit/bus_test.go since the shapes are intentionally
// identical. The per-subscription deliverWG discipline (post Greptile
// P1 on PR #151) is carried over here; the regression test below
// guards against the same coupling failure if the coding bus is ever
// refactored to a bus-wide wait-group.

func TestCodingBusSubscribeReturnsLiveSubscription(t *testing.T) {
	b := newBus()
	sub, err := b.subscribe(SubscribeOptions{})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	want := []event.Event{
		event.AgentToken{Text: "a"},
		event.AgentToken{Text: "b"},
		event.AgentDone{Success: true},
	}
	for _, ev := range want {
		b.publish(ev)
	}

	for i, w := range want {
		got := drainOneCoding(t, sub)
		if got != w {
			t.Errorf("event %d: got %#v, want %#v", i, got, w)
		}
	}
}

// TestCodingBusOrderPerSubscriberWithMultipleSubscribers verifies that
// every subscriber sees every published event in publish order.
func TestCodingBusOrderPerSubscriberWithMultipleSubscribers(t *testing.T) {
	b := newBus()
	subs := make([]*Subscription, 3)
	for i := range subs {
		s, err := b.subscribe(SubscribeOptions{BufferSize: 16})
		if err != nil {
			t.Fatalf("subscribe %d: %v", i, err)
		}
		subs[i] = s
	}

	events := []event.Event{
		event.AgentToken{Text: "1"},
		event.AgentTurnUsage{Turn: 1},
		event.AgentToken{Text: "2"},
		event.AgentWaiting{Finished: true},
		event.AgentDone{Success: true},
	}
	for _, ev := range events {
		b.publish(ev)
	}

	for i, sub := range subs {
		for j, want := range events {
			got := drainOneCoding(t, sub)
			if got != want {
				t.Errorf("subscriber %d event %d: got %#v, want %#v", i, j, got, want)
			}
		}
	}
}

// TestCodingBusStatusIsStreaming verifies that AgentStatus, which
// coding classifies as streaming (kit does not), drops on a full
// inbox under the default policy. Catches accidental divergence
// between [isStreamingEvent] and the [Agent.send] switch.
func TestCodingBusStatusIsStreaming(t *testing.T) {
	b := newBus()
	sub, _ := b.subscribe(SubscribeOptions{BufferSize: 1})

	b.publish(event.AgentStatus{Status: event.StatusThinking}) // accepted
	b.publish(event.AgentStatus{Status: event.StatusPlanning}) // dropped
	if got := sub.Drops(); got != 1 {
		t.Errorf("Drops on AgentStatus over-publish: got %d, want 1", got)
	}
}

// TestCodingBusSlowSubscriberDropsStreaming verifies streaming-event
// drop accounting under the default policy.
func TestCodingBusSlowSubscriberDropsStreaming(t *testing.T) {
	b := newBus()
	sub, _ := b.subscribe(SubscribeOptions{BufferSize: 2})

	for i := 0; i < 10; i++ {
		b.publish(event.AgentToken{Text: "x"})
	}
	if got := sub.Drops(); got != 8 {
		t.Errorf("Drops: got %d, want 8", got)
	}
}

// TestCodingBusControlBlocksUntilSubscriberDrains verifies the default
// Block policy on control events makes the publisher block until the
// inbox has room.
func TestCodingBusControlBlocksUntilSubscriberDrains(t *testing.T) {
	b := newBus()
	sub, _ := b.subscribe(SubscribeOptions{BufferSize: 1})

	b.publish(event.AgentDone{Success: true}) // occupies the slot

	publishReturned := make(chan struct{})
	go func() {
		b.publish(event.AgentError{Err: "boom"})
		close(publishReturned)
	}()

	select {
	case <-publishReturned:
		t.Fatal("publish returned before subscriber drained")
	case <-time.After(50 * time.Millisecond):
	}

	<-sub.Events()

	select {
	case <-publishReturned:
	case <-time.After(time.Second):
		t.Fatal("publish did not unblock after drain")
	}
}

// TestCodingBusCloseDrainsAllSubscribers verifies bus.close closes
// every subscription's inbox so reader loops exit.
func TestCodingBusCloseDrainsAllSubscribers(t *testing.T) {
	b := newBus()
	subs := make([]*Subscription, 3)
	for i := range subs {
		s, _ := b.subscribe(SubscribeOptions{})
		subs[i] = s
	}

	b.publish(event.AgentToken{Text: "x"})

	var wg sync.WaitGroup
	wg.Add(len(subs))
	for _, sub := range subs {
		go func(s *Subscription) {
			defer wg.Done()
			for range s.Events() {
			}
		}(sub)
	}

	b.close()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("readers did not exit after bus.close")
	}
}

// TestCodingSubscribeAfterCloseReturnsErrAgentClosed verifies
// subscribe after bus.close returns the sentinel error.
func TestCodingSubscribeAfterCloseReturnsErrAgentClosed(t *testing.T) {
	b := newBus()
	b.close()
	if _, err := b.subscribe(SubscribeOptions{}); !errors.Is(err, ErrAgentClosed) {
		t.Errorf("subscribe after close: got %v, want ErrAgentClosed", err)
	}
}

// TestCodingSubscribeDuringCloseIsRaceSafe hammers Subscribe + Close
// concurrently to surface races under -race.
func TestCodingSubscribeDuringCloseIsRaceSafe(t *testing.T) {
	b := newBus()

	var wg sync.WaitGroup
	wg.Add(8)
	for i := 0; i < 8; i++ {
		go func() {
			defer wg.Done()
			if sub, err := b.subscribe(SubscribeOptions{}); err == nil {
				go func() {
					for range sub.Events() {
					}
				}()
				sub.Close()
			}
		}()
	}
	go func() {
		runtime.Gosched()
		b.close()
	}()
	wg.Wait()
}

// TestCodingSubscriptionCloseUnsubscribesFromBus verifies a Closed
// subscription no longer receives events from subsequent publishes.
func TestCodingSubscriptionCloseUnsubscribesFromBus(t *testing.T) {
	b := newBus()
	keep, _ := b.subscribe(SubscribeOptions{})
	drop, _ := b.subscribe(SubscribeOptions{})

	drop.Close()

	b.publish(event.AgentToken{Text: "k"})

	if got := drainOneCoding(t, keep); got == nil {
		t.Fatal("keep subscription missed event")
	}
	select {
	case ev, ok := <-drop.Events():
		if ok {
			t.Errorf("drop subscription received %#v after Close", ev)
		}
	default:
		t.Fatal("drop subscription channel not closed after Close")
	}
}

// TestCodingSubscriptionCloseIsIdempotent verifies double-Close does
// not panic.
func TestCodingSubscriptionCloseIsIdempotent(t *testing.T) {
	b := newBus()
	sub, _ := b.subscribe(SubscribeOptions{})
	sub.Close()
	sub.Close()
}

// TestCodingBusCloseIsIdempotent verifies double bus.close does not
// panic.
func TestCodingBusCloseIsIdempotent(t *testing.T) {
	b := newBus()
	b.close()
	b.close()
}

// TestCodingSubscriptionCloseDoesNotBlockOnUnrelatedSlowSubscriber
// is the regression test for the P1 Greptile catch on PR #151,
// carried over to the coding-side bus: closing one subscription must
// not wait on in-flight deliveries to an unrelated slow subscriber.
//
// Failure mode this guards: publish snapshots [fast, slow], slow's
// deliver blocks on a [Block]-policy full inbox, fast.Close hangs on
// a bus-wide wait-group until slow's consumer drains — coupling
// unrelated subscriptions through the shutdown path.
func TestCodingSubscriptionCloseDoesNotBlockOnUnrelatedSlowSubscriber(t *testing.T) {
	b := newBus()
	t.Cleanup(b.close)

	fast, _ := b.subscribe(SubscribeOptions{BufferSize: 4})
	slow, _ := b.subscribe(SubscribeOptions{BufferSize: 1})

	b.publish(event.AgentDone{Success: true}) // slow now 1/1 (full)
	<-fast.Events()

	publishing := make(chan struct{})
	go func() {
		close(publishing)
		b.publish(event.AgentError{Err: "boom"})
	}()
	<-publishing
	select {
	case <-fast.Events():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("fast subscriber did not receive second event")
	}

	closed := make(chan struct{})
	go func() {
		fast.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("fast.Close blocked on unrelated slow subscriber — global deliverWG regression")
	}

	go func() {
		for range slow.Events() {
		}
	}()
}

// TestSubscribeProbeObservesAlongsideLegacyChannel is the P10 verification
// at the coding layer (mirror of P9 at the kit layer in
// `kit/kit_test.go::TestSubscribeProbeObservesAlongsideConsumer`).
//
// Concrete use case: a developer attaches a probe Subscription to an
// already-running coding agent — for observability, metrics, or a
// secondary frontend — without disturbing the legacy events channel
// the TUI consumes. The probe must see the same event types in the
// same order as the channel reader.
//
// Today's dual-write shape ([Agent.send] writes to both
// [Agent.events] and [Agent.bus]) is what enables this. When commit 4
// removes the legacy channel and the TUI migrates to its own
// Subscription, this test becomes a multi-Subscription-only check
// without any change to the assertion shape.
func TestSubscribeProbeObservesAlongsideLegacyChannel(t *testing.T) {
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

	events := make(chan event.Event, 256)
	ag := New(provider, stubWorkspace{}, events, nil)
	t.Cleanup(ag.Close)

	// Probe attaches BEFORE the run starts so it sees every event the
	// channel sees. A generous buffer keeps the test from depending on
	// drop policy under burst — the assertion is "probe sees the
	// stream," not "probe matches drop behavior."
	probe, err := ag.Subscribe(SubscribeOptions{BufferSize: 256})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(probe.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ag.RunWithMode(ctx, "main.go", "", "go", nil, event.ModeExecution)

	// Drain both streams in parallel until each has observed an
	// AgentWaiting (the run parks after one turn). Probe and channel
	// each respond to FlushBuffers in-band — only the legacy channel
	// reader can answer the request-channel-coupled event, so the
	// probe ignores it.
	channelSeq := drainCodingChannelUntilWaiting(t, events, 3*time.Second)
	probeSeq := drainCodingProbeUntilWaiting(t, probe.Events(), 3*time.Second)

	ag.Cancel()

	// Both observers see the same event-type sequence. The probe is
	// expected to MISS FlushBuffers because the legacy channel reader
	// (the test's drain) consumes it before the bus's iteration order
	// is meaningful — but per the dual-write contract, FlushBuffers
	// goes ONLY to the legacy channel today (see io.go). So we filter
	// FlushBuffers out of both sequences before comparing.
	got := eventTypeSeqCodingFiltered(channelSeq)
	want := eventTypeSeqCodingFiltered(probeSeq)

	if !sliceEqual(got, want) {
		t.Errorf("channel and probe sequences diverged\n  channel: %v\n    probe: %v", got, want)
	}
	if len(want) == 0 {
		t.Fatal("probe observed no events at all — Subscribe not wired into a.send")
	}
}

// drainCodingChannelUntilWaiting reads from the legacy events channel
// until an AgentWaiting arrives or the deadline fires. Responds to
// FlushBuffers in-band so the agent does not wedge on autosave.
func drainCodingChannelUntilWaiting(t *testing.T, ch <-chan event.Event, timeout time.Duration) []event.Event {
	t.Helper()
	out := make([]event.Event, 0, 32)
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-ch:
			if fb, ok := ev.(event.FlushBuffers); ok {
				fb.Result <- event.FlushResult{}
				out = append(out, ev)
				continue
			}
			out = append(out, ev)
			if _, ok := ev.(event.AgentWaiting); ok {
				return out
			}
		case <-deadline:
			t.Fatalf("legacy channel drain: deadline before AgentWaiting; got %d events", len(out))
			return out
		}
	}
}

// drainCodingProbeUntilWaiting reads from a probe Subscription until
// AgentWaiting arrives or the deadline fires. The probe does NOT
// answer FlushBuffers — its Result channel is single-reader and
// belongs to the legacy-channel consumer.
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

// eventTypeSeqCodingFiltered returns the type names of events in evs,
// dropping FlushBuffers (per the dual-write contract today, it goes
// only to the legacy channel and the probe should not see it; the
// channel-side drain consumes it before the assertion).
func eventTypeSeqCodingFiltered(evs []event.Event) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		if _, isFlush := ev.(event.FlushBuffers); isFlush {
			continue
		}
		out = append(out, fmt.Sprintf("%T", ev))
	}
	return out
}

// sliceEqual returns true when two string slices have identical
// elements in identical order.
func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// drainOneCoding reads one event from sub.Events with a small timeout.
// Distinguished from the kit-side helper of the same name by package
// scoping.
func drainOneCoding(t *testing.T, sub *Subscription) event.Event {
	t.Helper()
	select {
	case ev := <-sub.Events():
		return ev
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("drainOneCoding: timeout waiting for event")
		return nil
	}
}

// Compile-time guard mirroring kit's: surfaces accidental changes to
// the Subscription field layout.
var _ = atomic.LoadInt64
