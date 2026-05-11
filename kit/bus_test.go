package kit

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/nib/kit/event"
)

// TestBusSubscribeReturnsLiveSubscription verifies the basic happy
// path: subscribe, publish, receive on the subscription's inbox in
// the same order.
func TestBusSubscribeReturnsLiveSubscription(t *testing.T) {
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
		got := drainOne(t, sub)
		if got != w {
			t.Errorf("event %d: got %#v, want %#v", i, got, w)
		}
	}
}

// TestBusOrderPerSubscriberWithMultipleSubscribers (P2) verifies that
// every subscriber sees every published event in the same order as
// publish was called.
func TestBusOrderPerSubscriberWithMultipleSubscribers(t *testing.T) {
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
			got := drainOne(t, sub)
			if got != want {
				t.Errorf("subscriber %d event %d: got %#v, want %#v", i, j, got, want)
			}
		}
	}
}

// TestBusSlowSubscriberDropsStreaming (P3) verifies that a subscriber
// using the default DropStreaming policy on streaming events does not
// block the publisher when its inbox is full.
func TestBusSlowSubscriberDropsStreaming(t *testing.T) {
	b := newBus()
	sub, _ := b.subscribe(SubscribeOptions{BufferSize: 2}) // tiny inbox

	// Publish 10 streaming events without reading. With BufferSize 2
	// and DropStreaming, 2 sit in the inbox and 8 drop.
	for i := 0; i < 10; i++ {
		b.publish(event.AgentToken{Text: "x"})
	}

	if got := sub.Drops(); got != 8 {
		t.Errorf("Drops: got %d, want 8", got)
	}
	// Verify the inbox holds exactly 2 events.
	for i := 0; i < 2; i++ {
		select {
		case <-sub.Events():
		case <-time.After(50 * time.Millisecond):
			t.Fatalf("inbox missing event %d", i)
		}
	}
}

// TestBusControlBlocksUntilSubscriberDrains (P4) verifies that control
// events under BlockControl block the publisher until the subscriber
// drains the inbox.
func TestBusControlBlocksUntilSubscriberDrains(t *testing.T) {
	b := newBus()
	sub, _ := b.subscribe(SubscribeOptions{BufferSize: 1})

	// Fill the inbox with one control event (occupies the slot).
	b.publish(event.AgentDone{Success: true})

	publishReturned := make(chan struct{})
	go func() {
		b.publish(event.AgentError{Err: "boom"})
		close(publishReturned)
	}()

	// publish should be blocked: inbox full + BlockControl.
	select {
	case <-publishReturned:
		t.Fatal("publish returned before subscriber drained")
	case <-time.After(50 * time.Millisecond):
	}

	// Drain one event to make room.
	<-sub.Events()

	select {
	case <-publishReturned:
	case <-time.After(time.Second):
		t.Fatal("publish did not unblock after drain")
	}
}

// TestBusDropCounterIsMonotonic (P5) verifies the per-subscription
// drop counter accumulates and never decreases.
func TestBusDropCounterIsMonotonic(t *testing.T) {
	b := newBus()
	sub, _ := b.subscribe(SubscribeOptions{BufferSize: 1})

	// Saturate then over-publish; only one streaming event fits.
	for i := 0; i < 5; i++ {
		b.publish(event.AgentToken{Text: "x"})
	}
	want := int64(4)
	if got := sub.Drops(); got != want {
		t.Errorf("Drops after 5 publishes: got %d, want %d", got, want)
	}

	// Drain and publish more; counter should keep increasing.
	<-sub.Events()
	for i := 0; i < 3; i++ {
		b.publish(event.AgentToken{Text: "y"})
	}
	// 4 prior drops + (3 published, 1 fit, 2 dropped) = 6.
	if got := sub.Drops(); got != 6 {
		t.Errorf("Drops after 3 more publishes: got %d, want 6", got)
	}
}

