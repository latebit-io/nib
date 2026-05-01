// Package approval owns the coordination channels between the agent
// and its frontend. The agent blocks on these channels for edit
// approval and conversational replies between turns; the frontend
// drives the channels via the corresponding non-blocking signal
// methods.
//
// Splitting the channels out of [Agent] keeps the agent's run loop
// focused on conversational state, validation, budget, and event
// emission — coordination plumbing has its own home where the drain
// semantics, capacity choices, and ctx-cancellable awaits can be
// reviewed and tested without standing up a full agent. The agent
// retains the public API (Approve / Reject / Reply); those methods
// now delegate to a [Coordinator] so the channels stay private to
// this package.
package approval

import (
	"context"
	"errors"
	"log/slog"
)

// Approval carries the result of an approve/reject decision from the
// frontend back to the agent. Content is the post-apply buffer
// content for the edited file when Approved is true, used by the
// orchestrator to seed the file cache so subsequent tool reads
// observe the truth on disk/buffer — not the agent's predicted
// post-edit content (which can diverge from reality when the
// developer modifies the replacement text in the diff overlay
// before approving). Content is empty when Approved is false.
type Approval struct {
	Approved bool
	Content  string
}

// Coordinator owns the agent <-> frontend coordination channels.
//
// Channel capacities are deliberately 1, and the signal methods
// (Approve / Reject / Reply) are non-blocking
// `select { case ch <- v: default: }` sends. Two things follow from
// that contract:
//
//  1. A signal whose buffer is EMPTY queues — it is delivered to
//     the next Await* caller even if no goroutine is parked at the
//     moment of the signal. The headless runner relies on this:
//     after consuming an AgentEditProposed event it immediately
//     calls Approve, which can race ahead of the agent's
//     AwaitApproval; the buffered slot absorbs that race so the
//     agent picks the signal up the instant it parks.
//
//  2. A signal whose buffer is FULL is dropped (with a warning for
//     Reply). One pending signal per channel is meaningful; a
//     duplicate while the first is still queued is a redundant
//     keystroke or a frontend bug, not a queue we want to grow.
//
// Stale-signal isolation across runs is the caller's job: Agent
// allocates a fresh Coordinator at every RunWithMode / Reply-resume
// rather than reusing one across the run boundary, so a queued
// signal from a previous run cannot leak into the next.
// Reset (drain in place) is retained for tests and for callers that
// want to clear stale state without replacing the Coordinator.
type Coordinator struct {
	approveCh chan Approval
	inputCh   chan string // developer reply between turns
}

// New returns a Coordinator with both channels allocated. Safe to
// use immediately; callers should retain the returned pointer for the
// lifetime of the agent.
func New() *Coordinator {
	return &Coordinator{
		approveCh: make(chan Approval, 1),
		inputCh:   make(chan string, 1),
	}
}

// Reset drains any pending values on every channel. Call at run
// boundaries (RunWithMode, Reply resume) so a stale signal from a
// previous run cannot leak into the next.
func (c *Coordinator) Reset() {
	drain(c.approveCh)
	drain(c.inputCh)
}

// Approve signals approval for the pending edit and delivers the
// post-apply buffer content the orchestrator should seed into the
// file cache. Non-blocking: if the approve buffer is empty, the
// value queues for the next AwaitApproval caller; if the buffer
// already holds a pending signal, the new value is dropped (the
// existing queued signal is the one that will be delivered).
//
// Pass the actual buffer content after ApplyEdit, not the agent's
// predicted ExpectedContent — when the developer modifies the
// replacement text in the diff overlay before approving, those two
// diverge and the cache MUST hold the real post-apply state or
// subsequent read_file tool calls return stale data.
func (c *Coordinator) Approve(content string) {
	select {
	case c.approveCh <- Approval{Approved: true, Content: content}:
	default:
	}
}

// Reject signals rejection for the pending edit. Same buffered-send
// semantics as Approve: queues into an empty buffer, drops on a full
// buffer.
func (c *Coordinator) Reject() {
	select {
	case c.approveCh <- Approval{Approved: false}:
	default:
	}
}

// Reply enqueues the developer's next conversational message. Same
// buffered-send semantics as the other signals (queues into an empty
// buffer; the agent picks it up at the next AwaitInput), but the
// caller-visible result is exposed: returns false (with a warning)
// when the buffer is full so callers can surface a "message dropped"
// state to the developer instead of silently losing input. Matches
// the previous Agent.Reply contract where a duplicate reply during
// the same wait window is dropped rather than queued.
func (c *Coordinator) Reply(text string) bool {
	select {
	case c.inputCh <- text:
		return true
	default:
		slog.Warn("approval.Reply: inputCh full, dropping message")
		return false
	}
}

// ErrChannelClosed is returned by the Await* methods when the
// underlying channel has been closed. Closing is not part of normal
// flow — the channels are owned by the Coordinator and never closed
// during a healthy run — but the agent's wait helpers historically
// distinguished closed-channel from cancellation, and this sentinel
// preserves that distinction.
var ErrChannelClosed = errors.New("approval channel closed")

// AwaitApproval blocks until Approve or Reject is signaled, ctx is
// canceled, or the channel is closed. Returns the [Approval] (carrying
// the approved bool and post-apply content) when a value arrived,
// ctx.Err() on cancellation, or [ErrChannelClosed] when the channel
// was closed (not expected under normal operation).
func (c *Coordinator) AwaitApproval(ctx context.Context) (Approval, error) {
	select {
	case <-ctx.Done():
		return Approval{}, ctx.Err()
	case a, ok := <-c.approveCh:
		if !ok {
			return Approval{}, ErrChannelClosed
		}
		return a, nil
	}
}

// AwaitInput blocks until Reply enqueues a developer message, ctx
// is canceled, or the channel is closed.
func (c *Coordinator) AwaitInput(ctx context.Context) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case text, ok := <-c.inputCh:
		if !ok {
			return "", ErrChannelClosed
		}
		return text, nil
	}
}

// drain reads off any pending values without blocking. Returns
// promptly on a closed channel: a closed receive is always ready in
// the select arm, so without checking the comma-ok flag the loop
// would spin forever once the channel is closed. The Coordinator
// never closes its own channels — defensive guarantee for callers
// that compose drain with channels they own.
func drain[T any](ch chan T) {
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		default:
			return
		}
	}
}
