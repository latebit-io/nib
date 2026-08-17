package kit

import (
	"sync"
	"sync/atomic"

	"github.com/latebit-io/nib/kit/event"
)

// DropPolicy names how the bus reacts when a subscriber's inbox is
// full for an event of a given class.
//
// The two operative policies, [Block] and [Drop], are not
// interchangeable. Block preserves "guaranteed delivery" semantics:
// the publisher blocks until the subscriber drains. Drop preserves
// "best effort" semantics: the publisher discards the event and
// increments [Subscription.Drops].
//
// The zero value, [DefaultPolicy], is class-sensitive: it resolves to
// [Drop] for streaming events ([event.AgentToken],
// [event.AgentTurnUsage], [event.AgentInputEstimate]) and to [Block]
// for every other event. This matches the legacy pre-bus behavior on
// the single consumer channel; subscribers that don't customize see
// streaming events drop on full and control events block until the
// inbox accepts.
//
// Subscribers that can tolerate event loss on control events (e.g., a
// metrics probe that does not care about AgentDone) may opt into
// [Drop] for OnControlFull, but this loses delivery guarantees for
// lifecycle and failure signals. Subscribers that need strict
// ordering of streaming events (e.g., a transcript recorder) may opt
// into [Block] for OnStreamingFull, accepting the back-pressure cost.
type DropPolicy int

const (
	// DefaultPolicy resolves at deliver time to [Drop] for streaming
	// events and [Block] for control events. The zero value of
	// [DropPolicy], so leaving a [SubscribeOptions] field unset
	// produces the legacy single-consumer-channel semantics.
	DefaultPolicy DropPolicy = iota

	// Block makes the publisher block until the subscriber's inbox
	// accepts the event. Combined with a sufficiently buffered
	// inbox, this is the right policy for events whose loss would
	// strand the subscriber.
	Block

	// Drop makes the publisher discard the event and increment the
	// subscriber's drop counter. Right for high-volume, individually
	// replaceable events.
	Drop
)

// SubscribeOptions configures a single subscription's inbox buffer
// size and per-event-class delivery policy.
//
// Zero value means "library defaults" — BufferSize 64, OnStreamingFull
// [DefaultPolicy] (resolves to [Drop]), OnControlFull [DefaultPolicy]
// (resolves to [Block]). These defaults preserve the pre-bus
// consumer-channel behavior on every consumer that migrates to
// Subscribe without customizing the options.
type SubscribeOptions struct {
	// BufferSize is the subscriber inbox channel capacity. Zero or
	// negative means the default (64). High-volume tests that publish
	// thousands of events before draining may need to raise this.
	BufferSize int

	// OnStreamingFull is the policy when the inbox would block on a
	// streaming event ([event.AgentToken], [event.AgentTurnUsage],
	// [event.AgentInputEstimate]). Zero value is [DefaultPolicy],
	// which resolves to [Drop] for streaming events.
	OnStreamingFull DropPolicy

	// OnControlFull is the policy when the inbox would block on a
	// control event (every event type that is not streaming). Zero
	// value is [DefaultPolicy], which resolves to [Block] for control
	// events — guaranteed delivery, with the cost that a wedged
	// subscriber blocks the publisher.
	OnControlFull DropPolicy
}

// Subscription is one subscriber's view of the agent's event stream.
// Construct via [Agent.Subscribe]; release with [Subscription.Close]
// when the subscriber no longer needs events. The internal bus is the
// sole writer to the subscription's inbox channel; the subscriber is
// the sole reader.
//
// Lifetime: a Subscription is live from Subscribe until Close (or
// until the parent [Agent.Close] forces it). Closing the subscription
// closes the inbox channel — `for ev := range sub.Events()` exits
// naturally after the last buffered event drains. Concurrent calls
// to Close are safe; the second and subsequent calls are no-ops.
type Subscription struct {
	bus  *bus
	opts SubscribeOptions

	inbox chan event.Event

	// done is closed by signalDone to interrupt any in-flight deliver
	// call that is blocked on the inbox. Idempotent via doneOnce.
	done     chan struct{}
	doneOnce sync.Once

	// closeOnce guards the close-inbox step; both [Subscription.Close]
	// and bus shutdown route through it so concurrent shutdown paths
	// never double-close the inbox channel.
	closeOnce sync.Once

	// deliverWG tracks in-flight [Subscription.deliver] calls TO THIS
	// SUBSCRIPTION. Per-subscription rather than per-bus so
	// [Subscription.Close] only waits for its own in-flight delivers
	// — a Close on subscription A must not block on a deliver-in-flight
	// to subscription B (which would happen with a shared bus-level
	// wait-group whenever B is on a [Block] policy with a slow consumer).
	deliverWG sync.WaitGroup

	// drops counts events dropped under [Drop] policy. Atomic so
	// [Subscription.Drops] can read it without locks.
	drops int64
}

