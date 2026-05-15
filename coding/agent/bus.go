package agent

import (
	"slices"
	"sync"

	"github.com/latebit-io/nib/coding/event"
)

// bus is the in-process fan-out primitive that [Agent] uses to
// deliver coding-side events to one or more subscribers registered
// via [Agent.Subscribe]. The bus is the single writer to every
// subscriber's inbox channel.
//
// The bus is unexported: consumers obtain Subscriptions through
// [Agent.Subscribe] and never reach the bus directly.
//
// Concurrency model:
//
//   - subscribe / unsubscribe / close mutate the subscribers slice
//     under [bus.mu].
//   - publish snapshots the subscribers slice under [bus.mu], then
//     calls [Subscription.deliver] for each snapshot entry without
//     holding the lock — so a slow subscriber on a [Block] policy
//     does not freeze new subscribe/unsubscribe operations.
//   - Each [Subscription] holds its own [Subscription.deliverWG]
//     counting in-flight deliver calls to that subscription. The
//     per-subscription wait-group discipline mirrors kit's bus
//     (PR #151, post-Greptile P1 fix): closing one subscription
//     must never be coupled to deliveries in flight on another.
//
// Caller contract: publish may be called from any goroutine. Cross-
// publisher ordering matches the pre-bus channel semantics — channel
// sends from concurrent goroutines interleave non-deterministically,
// and so do bus publishes. Per-publisher order is preserved.
type bus struct {
	mu          sync.Mutex
	subscribers []*Subscription
	closed      bool
}

// newBus constructs an empty bus.
func newBus() *bus { return &bus{} }

// subscribe registers a new subscription. Returns [ErrAgentClosed]
// when the bus has already been closed (no further deliveries will
// occur).
//
// Late subscribers — Subscribe calls that race past earlier publishes
// — are valid; they start receiving from the next publish onward and
// miss every event already dispatched. The bus is fan-out, not a
// journal.
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
		return nil, ErrAgentClosed
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
// iterated without the lock. Each subscription's deliverWG is
// incremented under the lock alongside the snapshot — the load-bearing
// invariant from kit's bus that prevents a concurrent
// [Subscription.Close] from returning before a captured-in-snapshot
// publish has had a chance to run and Done.
func (b *bus) publish(ev event.Event) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	subs := slices.Clone(b.subscribers)
	for _, sub := range subs {
		sub.deliverWG.Add(1)
	}
	b.mu.Unlock()

	for _, sub := range subs {
		sub.deliver(ev)
	}
}

// tryPublish dispatches ev to every currently-registered subscriber on
// a best-effort, never-blocking basis: each subscriber's inbox is
// attempted with a non-blocking send. If the inbox is full, the event
// is dropped on that subscriber regardless of its configured policy,
// and [Subscription.drops] is incremented. Returns immediately when
// the bus is closed.
//
// tryPublish exists for the dual-write transitional phase
// ([Agent.send], [Agent.sendCritical]) where the legacy events
// channel is the canonical delivery path with its own bounded timeout
// and the bus is a shadow path for [Subscription]-attached observers.
// In that phase the legacy channel owns "guaranteed delivery" for
// Block-policy semantics and a blocking bus publish would void the
// legacy channel's deadline contract — exactly the failure mode
// Greptile flagged on PR #153.
//
// Once the legacy channel is removed (commit 4 of the kit-event-bus
// arc), call sites switch to [bus.publish] and each subscriber's
// configured [DropPolicy] takes effect normally.
//
// Same snapshot-under-lock, deliver-outside-lock, per-subscription
// deliverWG discipline as [bus.publish].
func (b *bus) tryPublish(ev event.Event) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	subs := slices.Clone(b.subscribers)
	for _, sub := range subs {
		sub.deliverWG.Add(1)
	}
	b.mu.Unlock()

	for _, sub := range subs {
		sub.tryDeliver(ev)
	}
}

// unsubscribe removes sub from the registry if present. Subsequent
// [bus.publish] calls will not deliver to sub. Idempotent.
//
// Does NOT signal [Subscription.done] or close the inbox; that is
// [Subscription.Close]'s responsibility.
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
// inbox channel is closed and no further [bus.publish] call will
// deliver events.
//
// Ordering:
//
//  1. Mark closed under the lock; subsequent publish/subscribe calls
//     observe this and short-circuit.
//  2. Snapshot and drop the subscribers slice.
//  3. Signal every subscription's done channel so in-flight
//     [Subscription.deliver] calls blocked on the inbox bail.
//  4. Wait on each subscription's [Subscription.deliverWG] in turn.
//     Per-subscription rather than bus-wide so iteration order does
//     not couple unrelated subscriptions; each wait finishes as soon
//     as that subscription's own in-flight delivers complete.
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
