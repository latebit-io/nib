package contracttest

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/nib/engine/capture"
)

// SessionEventSink runs the [capture.SessionEventSink] contract suite
// against sinks returned by ctor.
//
// Contract preconditions on the ctor'd sink:
//
//   - The sink is freshly constructed and not yet closed.
//   - Calls to Append complete in finite time without blocking the
//     hot path (the contract requires non-blocking semantics on the
//     fast path — adapters that drop on overflow are conforming).
//
// Subtests verify the documented invariants on [capture.SessionEventSink]:
// safe handling of zero/populated/nil-payload events, concurrent
// Append, Append-after-Close not panicking. Each subtest constructs a
// fresh sink via ctor and registers a t.Cleanup to Close it so
// resource-holding adapters do not leak across cases.
//
// Apply to a new sink implementation by adding a single test:
//
//	func TestMySinkContract(t *testing.T) {
//	    contracttest.SessionEventSink(t, func() capture.SessionEventSink {
//	        return mysink.New(...)
//	    })
//	}
func SessionEventSink(t *testing.T, ctor func() capture.SessionEventSink) {
	t.Helper()
	if ctor == nil {
		t.Fatal("contracttest.SessionEventSink: ctor must be non-nil")
	}
	t.Run("AppendAcceptsZeroEvent", func(t *testing.T) { sinkZeroEvent(t, ctor) })
	t.Run("AppendAcceptsPopulatedEvent", func(t *testing.T) { sinkPopulatedEvent(t, ctor) })
	t.Run("AppendAcceptsNilPayload", func(t *testing.T) { sinkNilPayload(t, ctor) })
	t.Run("ConcurrentAppendSafe", func(t *testing.T) { sinkConcurrentAppend(t, ctor) })
	t.Run("PayloadSnapshottedBeforeReturn", func(t *testing.T) { sinkPayloadSnapshot(t, ctor) })
	t.Run("AppendAfterCloseDoesNotPanic", func(t *testing.T) { sinkAppendAfterClose(t, ctor) })
}

// sinkWithCleanup constructs a sink via ctor and registers a t.Cleanup
// that Closes it within a 5s deadline. Logs (via t.Errorf) but does not
// fail the test on Close error — Close failures during cleanup of an
// otherwise-passing test should not retroactively fail it.
func sinkWithCleanup(t *testing.T, ctor func() capture.SessionEventSink) capture.SessionEventSink {
	t.Helper()
	s := ctor()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Close(ctx); err != nil {
			t.Errorf("contract cleanup: Close returned error: %v", err)
		}
	})
	return s
}

func sinkZeroEvent(t *testing.T, ctor func() capture.SessionEventSink) {
	s := sinkWithCleanup(t, ctor)
	if err := s.Append(context.Background(), capture.Event{}); err != nil {
		t.Fatalf("contract: Append(zero-value event) returned error: %v", err)
	}
}

func sinkPopulatedEvent(t *testing.T, ctor func() capture.SessionEventSink) {
	s := sinkWithCleanup(t, ctor)
	ev := capture.Event{
		Kind:      "intent",
		Timestamp: time.Now(),
		SessionID: "contracttest",
		Payload:   map[string]any{"goal": "verify"},
	}
	if err := s.Append(context.Background(), ev); err != nil {
		t.Fatalf("contract: Append(populated event) returned error: %v", err)
	}
}

func sinkNilPayload(t *testing.T, ctor func() capture.SessionEventSink) {
	s := sinkWithCleanup(t, ctor)
	if err := s.Append(context.Background(), capture.Event{Kind: "x", SessionID: "y"}); err != nil {
		t.Fatalf("contract: Append(nil-payload event) returned error: %v", err)
	}
}

func sinkConcurrentAppend(t *testing.T, ctor func() capture.SessionEventSink) {
	s := sinkWithCleanup(t, ctor)
	const n = 16
	var wg sync.WaitGroup
	wg.Add(n)
	var failures atomic.Int32
	for i := range n {
		go func(i int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					failures.Add(1)
					t.Errorf("contract: concurrent Append panicked (goroutine %d): %v", i, r)
				}
			}()
			ev := capture.Event{
				Kind:      "stress",
				Timestamp: time.Now(),
				SessionID: "contracttest-concurrent",
				Payload:   map[string]any{"i": i},
			}
			if err := s.Append(context.Background(), ev); err != nil {
				failures.Add(1)
				t.Errorf("contract: concurrent Append (goroutine %d) returned error: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
}

// sinkPayloadSnapshot exercises the deep-copy claim: "Adapters that
// persist events asynchronously MUST deep-copy Payload (and any nested
// map/slice it references) before releasing the caller." Mutating
// Payload after Append returns must not race the adapter's async
// reader. The race detector catches violations.
func sinkPayloadSnapshot(t *testing.T, ctor func() capture.SessionEventSink) {
	s := sinkWithCleanup(t, ctor)
	payload := map[string]any{"counter": 0}
	ev := capture.Event{
		Kind:      "snapshot-test",
		Timestamp: time.Now(),
		SessionID: "contracttest-snapshot",
		Payload:   payload,
	}
	if err := s.Append(context.Background(), ev); err != nil {
		t.Fatalf("contract: Append returned error: %v", err)
	}
	for i := range 50 {
		payload["counter"] = i
	}
}

// sinkAppendAfterClose verifies the docstring claim: "calling Append
// after Close is a programmer error and may drop events silently but
// must not panic." Constructs its own sink (no Cleanup) so the Close
// here is the only Close.
func sinkAppendAfterClose(t *testing.T, ctor func() capture.SessionEventSink) {
	s := ctor()
	ctx := context.Background()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("contract: Close returned error: %v", err)
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("contract: Append after Close panicked: %v", r)
		}
	}()
	// Returned error is implementation-defined ("may drop events
	// silently") — the invariant is "must not panic." Ignore err.
	_ = s.Append(ctx, capture.Event{Kind: "post-close"})
}
