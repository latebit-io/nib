// Package approval owns the coordination channels between the agent
// and its frontend. The agent blocks on these channels for edit
// approval, post-approval continue, conversational replies between
// turns, and (currently dormant) request_input answers; the frontend
// drives the channels via the corresponding non-blocking signal
// methods.
//
// Splitting the channels out of [Agent] keeps the agent's run loop
// focused on conversational state, validation, budget, and event
// emission — coordination plumbing has its own home where the drain
// semantics, capacity choices, and ctx-cancellable awaits can be
// reviewed and tested without standing up a full agent. The agent
// retains the public API (Approve / Reject / Continue / AnswerInput /
// Reply); those methods now delegate to a [Coordinator] so the channels
// stay private to this package.
package approval

import (
	"context"
	"errors"
	"log/slog"
)

// Coordinator owns the agent <-> frontend coordination channels.
//
// Channel capacities are deliberately 1, and the signal methods
// (Approve / Reject / Continue / Answer / Reply) are non-blocking
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
//     Answer/Reply). One pending signal per channel is meaningful;
//     a duplicate while the first is still queued is a redundant
//     keystroke or a frontend bug, not a queue we want to grow.
//
// Stale-signal isolation across runs is the caller's job: Agent
// allocates a fresh Coordinator at every RunWithMode / Reply-resume
// rather than reusing one across the run boundary, so a queued
// signal from a previous run cannot leak into the next.
// Reset (drain in place) is retained for tests and for callers that
// want to clear stale state without replacing the Coordinator.
type Coordinator struct {
	approveCh  chan bool   // true=approved, false=rejected
	continueCh chan string // post-approval buffer content
	inputCh    chan string // developer reply between turns
	answerCh   chan string // request_input answer (currently dormant)
}

// New returns a Coordinator with all four channels allocated. Safe to
// use immediately; callers should retain the returned pointer for the
// lifetime of the agent.
func New() *Coordinator {
	return &Coordinator{
		approveCh:  make(chan bool, 1),
		continueCh: make(chan string, 1),
		inputCh:    make(chan string, 1),
		answerCh:   make(chan string, 1),
	}
}

// Reset drains any pending values on every channel. Call at run
// boundaries (RunWithMode, Reply resume) so a stale signal from a
// previous run cannot leak into the next.
func (c *Coordinator) Reset() {
	drain(c.approveCh)
	drain(c.continueCh)
	drain(c.inputCh)
	drain(c.answerCh)
}

// Approve signals approval for the pending edit. Non-blocking: if the
// approve buffer is empty, the value queues for the next AwaitApproval
// caller; if the buffer already holds a pending signal, the new value
// is dropped (the existing queued signal is the one that will be
// delivered).
func (c *Coordinator) Approve() {
	select {
	case c.approveCh <- true:
	default:
	}
}

// Reject signals rejection for the pending edit. Same buffered-send
// semantics as Approve: queues into an empty buffer, drops on a full
// buffer.
func (c *Coordinator) Reject() {
	select {
	case c.approveCh <- false:
	default:
	}
}

// Continue delivers the post-approval buffer content to the agent.
// Same buffered-send semantics as Approve. The path/cache concerns
// are the agent's; this method only transports the new content.
func (c *Coordinator) Continue(content string) {
	select {
	case c.continueCh <- content:
	default:
	}
}

// Answer delivers the developer's typed answer to a pending
// request_input prompt. Same buffered-send semantics as Approve, but
// a full-buffer drop is logged at warn level — for the answer flow a
// dropped value is more often a frontend bug than an intentional
// no-op, since request_input prompts are emitted explicitly by the
// agent rather than implied by an editor approval gesture.
//
// The request_input tool is currently unregistered in the agent (see
// agent composition); the channel and method are kept so the surface
// stays defensive and a future re-introduction has a wire ready.
func (c *Coordinator) Answer(text string) {
	select {
	case c.answerCh <- text:
	default:
		slog.Warn("approval.Answer: answer buffer full, dropping answer")
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
// canceled, or the channel is closed. Returns the approval bool when
// a value arrived, ctx.Err() on cancellation, or [ErrChannelClosed]
// when the channel was closed (not expected under normal operation).
func (c *Coordinator) AwaitApproval(ctx context.Context) (bool, error) {
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case approved, ok := <-c.approveCh:
		if !ok {
			return false, ErrChannelClosed
		}
		return approved, nil
	}
}

// AwaitContinue blocks until Continue is signaled, ctx is canceled,
// or the channel is closed. The returned content is the post-
// approval buffer state delivered by the frontend.
func (c *Coordinator) AwaitContinue(ctx context.Context) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case content, ok := <-c.continueCh:
		if !ok {
			return "", ErrChannelClosed
		}
		return content, nil
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

// AwaitAnswer blocks until Answer is signaled for a pending
// request_input prompt, ctx is canceled, or the channel is closed.
// Currently unused by the live agent; preserved for the dormant
// request_input flow.
func (c *Coordinator) AwaitAnswer(ctx context.Context) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case text, ok := <-c.answerCh:
		if !ok {
			return "", ErrChannelClosed
		}
		return text, nil
	}
}

// drain reads off any pending values without blocking.
func drain[T any](ch chan T) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
