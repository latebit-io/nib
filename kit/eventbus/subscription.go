package eventbus

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/latebit-io/nib/kit/event"
)

// DropPolicy names how publication reacts when a subscriber inbox is full.
type DropPolicy int

const (
	// DefaultPolicy resolves to [Drop] for streaming events and [Block] for
	// control events.
	DefaultPolicy DropPolicy = iota

	// Block waits until the inbox accepts the event or delivery is canceled.
	Block

	// Drop discards an event when the inbox cannot accept it immediately.
	Drop
)

// SubscribeOptions configures one subscription's buffer and backpressure.
// The zero value uses a buffer of 64, drops streaming events on a full inbox,
// and blocks control-event publication until the inbox accepts.
type SubscribeOptions struct {
	// BufferSize is the inbox capacity. Values below one use the default.
	BufferSize int

	// OnStreamingFull controls delivery when a streaming event would block.
	OnStreamingFull DropPolicy

	// OnControlFull controls delivery when a control event would block.
	OnControlFull DropPolicy
}

// Subscription is one subscriber's view of a [Bus]. Construct through
// [Bus.Subscribe] and release with [Subscription.Close]. A Subscription must
// not be copied after first use.
type Subscription struct {
	bus *Bus
	// Policies resolved from SubscribeOptions at Subscribe; never DefaultPolicy.
	streamingPolicy DropPolicy
	controlPolicy   DropPolicy

	inbox chan event.Event
	done  chan struct{}

	doneOnce  sync.Once
	closeOnce sync.Once
	deliverWG sync.WaitGroup
	drops     atomic.Int64
}

// Events returns the inbox channel. It closes after the subscription or its
// bus closes, once buffered events have drained.
func (s *Subscription) Events() <-chan event.Event {
	return s.inbox
}

// Drops returns the monotonic number of events discarded under [Drop].
func (s *Subscription) Drops() int64 {
	return s.drops.Load()
}

// Close unregisters the subscription, interrupts its blocked deliveries, and
// closes its inbox after in-flight deliveries finish. Close is idempotent.
func (s *Subscription) Close() {
	s.bus.unsubscribe(s)
	s.signalDone()
	s.deliverWG.Wait()
	s.finishClose()
}

func (s *Subscription) policy(streaming bool) DropPolicy {
	if streaming {
		return s.streamingPolicy
	}
	return s.controlPolicy
}

func (s *Subscription) deliverContext(ctx context.Context, ev event.Event, streaming bool) error {
	defer s.deliverWG.Done()
	if s.policy(streaming) == Block {
		select {
		case s.inbox <- ev:
			return nil
		case <-s.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	select {
	case s.inbox <- ev:
	case <-s.done:
	default:
		s.drops.Add(1)
	}
	return nil
}

func (s *Subscription) signalDone() {
	s.doneOnce.Do(func() { close(s.done) })
}

func (s *Subscription) finishClose() {
	s.closeOnce.Do(func() { close(s.inbox) })
}
