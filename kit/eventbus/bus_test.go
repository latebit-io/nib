package eventbus

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/nib/kit/event"
)

type streamEvent struct {
	publisher int
	sequence  int
}

func (streamEvent) Event() {}

type controlEvent struct{ sequence int }

func (controlEvent) Event() {}

type observedContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (c *observedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}

func newTestBus() *Bus {
	return New(func(ev event.Event) bool {
		_, ok := ev.(streamEvent)
		return ok
	})
}

func mustSubscribe(t *testing.T, b *Bus, opts SubscribeOptions) *Subscription {
	t.Helper()
	sub, err := b.Subscribe(opts)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	return sub
}

func receive(t *testing.T, sub *Subscription) event.Event {
	t.Helper()
	select {
	case ev := <-sub.Events():
		return ev
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
		return nil
	}
}

func requireBlocked(t *testing.T, started, done <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("publication goroutine did not start")
	}
	select {
	case <-done:
		t.Fatal("publication returned while subscriber inbox was full")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestBusPublishesInOrderToAllSubscribers covers basic fan-out ordering.
func TestBusPublishesInOrderToAllSubscribers(t *testing.T) {
	b := newTestBus()
	t.Cleanup(b.Close)
	subs := []*Subscription{
		mustSubscribe(t, b, SubscribeOptions{BufferSize: 8}),
		mustSubscribe(t, b, SubscribeOptions{BufferSize: 8}),
	}
	want := []event.Event{
		streamEvent{sequence: 1},
		controlEvent{sequence: 2},
		streamEvent{sequence: 3},
	}
	for _, ev := range want {
		b.Publish(ev)
	}
	for i, sub := range subs {
		for j, expected := range want {
			if got := receive(t, sub); got != expected {
				t.Errorf("subscriber %d event %d = %#v; want %#v", i, j, got, expected)
			}
		}
	}
}

// TestSubscribeDefaultsBufferCapacity covers zero and negative sizes.
func TestSubscribeDefaultsBufferCapacity(t *testing.T) {
	for _, size := range []int{0, -1} {
		t.Run("size", func(t *testing.T) {
			b := newTestBus()
			t.Cleanup(b.Close)
			sub := mustSubscribe(t, b, SubscribeOptions{BufferSize: size})
			if got := cap(sub.Events()); got != defaultBufferSize {
				t.Errorf("capacity = %d; want %d", got, defaultBufferSize)
			}
		})
	}
}

// TestDefaultAndExplicitDropPolicies covers policy resolution by event class.
func TestDefaultAndExplicitDropPolicies(t *testing.T) {
	t.Run("default streaming drops", func(t *testing.T) {
		b := newTestBus()
		t.Cleanup(b.Close)
		sub := mustSubscribe(t, b, SubscribeOptions{BufferSize: 1})
		b.Publish(streamEvent{sequence: 1})
		b.Publish(streamEvent{sequence: 2})
		if got := sub.Drops(); got != 1 {
			t.Errorf("Drops = %d; want 1", got)
		}
	})

	t.Run("explicit control drop", func(t *testing.T) {
		b := newTestBus()
		t.Cleanup(b.Close)
		sub := mustSubscribe(t, b, SubscribeOptions{BufferSize: 1, OnControlFull: Drop})
		b.Publish(controlEvent{sequence: 1})
		b.Publish(controlEvent{sequence: 2})
		if got := sub.Drops(); got != 1 {
			t.Errorf("Drops = %d; want 1", got)
		}
	})

	t.Run("explicit streaming block", func(t *testing.T) {
		b := newTestBus()
		t.Cleanup(b.Close)
		mustSubscribe(t, b, SubscribeOptions{BufferSize: 1, OnStreamingFull: Block})
		b.Publish(streamEvent{sequence: 1})
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		defer cancel()
		if err := b.PublishContext(ctx, streamEvent{sequence: 2}); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("PublishContext error = %v; want context deadline", err)
		}
	})

	t.Run("nil classifier defaults to control", func(t *testing.T) {
		b := New(nil)
		t.Cleanup(b.Close)
		mustSubscribe(t, b, SubscribeOptions{BufferSize: 1})
		b.Publish(streamEvent{sequence: 1})
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		defer cancel()
		if err := b.PublishContext(ctx, streamEvent{sequence: 2}); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("PublishContext error = %v; want context deadline", err)
		}
	})
}

