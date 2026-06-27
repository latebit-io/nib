package kit

import (
	"slices"
	"sync"

	"github.com/latebit-io/nib/kit/event"
)

// bus is the in-process fan-out primitive that [Agent] uses to deliver
// translated events to one or more subscribers. The bus is the single
// writer to every subscriber's inbox channel; subscribers register
// via [bus.subscribe] and tear down via [Subscription.Close].
//
// The bus is unexported: consumers obtain Subscriptions through
// [Agent.Subscribe] and never reach the bus directly. This keeps the
// bus type free to evolve (e.g., to share an implementation between
// kit and coding layers via a generic) without breaking the public
// surface.
//
// Concurrency model:
//
//   - subscribe / unsubscribe / close mutate the subscribers slice
//     under [bus.mu].
//   - publish snapshots the subscribers slice under [bus.mu], then
//     calls [Subscription.deliver] for each snapshot entry without
//     holding the lock — so a slow subscriber on a [Block] policy does
//     not freeze new subscribe/unsubscribe operations.
//   - deliveries are serialized by the publisher (one goroutine
//     iterates the snapshot in order). Per-subscriber ordering is
//     therefore trivially preserved; cross-subscriber ordering is
//     undefined when policies differ (a fast [Drop]-policy subscriber
//     may "race ahead" of a slow [Block]-policy one, but each sees its
//     own stream in publish order).
//   - Each [Subscription] holds its own [Subscription.deliverWG]
//     counting in-flight deliver calls to that subscription.
//     [Subscription.Close] waits on the per-subscription wait-group;
//     [bus.close] waits on each subscription's wait-group in turn.
//     Per-subscription rather than per-bus so closing one
//     subscription is never coupled to deliveries in flight on
//     another (a [Block]-policy subscriber stuck on a slow consumer
//     must not block an unrelated subscription's shutdown).
type bus struct {
	mu          sync.Mutex
	subscribers []*Subscription
	closed      bool
}

// newBus constructs an empty bus.
func newBus() *bus { return &bus{} }

// subscribe registers a new subscription. Returns [ErrClosed] when the
// bus has already been closed (no further deliveries will occur).
//
// Late subscribers — Subscribe calls that race past the first
// [bus.publish] — are valid; they start receiving from the next
// publish onward and miss every event already dispatched. This
// matches the plan's "late subscribers start at next Publish"
// semantics; the bus is fan-out, not a journal.
func (b *bus) subscribe(opts SubscribeOptions) (*Subscription, error) {
	if opts.BufferSize <= 0 {
		opts.BufferSize = 64
	}
	sub := &Subscription{
		bus:   b,
		opts:  opts,
		inbox: make(chan event.Event, opts.BufferSize),
		done:  make(chan struct{}),
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, ErrClosed
	}
	b.subscribers = append(b.subscribers, sub)
	return sub, nil
}

// publish dispatches ev to every currently-registered subscriber per
// each subscriber's configured policy. Returns immediately when the
// bus is closed.
//
// The subscribers slice is snapshotted under [bus.mu] so a concurrent
// unsubscribe cannot mutate it mid-iteration; the snapshot is then
// iterated without the lock so a [Block] subscriber blocking the
// publisher does not also freeze the subscriber registry.
//
// Caller contract: publish must be called from a serialized publisher
// goroutine. Concurrent calls leave per-subscriber order undefined
// (each subscriber still sees a consistent stream, but two publishes
// racing on the same subscriber may interleave in any order). The
// kit translator is the sole publisher for the kit-internal bus and
// runs on a single goroutine, so the constraint is met by
// construction; future consumers of this primitive (e.g., a
// coding-side bus driven by a single forwarder) must hold the same
// discipline or introduce a publish-side lock here.
func (b *bus) publish(ev event.Event) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	subs := slices.Clone(b.subscribers)
	// Add to each subscription's wait-group under the registry lock,
	// before any iteration. This guarantees that a concurrent
	// [Subscription.Close] that has already taken the snapshot's
	// subscription out via [bus.unsubscribe] cannot return from
	// [Subscription.Close.deliverWG.Wait] before this publish's
	// deliver call has had a chance to run and Done.
	for _, sub := range subs {
		sub.deliverWG.Add(1)
	}
	b.mu.Unlock()

	for _, sub := range subs {
		sub.deliver(ev)
	}
}

// unsubscribe removes sub from the registry if present. Subsequent
// [bus.publish] calls will not deliver to sub. Idempotent — calling
// unsubscribe on an already-removed subscription is a no-op.
//
// Does NOT signal [Subscription.done] or close the inbox; that is
// [Subscription.Close]'s responsibility (it calls unsubscribe before
// signalling and closing).
func (b *bus) unsubscribe(sub *Subscription) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, s := range b.subscribers {
		if s == sub {
			b.subscribers = slices.Delete(b.subscribers, i, i+1)
			return
		}
	}
}

// close shuts the bus down. After close returns, every subscriber's
// inbox channel is closed (reader loops exit naturally) and no further
// [bus.publish] call will deliver events.
//
// Ordering:
//
//  1. Mark closed under the lock. Subsequent publish/subscribe
//     calls observe this and short-circuit.
//  2. Snapshot and drop the subscribers slice.
//  3. Signal every subscription's done channel so in-flight
//     [Subscription.deliver] calls blocked on the inbox bail.
//  4. Wait on each subscription's [Subscription.deliverWG] for its
//     in-flight delivers to drain. Per-subscription rather than
//     bus-wide so the iteration order does not couple unrelated
//     subscriptions; each wait finishes as soon as that subscription's
//     own in-flight delivers complete.
//  5. Close each subscription's inbox channel.
//
// Idempotent.
func (b *bus) close() {
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
