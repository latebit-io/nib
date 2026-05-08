package session

import (
	"context"
	"log/slog"
	"time"

	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/engine/capture"
)

// Capture-sink wiring — receives session events (intent, proposal, accepted,
// rejected, validator) for downstream observers. The sink is installed once
// at composition; emission is best-effort and never blocks the session hot
// path. State (sink, sessionID) lives on Session.

// SetEventSink installs the capture sink that receives session events
// (intent, proposal, accepted, rejected, validator). Pass the live
// adapter at the composition root; passing nil resets to the no-op sink
// so every internal call site can dispatch without nil checks.
//
// Safe to call once during wiring; not designed for hot-path swaps.
func (s *Session) SetEventSink(sink capture.SessionEventSink) {
	if sink == nil {
		sink = capture.NoopSink{}
	}
	s.sink = sink
}

// SessionID returns the identifier that groups this process's capture
// events. Adapters may use it as a per-session document path component.
func (s *Session) SessionID() string { return s.sessionID }

// validatorStagesPayload projects the frontend-facing ValidatorSummary
// slice into a JSON-serialisable form for capture payloads. Kept as a
// helper so the HandleEvent call site stays readable and future
// summary-field additions do not ripple through inline map literals.
func validatorStagesPayload(summaries []event.ValidatorSummary) []map[string]any {
	out := make([]map[string]any, 0, len(summaries))
	for _, s := range summaries {
		entry := map[string]any{
			"stage":   s.Stage,
			"verdict": s.Verdict,
		}
		if s.Feedback != "" {
			entry["feedback"] = s.Feedback
		}
		out = append(out, entry)
	}
	return out
}

// emitCapture fires a capture event through the configured sink. Errors
// are logged at warn level — capture failures must never block the
// session's hot path or surface to the developer.
func (s *Session) emitCapture(kind string, payload map[string]any) {
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ev := capture.Event{
		Kind:      kind,
		Timestamp: time.Now(),
		SessionID: s.sessionID,
		Payload:   payload,
	}
	if err := s.sink.Append(ctx, ev); err != nil {
		slog.Warn("session capture append failed", "kind", kind, "err", err)
	}
}
