// Package capture defines the port for persisting session events (intents,
// proposals, validator outcomes, accept/reject decisions) to external stores.
//
// The engine depends only on the SessionEventSink interface defined here.
// Concrete adapters (e.g. engine/capture/demarkus) live in sub-packages and
// are wired at the composition root. A NoopSink satisfies the null-object
// pattern so every call site dispatches unconditionally without nil checks.
package capture

import (
	"context"
	"time"
)

// Event is a single structured record of something that happened in a session.
//
// Payload is deliberately map[string]any so the engine never leaks internal
// types (event.PendingEdit, validate.Result, and so on) across the capture
// boundary. Adapters are responsible for serialising payloads to their wire
// format.
type Event struct {
	// Kind names the event category. Reserved kinds used by the engine:
	// "intent", "proposal", "validator", "accepted", "rejected", "continue".
	Kind string

	// Timestamp is the wall-clock time the event occurred.
	Timestamp time.Time

	// SessionID identifies the Junto process-lifetime session this event
	// belongs to. Adapters use it to group or route events (e.g. into a
	// per-session document path).
	SessionID string

	// Payload carries kind-specific fields as JSON-serialisable values.
	// Adapters should treat unknown keys as opaque and pass them through.
	Payload map[string]any
}

// SessionEventSink is the port through which the session emits capture events.
//
// Implementations must be safe for concurrent calls from the session
// goroutine and any agent-event dispatch goroutine. Append must not block
// the hot path — adapters backed by RPC should buffer internally and drop
// (with a warning log) on overflow rather than stall the session.
//
// Ownership contract: Append is expected to snapshot the event before
// returning, so callers can safely let Event.Payload fall out of scope
// or even mutate it after Append returns. Adapters that persist events
// asynchronously MUST deep-copy Payload (and any nested map/slice it
// references) before releasing the caller, otherwise a post-return
// mutation races with the dispatch goroutine. The NoopSink and the
// demarkus Sink both honour this contract.
type SessionEventSink interface {
	// Append records an event. Returns an error only on unrecoverable sink
	// failure; transient issues (buffer full, RPC retry) should be logged
	// inside the adapter and not surfaced to callers. See the interface
	// docstring for the payload ownership contract.
	Append(ctx context.Context, e Event) error

	// Close flushes pending events and releases resources. Callers invoke
	// it once at application shutdown; calling Append after Close is a
	// programmer error and may drop events silently but must not panic.
	Close(ctx context.Context) error
}

// Redactor transforms an event before it is persisted. Adapters apply the
// configured redactor at the wire boundary so sensitive payload fields can
// be elided or size-capped without changing the shape of the event upstream.
//
// The default redactor is the identity function; adapters that take a nil
// Redactor must treat it as identity.
type Redactor func(Event) Event

// NoopSink is the null-object implementation of SessionEventSink. It is the
// default sink installed by [session.New] so call sites can dispatch without
// nil checks; production deployments swap in a live adapter via
// [session.Session.SetEventSink] at the composition root.
type NoopSink struct{}

// Append discards the event and returns nil.
func (NoopSink) Append(context.Context, Event) error { return nil }

// Close returns nil; NoopSink owns no resources.
func (NoopSink) Close(context.Context) error { return nil }