// TestDefaultControlPublishBlocks verifies unbounded control backpressure.
func TestDefaultControlPublishBlocks(t *testing.T) {
	started := make(chan struct{})
	var startedOnce sync.Once
	b := New(func(ev event.Event) bool {
		if control, ok := ev.(controlEvent); ok && control.sequence == 2 {
			startedOnce.Do(func() { close(started) })
		}
		return false
	})
	t.Cleanup(b.Close)
	sub := mustSubscribe(t, b, SubscribeOptions{BufferSize: 1})
	b.Publish(controlEvent{sequence: 1})
	done := make(chan struct{})
	go func() {
		b.Publish(controlEvent{sequence: 2})
		close(done)
	}()
	requireBlocked(t, started, done)
	<-sub.Events()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("publication did not resume after subscriber drained")
	}
}

// TestSubscribeRejectsInvalidPolicy prevents silent event loss.
func TestSubscribeRejectsInvalidPolicy(t *testing.T) {
	tests := []SubscribeOptions{
		{OnStreamingFull: DropPolicy(-1)},
		{OnStreamingFull: DropPolicy(3)},
		{OnControlFull: DropPolicy(-1)},
		{OnControlFull: DropPolicy(3)},
	}
	for _, opts := range tests {
		b := newTestBus()
		if _, err := b.Subscribe(opts); !errors.Is(err, ErrInvalidPolicy) {
			t.Errorf("Subscribe(%+v) error = %v; want ErrInvalidPolicy", opts, err)
		}
		b.Close()
	}

	b := newTestBus()
	b.Close()
	if _, err := b.Subscribe(SubscribeOptions{OnControlFull: DropPolicy(99)}); !errors.Is(err, ErrClosed) {
		t.Errorf("closed bus error = %v; want ErrClosed", err)
	}
}

// TestClassifierCanCloseBus ensures classification runs before delivery accounting.
func TestClassifierCanCloseBus(t *testing.T) {
	var b *Bus
	b = New(func(event.Event) bool {
		b.Close()
		return false
	})
	mustSubscribe(t, b, SubscribeOptions{})
	done := make(chan struct{})
	go func() {
		b.Publish(controlEvent{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("classifier closing bus deadlocked publication")
	}
}

// TestPublishAfterCloseSkipsClassifier preserves closed-bus short-circuiting.
func TestPublishAfterCloseSkipsClassifier(t *testing.T) {
	var calls atomic.Int64
	b := New(func(event.Event) bool {
		calls.Add(1)
		return false
	})
	b.Close()
	b.Publish(controlEvent{})
	if err := b.PublishContext(context.Background(), controlEvent{}); !errors.Is(err, ErrClosed) {
		t.Errorf("PublishContext error = %v; want ErrClosed", err)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("classifier calls after Close = %d; want 0", got)
	}
}

// TestSubscriptionAndBusClose tracks registration and channel lifecycle.
func TestSubscriptionAndBusClose(t *testing.T) {
	b := newTestBus()
	first := mustSubscribe(t, b, SubscribeOptions{})
	second := mustSubscribe(t, b, SubscribeOptions{})
	if got := b.SubscriberCount(); got != 2 {
		t.Fatalf("SubscriberCount = %d; want 2", got)
	}

	first.Close()
	first.Close()
	if got := b.SubscriberCount(); got != 1 {
		t.Errorf("SubscriberCount after Close = %d; want 1", got)
	}
	if _, ok := <-first.Events(); ok {
		t.Error("subscription inbox remained open after Close")
	}

	b.Close()
	b.Close()
	if got := b.SubscriberCount(); got != 0 {
		t.Errorf("SubscriberCount after bus Close = %d; want 0", got)
	}
	if _, ok := <-second.Events(); ok {
		t.Error("subscription inbox remained open after bus Close")
	}
	if _, err := b.Subscribe(SubscribeOptions{}); !errors.Is(err, ErrClosed) {
		t.Errorf("Subscribe after Close error = %v; want ErrClosed", err)
	}
}

// TestConcurrentPublishPreservesEachPublisherOrder locks the concurrency contract.
func TestConcurrentPublishPreservesEachPublisherOrder(t *testing.T) {
	const (
		publishers   = 4
		perPublisher = 50
	)
	b := New(nil)
	t.Cleanup(b.Close)
	sub := mustSubscribe(t, b, SubscribeOptions{BufferSize: publishers * perPublisher})

	var wg sync.WaitGroup
	for publisher := range publishers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sequence := range perPublisher {
				b.Publish(streamEvent{publisher: publisher, sequence: sequence})
			}
		}()
	}
	wg.Wait()

	next := make([]int, publishers)
	for range publishers * perPublisher {
		got, ok := receive(t, sub).(streamEvent)
		if !ok {
			t.Fatal("received unexpected event type")
		}
		if got.sequence != next[got.publisher] {
			t.Errorf("publisher %d sequence = %d; want %d", got.publisher, got.sequence, next[got.publisher])
		}
		next[got.publisher]++
	}
}

