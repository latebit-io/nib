package kit

import (
	"github.com/latebit-io/nib/kit/event"
	"github.com/latebit-io/nib/kit/eventbus"
)

// DropPolicy names how the bus reacts when a subscriber's inbox is full for
// an event of a given class. [Block] preserves delivery by applying
// backpressure; [Drop] preserves publisher progress and increments
// [Subscription.Drops].
//
// The zero value resolves to [Drop] for [event.AgentToken],
// [event.AgentTurnUsage], and [event.AgentInputEstimate]. It resolves to
// [Block] for every other event.
type DropPolicy int

const (
	// DefaultPolicy resolves to [Drop] for streaming events and [Block] for
	// control events.
	DefaultPolicy = DropPolicy(eventbus.DefaultPolicy)

	// Block waits until the subscriber inbox accepts the event.
	Block = DropPolicy(eventbus.Block)

	// Drop discards the event and increments [Subscription.Drops] when the
	// subscriber inbox cannot accept it immediately.
	Drop = DropPolicy(eventbus.Drop)
)

// SubscribeOptions configures one subscription's inbox and backpressure.
// The zero value uses a buffer of 64, drops streaming events on a full inbox,
// and blocks control-event publication until the inbox accepts. Subscribers
// needing lossless transcript events can select [Block] for streaming;
// best-effort probes can select [Drop] for control events.
type SubscribeOptions struct {
	// BufferSize is the subscriber inbox capacity. Values below one use 64.
	BufferSize int

	// OnStreamingFull controls delivery when AgentToken, AgentTurnUsage, or
	// AgentInputEstimate would block. Zero resolves to Drop.
	OnStreamingFull DropPolicy

	// OnControlFull controls delivery for every other event. Zero resolves
	// to Block.
	OnControlFull DropPolicy
}

// Subscription is one subscriber's view of the agent's event stream.
// Construct through [Agent.Subscribe] and release with [Subscription.Close].
// Its inbox closes after Subscription.Close or parent [Agent.Close], and
// buffered events remain readable. Concurrent Close calls are safe. A
// Subscription must not be copied after first use.
type Subscription struct {
	inner *eventbus.Subscription
}

// Events returns the subscription inbox. The channel closes after this
// subscription or its parent [Agent] closes and buffered events drain.
func (s *Subscription) Events() <-chan event.Event {
	return s.inner.Events()
}

// Drops returns the monotonic number of events discarded under [Drop].
func (s *Subscription) Drops() int64 {
	return s.inner.Drops()
}

// Close unregisters and releases the subscription. It waits only for this
// subscription's in-flight deliveries, never another subscriber's. Close is
// safe for concurrent and repeated calls.
func (s *Subscription) Close() {
	s.inner.Close()
}

func toEventBusOptions(opts SubscribeOptions) eventbus.SubscribeOptions {
	return eventbus.SubscribeOptions{
		BufferSize:      opts.BufferSize,
		OnStreamingFull: eventbus.DropPolicy(opts.OnStreamingFull),
		OnControlFull:   eventbus.DropPolicy(opts.OnControlFull),
	}
}

func isStreamingEvent(ev event.Event) bool {
	switch ev.(type) {
	case event.AgentToken, event.AgentTurnUsage, event.AgentInputEstimate:
		return true
	default:
		return false
	}
}