// Events returns the inbox channel the bus delivers events to. The
// channel is closed by [Subscription.Close] (caller-initiated) or by
// the parent [Agent.Close] (forced). After the channel is closed the
// `for ev := range sub.Events()` consumer loop exits naturally once
// the buffered tail drains.
func (s *Subscription) Events() <-chan event.Event {
	return s.inbox
}

// Drops returns the monotonic count of events dropped on this
// subscription's inbox when a [Drop] policy found the inbox full.
// Both event classes contribute: a streaming event dropped under
// [SubscribeOptions.OnStreamingFull] and a control event dropped under
// [SubscribeOptions.OnControlFull] each increment this counter.
// Observable so subscribers can detect when they are falling behind
// without inspecting the channel internals.
func (s *Subscription) Drops() int64 {
	return atomic.LoadInt64(&s.drops)
}

// Close unsubscribes and releases the subscription. After Close
// returns, no further events are delivered; in-flight deliver calls
// observe the closure and bail without sending; the inbox channel is
// closed so reader loops exit.
//
// Close blocks until in-flight deliver calls TO THIS subscription
// drain — bounded by the configured policy ([Drop] returns
// immediately; [Block] returns once the deliver bails on
// [Subscription.done]). Crucially, Close does NOT wait on deliveries
// to other subscriptions; a slow [Block]-policy subscriber on the
// same bus cannot delay this Close. Idempotent.
func (s *Subscription) Close() {
	s.bus.unsubscribe(s)
	s.signalDone()
	s.deliverWG.Wait()
	s.finishClose()
}

// signalDone closes [Subscription.done] so any in-flight deliver call
// blocked on the inbox observes the closure and bails. Idempotent.
func (s *Subscription) signalDone() {
	s.doneOnce.Do(func() { close(s.done) })
}

// finishClose closes the inbox channel. Idempotent; coordinated with
// the bus-shutdown path so concurrent close calls do not race.
func (s *Subscription) finishClose() {
	s.closeOnce.Do(func() { close(s.inbox) })
}

// deliver dispatches one event to this subscriber per its configured
// policy. The bus calls this from its publisher goroutine, holding
// this subscription's [Subscription.deliverWG] for the duration;
// deliver's defer decrements the wait-group on return so
// [Subscription.Close] and bus close can wait for in-flight
// deliveries to drain — per-subscription so one subscription's
// shutdown is not coupled to another's.
//
// Policy resolution:
//
//   - Streaming events (AgentToken, AgentTurnUsage, AgentInputEstimate)
//     use [SubscribeOptions.OnStreamingFull].
//   - Every other event type is control; uses [SubscribeOptions.OnControlFull].
//   - [DefaultPolicy] resolves to [Drop] for streaming, [Block] for
//     control — matching legacy single-consumer-channel semantics.
//
// On [Block] the send blocks until the inbox accepts OR
// [Subscription.done] is closed (caller initiated Close OR bus
// shutdown). On [Drop] the send is non-blocking; a full inbox
// increments [Subscription.drops] and returns.
func (s *Subscription) deliver(ev event.Event) {
	defer s.deliverWG.Done()

	streaming := isStreamingEvent(ev)
	policy := s.opts.OnControlFull
	if streaming {
		policy = s.opts.OnStreamingFull
	}
	if policy == DefaultPolicy {
		if streaming {
			policy = Drop
		} else {
			policy = Block
		}
	}

	switch policy {
	case Block:
		select {
		case s.inbox <- ev:
		case <-s.done:
		}
	case Drop:
		select {
		case s.inbox <- ev:
		case <-s.done:
		default:
			atomic.AddInt64(&s.drops, 1)
		}
	}
}

// isStreamingEvent returns true for the high-volume, individually
// replaceable event types whose loss is recoverable from later
// events. Every other event is control: lifecycle, failure, or
// signal events whose loss would strand the subscriber.
//
// This is the single drop policy for the bus; a new high-volume
// event type must be added here to be droppable under back-pressure.
func isStreamingEvent(ev event.Event) bool {
	switch ev.(type) {
	case event.AgentToken, event.AgentTurnUsage, event.AgentInputEstimate:
		return true
	default:
		return false
	}
}
