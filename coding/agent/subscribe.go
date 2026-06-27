package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/latebit-io/nib/coding/event"
)

// ErrAgentClosed is returned by [Agent.Subscribe] after [Agent.Close]
// has been called. Once Close completes, the bus is shut down and
// no further subscriptions can be registered.
var ErrAgentClosed = errors.New("coding/agent: agent is closed")

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
// [Drop] for streaming events ([event.AgentToken], [event.AgentStatus],
// [event.AgentTurnUsage], [event.AgentInputEstimate],
// [event.AgentCompacted]) and to [Block] for every other event. This
// matches the legacy [Agent.send] classification — streaming events
// drop on a full inbox; control events block (up to the publisher's
// timeout) until the inbox accepts.
type DropPolicy int

const (
	// DefaultPolicy resolves at deliver time to [Drop] for streaming
	// events and [Block] for control events.
	DefaultPolicy DropPolicy = iota

	// Block makes the publisher block until the subscriber's inbox
	// accepts the event.
	Block

	// Drop makes the publisher discard the event and increment the
	// subscriber's drop counter.
	Drop
)

// SubscribeOptions configures a single subscription's inbox buffer
// size and per-event-class delivery policy.
//
// Zero value means "library defaults" — BufferSize 64, OnStreamingFull
// [DefaultPolicy] (resolves to [Drop]), OnControlFull [DefaultPolicy]
// (resolves to [Block]). These defaults preserve the pre-bus channel
// behavior on every consumer that migrates to Subscribe without
// customizing the options.
type SubscribeOptions struct {
	// BufferSize is the subscriber inbox channel capacity. Zero or
	// negative means the default (64).
	BufferSize int

	// OnStreamingFull is the policy when the inbox would block on a
	// streaming event. Zero value is [DefaultPolicy] → [Drop].
	OnStreamingFull DropPolicy

	// OnControlFull is the policy when the inbox would block on a
	// control event. Zero value is [DefaultPolicy] → [Block].
	OnControlFull DropPolicy
}

// Subscription is one subscriber's view of the [Agent]'s event stream.
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
	// — mirrors the discipline learned the hard way in kit's bus
	// (PR #151 Greptile P1): a Close on subscription A must not block
	// on a deliver-in-flight to subscription B.
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
// subscription's inbox due to a configured [Drop] policy finding the
// inbox full.
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
// [Subscription.done]). Idempotent.
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

// effectivePolicy returns the concrete [Block] or [Drop] policy that
// applies to ev under this subscription's [SubscribeOptions]. A
// [DefaultPolicy] field on the options is resolved here per the
// class-sensitive rule documented on [DropPolicy]: streaming → Drop,
// control → Block.
//
// Shared by [Subscription.deliver] and [Subscription.deliverContext]
// so a future change to the resolution rule is a single-site edit.
func (s *Subscription) effectivePolicy(ev event.Event) DropPolicy {
	streaming := isStreamingEvent(ev)
	policy := s.opts.OnControlFull
	if streaming {
		policy = s.opts.OnStreamingFull
	}
	if policy == DefaultPolicy {
		if streaming {
			return Drop
		}
		return Block
	}
	return policy
}

// deliver dispatches one event to this subscriber per its configured
// policy. The bus calls this from its publisher path, holding this
// subscription's [Subscription.deliverWG] for the duration; deliver's
// defer decrements the wait-group on return so [Subscription.Close]
// and bus close can wait for in-flight deliveries to drain.
func (s *Subscription) deliver(ev event.Event) {
	defer s.deliverWG.Done()
	switch s.effectivePolicy(ev) {
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

// deliverContext attempts to deliver one event to this subscriber with
// ctx-bounded semantics. Drop-policy subscribers behave exactly as
// [Subscription.tryDeliver] — they are lossy by choice and ctx does
// not apply. Block-policy subscribers attempt a bounded inbox send,
// returning [context.Context.Err] if ctx fires before the inbox
// accepts.
//
// Used by [bus.publishContext] to back the post-channel-removal
// [Agent.sendCritical] path: the call site supplies a deadline ctx, and
// the bus guarantees that no individual Block-policy subscriber can
// extend delivery past that deadline. Mirrors the deadline contract
// the legacy events channel carried via its 5-second timer.
//
// Decrements the per-subscription deliverWG on return so Close/
// bus.close can wait on in-flight attempts.
func (s *Subscription) deliverContext(ctx context.Context, ev event.Event) error {
	defer s.deliverWG.Done()
	switch s.effectivePolicy(ev) {
	case Block:
		select {
		case s.inbox <- ev:
			return nil
		case <-s.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	case Drop:
		select {
		case s.inbox <- ev:
		case <-s.done:
		default:
			atomic.AddInt64(&s.drops, 1)
		}
		return nil
	}
	return nil
}

// isStreamingEvent returns true for the high-volume, individually
// replaceable event types whose loss is recoverable from later events.
// Every other event is control: lifecycle, failure, or signal events
// whose loss would strand the subscriber.
//
// This is the SINGLE source of truth for the streaming/control split.
// [Agent.send] (lifecycle.go) no longer classifies events — it just
// hands them to the bus, which resolves [DefaultPolicy] via
// [Subscription.effectivePolicy], which calls this. AgentStatus is here
// (unlike kit's narrower list) because coding emits AgentStatus on every
// Thinking/Planning/Reviewing transition, which is high-volume enough to
// deserve drop-on-full. AgentCompacted is here because compaction can
// fire repeatedly within a single run.
//
// Adding a new streaming event type is a one-line edit here; there is no
// parallel switch to keep in sync.
func isStreamingEvent(ev event.Event) bool {
	switch ev.(type) {
	case event.AgentToken,
		event.AgentStatus,
		event.AgentTurnUsage,
		event.AgentInputEstimate,
		event.AgentCompacted:
		return true
	default:
		return false
	}
}
