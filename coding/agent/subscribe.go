package agent

import (
	"errors"

	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/kit/eventbus"
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
// [event.AgentCompacted]) and to [Block] for every other event. Only
// [Agent.sendCritical] bounds a Block wait, with a deadline.
type DropPolicy int

const (
	// DefaultPolicy resolves at deliver time to [Drop] for streaming
	// events and [Block] for control events.
	DefaultPolicy = DropPolicy(eventbus.DefaultPolicy)

	// Block makes the publisher block until the subscriber's inbox
	// accepts the event.
	Block = DropPolicy(eventbus.Block)

	// Drop makes the publisher discard the event and increment the
	// subscriber's drop counter.
	Drop = DropPolicy(eventbus.Drop)
)

// SubscribeOptions configures a single subscription's inbox buffer
// size and per-event-class delivery policy.
//
// Zero value means "library defaults" — BufferSize 64, OnStreamingFull
// [DefaultPolicy] (resolves to [Drop]), OnControlFull [DefaultPolicy]
// (resolves to [Block]).
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
// when the subscriber no longer needs events. The bus is the sole
// writer to the subscription's inbox channel; the subscriber is the
// sole reader.
//
// Lifetime: a Subscription is live from Subscribe until Close (or
// until the parent [Agent.Close] forces it). Closing the subscription
// closes the inbox channel — `for ev := range sub.Events()` exits
// naturally after the last buffered event drains. Concurrent calls
// to Close are safe; the second and subsequent calls are no-ops.
type Subscription struct {
	inner *eventbus.Subscription
}

// Events returns the inbox channel the bus delivers events to. The
// channel is closed by [Subscription.Close] (caller-initiated) or by
// the parent [Agent.Close] (forced). After the channel is closed the
// `for ev := range sub.Events()` consumer loop exits naturally once
// the buffered tail drains.
func (s *Subscription) Events() <-chan event.Event {
	return s.inner.Events()
}

// Drops returns the monotonic count of events dropped on this
// subscription's inbox due to a configured [Drop] policy finding the
// inbox full.
func (s *Subscription) Drops() int64 {
	return s.inner.Drops()
}

// Close unsubscribes and releases the subscription. It waits only for
// in-flight deliveries to this subscription, never another
// subscriber's. Idempotent.
func (s *Subscription) Close() {
	s.inner.Close()
}

// newBus constructs the coding agent's bus with the coding-side
// streaming classifier.
func newBus() *eventbus.Bus {
	return eventbus.New(isStreamingEvent)
}

func toEventBusOptions(opts SubscribeOptions) eventbus.SubscribeOptions {
	return eventbus.SubscribeOptions{
		BufferSize:      opts.BufferSize,
		OnStreamingFull: eventbus.DropPolicy(opts.OnStreamingFull),
		OnControlFull:   eventbus.DropPolicy(opts.OnControlFull),
	}
}

// isStreamingEvent is the coding bus's streaming/control split: true for
// high-volume events whose loss later events recover. Wider than kit's:
// AgentStatus fires on every phase change and AgentCompacted can repeat
// within a run.
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
