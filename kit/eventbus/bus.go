package eventbus

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/latebit-io/nib/kit/event"
)

const defaultBufferSize = 64

var (
	// ErrClosed is returned when subscribing or publishing with context
	// after the bus has closed.
	ErrClosed = errors.New("eventbus: closed")

	// ErrInvalidPolicy is returned when subscription options contain an
	// unsupported delivery policy.
	ErrInvalidPolicy = errors.New("eventbus: invalid drop policy")
)

// IsStreaming classifies high-volume events whose loss is recoverable. It
// must be safe for concurrent calls. A nil classifier treats every event as
// control.
type IsStreaming func(event.Event) bool

// Bus synchronously fans published events out to independent subscribers.
// Construct with [New]. Bus methods are safe for concurrent use.
//
// Each subscriber inbox has Bus as its sole writer. Publication order is
// preserved within one publisher; concurrent publishers may interleave.
type Bus struct {
	mu          sync.Mutex
	subscribers []*Subscription
	isStreaming IsStreaming
	closed      bool
}

// New constructs an empty Bus using isStreaming to resolve default
// delivery policies. A nil classifier treats every event as control.
func New(isStreaming IsStreaming) *Bus {
	if isStreaming == nil {
		isStreaming = func(event.Event) bool { return false }
	}
	return &Bus{isStreaming: isStreaming}
}

// Subscribe registers a subscriber that receives events published after
// this call. The bus is fan-out, not a journal, so earlier events are not
// replayed.
func (b *Bus) Subscribe(opts SubscribeOptions) (*Subscription, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, ErrClosed
	}
	if err := opts.validate(); err != nil {
		return nil, err
	}
	if opts.BufferSize <= 0 {
		opts.BufferSize = defaultBufferSize
	}
	sub := &Subscription{
		bus:             b,
		streamingPolicy: opts.OnStreamingFull.resolve(Drop),
		controlPolicy:   opts.OnControlFull.resolve(Block),
		inbox:           make(chan event.Event, opts.BufferSize),
		done:            make(chan struct{}),
	}

	b.subscribers = append(b.subscribers, sub)
	return sub, nil
}

// Publish delivers ev to every current subscriber according to its policy.
// It returns without delivery after the bus closes.
func (b *Bus) Publish(ev event.Event) {
	streaming, closed := b.classify(ev)
	if closed {
		return
	}
	subs, _ := b.snapshot() // closed after classify: subs is nil, nothing to deliver
	for _, sub := range subs {
		sub.deliver(ev, streaming)
	}
}

// PublishContext delivers ev to every current subscriber with ctx-bounded
// Block policy sends. Subscribers are attempted concurrently so one blocked
// inbox cannot consume another subscriber's deadline. Drop policy remains
// non-blocking. It returns [ErrClosed] after the bus closes.
func (b *Bus) PublishContext(ctx context.Context, ev event.Event) error {
	streaming, closed := b.classify(ev)
	if closed {
		return ErrClosed
	}
	subs, closed := b.snapshot()
	if closed {
		return ErrClosed
	}

	// deliverContext can only fail with ctx.Err(), so one flag suffices.
	var (
		wg       sync.WaitGroup
		deadline atomic.Bool
	)
	for _, sub := range subs {
		wg.Go(func() {
			if sub.deliverContext(ctx, ev, streaming) != nil {
				deadline.Store(true)
			}
		})
	}
	wg.Wait()
	if deadline.Load() {
		return ctx.Err()
	}
	return nil
}

// SubscriberCount returns the number of live subscriptions.
func (b *Bus) SubscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subscribers)
}

// Close stops future publication and closes every current subscription after
// its in-flight deliveries finish. Close is idempotent.
func (b *Bus) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	subs := b.subscribers
	b.subscribers = nil
	b.mu.Unlock()

	for _, sub := range subs {
		sub.signalDone()
	}
	for _, sub := range subs {
		sub.deliverWG.Wait()
	}
	for _, sub := range subs {
		sub.finishClose()
	}
}

func (b *Bus) classify(ev event.Event) (bool, bool) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return false, true
	}
	classifier := b.isStreaming
	b.mu.Unlock()
	return classifier(ev), false
}

// snapshot returns the live subscribers with each one's delivery counted,
// or closed=true after Close.
func (b *Bus) snapshot() (subs []*Subscription, closed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, true
	}
	subs = slices.Clone(b.subscribers)
	// Add while holding the registry lock so Subscription.Close cannot
	// begin waiting after removal but before this publication is counted.
	for _, sub := range subs {
		sub.deliverWG.Add(1)
	}
	return subs, false
}

func (b *Bus) unsubscribe(sub *Subscription) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if i := slices.Index(b.subscribers, sub); i >= 0 {
		b.subscribers = slices.Delete(b.subscribers, i, i+1)
	}
}

func (o SubscribeOptions) validate() error {
	if !o.OnStreamingFull.valid() {
		return fmt.Errorf("%w: OnStreamingFull=%d", ErrInvalidPolicy, o.OnStreamingFull)
	}
	if !o.OnControlFull.valid() {
		return fmt.Errorf("%w: OnControlFull=%d", ErrInvalidPolicy, o.OnControlFull)
	}
	return nil
}

func (p DropPolicy) valid() bool {
	return p >= DefaultPolicy && p <= Drop
}

// resolve maps DefaultPolicy to the event class's default.
func (p DropPolicy) resolve(def DropPolicy) DropPolicy {
	if p == DefaultPolicy {
		return def
	}
	return p
}
