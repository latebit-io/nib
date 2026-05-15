package agent

import (
	"context"
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

// publishContext dispatches ev to every currently-registered subscriber
// with ctx-bounded delivery semantics. Drop-policy subscribers delegate
// to the non-blocking inbox send (they are lossy by choice; ctx does
// not apply). Block-policy subscribers attempt a select-bounded inbox
// send that aborts on ctx cancellation.
//
// Returns the first ctx.Err() observed across subscribers; subscribers
// after the first deadline miss are still attempted on a best-effort
// basis so a single slow Block-policy subscriber does not silently
// drop the event for every subscriber behind it in iteration order.
// Returns [ErrAgentClosed] when the bus is closed.
//
// publishContext is the post-channel-removal home for the deadline
// contract that the legacy events channel's 5-second timer carried in
// [Agent.sendCritical]. [bus.publish] (the unbounded-block variant) is
// wrong for sendCritical (Greptile P1, PR #153); [bus.tryPublish] (the
// always-drop variant) is wrong because it does not honor a Block-
// policy consumer's "guaranteed delivery up to the deadline" contract.
// publishContext is the middle: deliver guaranteed for Block-policy
// subscribers, but ONLY up to the supplied deadline.
//
// Same snapshot-under-lock, deliver-outside-lock, per-subscription
// deliverWG discipline as [bus.publish] and [bus.tryPublish].
func (b *bus) publishContext(ctx context.Context, ev event.Event) error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return ErrAgentClosed
	}
	subs := slices.Clone(b.subscribers)
	for _, sub := range subs {
		sub.deliverWG.Add(1)
	}
	b.mu.Unlock()

	var firstErr error
	for _, sub := range subs {
		if err := sub.deliverContext(ctx, ev); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
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