// TestSubscriptionCloseDoesNotWaitForAnotherSubscription covers wait-group isolation.
func TestSubscriptionCloseDoesNotWaitForAnotherSubscription(t *testing.T) {
	b := newTestBus()
	t.Cleanup(b.Close)
	fast := mustSubscribe(t, b, SubscribeOptions{BufferSize: 2})
	slow := mustSubscribe(t, b, SubscribeOptions{BufferSize: 1})
	b.Publish(controlEvent{sequence: 1})
	<-fast.Events()

	published := make(chan struct{})
	go func() {
		b.Publish(controlEvent{sequence: 2})
		close(published)
	}()
	if got := receive(t, fast); got != (controlEvent{sequence: 2}) {
		t.Fatalf("fast subscriber event = %#v", got)
	}

	closed := make(chan struct{})
	go func() {
		fast.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close waited for unrelated blocked subscriber")
	}
	<-slow.Events()
	select {
	case <-published:
	case <-time.After(time.Second):
		t.Fatal("publication did not resume after slow subscriber drained")
	}
}

// TestPublishContextDoesNotLetBlockedSubscriberStarveHealthySubscriber covers parallel fan-out.
func TestPublishContextDoesNotLetBlockedSubscriberStarveHealthySubscriber(t *testing.T) {
	b := newTestBus()
	t.Cleanup(b.Close)
	slow := mustSubscribe(t, b, SubscribeOptions{BufferSize: 1})
	healthy := mustSubscribe(t, b, SubscribeOptions{BufferSize: 2})
	b.Publish(controlEvent{sequence: 1})
	<-healthy.Events()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.PublishContext(ctx, controlEvent{sequence: 2}) }()
	if got := receive(t, healthy); got != (controlEvent{sequence: 2}) {
		t.Errorf("healthy subscriber event = %#v", got)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("PublishContext error = %v; want context cancellation", err)
	}
	<-slow.Events()
}

// TestCloseInterruptsContextPublication covers shutdown during bounded delivery.
func TestCloseInterruptsContextPublication(t *testing.T) {
	b := newTestBus()
	sub := mustSubscribe(t, b, SubscribeOptions{BufferSize: 1})
	b.Publish(controlEvent{sequence: 1})

	ctx := &observedContext{Context: context.Background(), observed: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- b.PublishContext(ctx, controlEvent{sequence: 2}) }()
	select {
	case <-ctx.observed:
	case <-time.After(time.Second):
		t.Fatal("PublishContext did not enter subscriber delivery")
	}
	b.Close()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, ErrClosed) {
			t.Errorf("PublishContext during Close error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("PublishContext did not stop when bus closed")
	}
	for range sub.Events() {
	}
}

// TestSubscribeRacingCloseIsSafe exercises registry lifecycle under the race detector.
func TestSubscribeRacingCloseIsSafe(t *testing.T) {
	b := newTestBus()
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if sub, err := b.Subscribe(SubscribeOptions{}); err == nil {
				sub.Close()
			}
		}()
	}
	b.Close()
	wg.Wait()
}
