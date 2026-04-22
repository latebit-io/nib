package capture

import (
	"context"
	"testing"
	"time"
)

// TestNoopSinkContract verifies that the NoopSink upholds the
// SessionEventSink contract without panicking on zero-value, populated, or
// empty-payload events. The null-object contract is load-bearing for every
// call site in the engine, so this test is the safety net for the default
// wiring path.
func TestNoopSinkContract(t *testing.T) {
	t.Parallel()

	var s SessionEventSink = NoopSink{}

	tests := []struct {
		name string
		ev   Event
	}{
		{"zero event", Event{}},
		{"populated", Event{
			Kind:      "intent",
			Timestamp: time.Now(),
			SessionID: "abc",
			Payload:   map[string]any{"goal": "fix bug"},
		}},
		{"nil payload", Event{Kind: "intent", SessionID: "abc"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.Append(context.Background(), tc.ev); err != nil {
				t.Errorf("Append returned error: %v", err)
			}
		})
	}
	if err := s.Close(context.Background()); err != nil {
		t.Errorf("Close returned error: %v", err)
	}
}

// TestNoopSinkAppendAfterClose verifies NoopSink does not panic when Append
// is called after Close. The docstring calls this a programmer error that
// may silently drop, but it must not crash the process.
func TestNoopSinkAppendAfterClose(t *testing.T) {
	t.Parallel()

	s := NoopSink{}
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if err := s.Append(context.Background(), Event{Kind: "x"}); err != nil {
		t.Errorf("Append after Close returned error: %v", err)
	}
}

// TestRedactorIdentity verifies that an identity Redactor round-trips an
// event unchanged. Adapters that accept a nil Redactor must substitute
// identity behaviour; this test pins the expected semantics.
func TestRedactorIdentity(t *testing.T) {
	t.Parallel()

	identity := Redactor(func(e Event) Event { return e })
	in := Event{Kind: "intent", SessionID: "s1", Timestamp: time.Unix(1, 0)}
	out := identity(in)
	if out.Kind != in.Kind || out.SessionID != in.SessionID || !out.Timestamp.Equal(in.Timestamp) {
		t.Errorf("identity redactor changed event: got %+v, want %+v", out, in)
	}
}