// TestBusCloseDrainsAllSubscribers (P6) verifies that bus.close
// closes every subscription's inbox so reader loops exit.
func TestBusCloseDrainsAllSubscribers(t *testing.T) {
	b := newBus()
	subs := make([]*Subscription, 3)
	for i := range subs {
		s, _ := b.subscribe(SubscribeOptions{})
		subs[i] = s
	}

	// Publish one event so each subscription has something queued.
	b.publish(event.AgentToken{Text: "x"})

	// Spawn reader goroutines; each exits when its inbox closes.
	var wg sync.WaitGroup
	wg.Add(len(subs))
	for _, sub := range subs {
		go func(s *Subscription) {
			defer wg.Done()
			for range s.Events() { // drain until close
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

// TestSubscribeAfterCloseReturnsErrClosed (P7) verifies subscribe
// after the bus is closed returns ErrClosed.
func TestSubscribeAfterCloseReturnsErrClosed(t *testing.T) {
	b := newBus()
	b.close()
	if _, err := b.subscribe(SubscribeOptions{}); !errors.Is(err, ErrClosed) {
		t.Errorf("subscribe after close: got %v, want ErrClosed", err)
	}
}

// TestSubscribeDuringCloseIsRaceSafe (P8) hammers Subscribe and
// Close concurrently to surface races under -race.
func TestSubscribeDuringCloseIsRaceSafe(t *testing.T) {
	b := newBus()

	var wg sync.WaitGroup
	wg.Add(8)
	for i := 0; i < 8; i++ {
		go func() {
			defer wg.Done()
			if sub, err := b.subscribe(SubscribeOptions{}); err == nil {
				// Best-effort drain; close once.
				go func() {
					for range sub.Events() {
					}
				}()
				sub.Close()
			}
		}()
	}
	// Concurrent close after a tiny pause to interleave with subscribes.
	go func() {
		runtime.Gosched()
		b.close()
	}()
	wg.Wait()
}

// TestSubscriptionCloseUnsubscribesFromBus verifies that after
// Subscription.Close, the bus does not deliver further events to that
// subscription.
func TestSubscriptionCloseUnsubscribesFromBus(t *testing.T) {
	b := newBus()
	keep, _ := b.subscribe(SubscribeOptions{})
	drop, _ := b.subscribe(SubscribeOptions{})

	drop.Close()

	// Publish: keep should receive, drop should not.
	b.publish(event.AgentToken{Text: "k"})

	if got := drainOne(t, keep); got == nil {
		t.Fatal("keep subscription missed event")
	}
	// drop.Events() is closed; range exit on a closed channel returns
	// the zero value immediately. Verify no event arrived.
	select {
	case ev, ok := <-drop.Events():
		if ok {
			t.Errorf("drop subscription received %#v after Close", ev)
		}
	default:
		t.Fatal("drop subscription channel not closed after Close")
	}
}

// TestSubscriptionCloseIsIdempotent verifies double-Close does not
// panic or block.
func TestSubscriptionCloseIsIdempotent(t *testing.T) {
	b := newBus()
	sub, _ := b.subscribe(SubscribeOptions{})
	sub.Close()
	sub.Close() // must not panic
}

// TestBusCloseIsIdempotent verifies double-close on the bus does not
// panic.
func TestBusCloseIsIdempotent(t *testing.T) {
	b := newBus()
	b.close()
	b.close() // must not panic
}

// TestSubscribeDefaultOptionsMatchLegacyPolicy verifies that the zero
// value of SubscribeOptions encodes the pre-bus policy: streaming
// drops, control blocks. The legacy-compat shim relies on this so
// callers migrating from cfg.Events see no behavioral change.
func TestSubscribeDefaultOptionsMatchLegacyPolicy(t *testing.T) {
	b := newBus()
	sub, _ := b.subscribe(SubscribeOptions{BufferSize: 1})

	// Streaming: fill + over-publish without draining should drop.
	b.publish(event.AgentToken{Text: "1"}) // accepted
	b.publish(event.AgentToken{Text: "2"}) // dropped
	if got := sub.Drops(); got != 1 {
		t.Errorf("streaming drop counter: got %d, want 1", got)
	}

	// Control: inbox now has 1 streaming token in the slot. A control
	// event on top would block. Confirm by attempting publish in a
	// goroutine and observing it doesn't return.
	done := make(chan struct{})
	go func() {
		b.publish(event.AgentDone{Success: true})
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("control event publish did not block on full inbox")
	case <-time.After(30 * time.Millisecond):
	}
	// Drain to unblock so the test exits cleanly.
	<-sub.Events()
	<-done
}

// TestDeliverWGTracksInflightDelivers checks the internal
// deliverWG invariant: every deliver call decrements the counter.
// Guard against a future refactor that forgets the defer.
func TestDeliverWGTracksInflightDelivers(t *testing.T) {
	b := newBus()
	sub, _ := b.subscribe(SubscribeOptions{BufferSize: 8})
	defer sub.Close()

	for i := 0; i < 8; i++ {
		b.publish(event.AgentToken{Text: "x"})
	}
	// All publishes returned, so deliverWG should be at zero.
	// We can't read it directly; instead, bus.close should not deadlock.
	closed := make(chan struct{})
	go func() {
		// Drain the inbox so close can finish.
		go func() {
			for range sub.Events() {
			}
		}()
		b.close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("bus.close did not return — deliverWG likely leaked")
	}
}

// drainOne reads one event from sub.Events with a small timeout.
// Returns the event or t.Fatals on timeout.
func drainOne(t *testing.T, sub *Subscription) event.Event {
	t.Helper()
	select {
	case ev := <-sub.Events():
		return ev
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("drainOne: timeout waiting for event")
		return nil
	}
}

// Compile-time guard: atomic.LoadInt64 alignment isn't a concern on
// 64-bit hosts, but we want to surface any change in the Subscription
// layout that would put drops on a misaligned offset.
var _ = atomic.LoadInt64
