package contracttest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/nib/ai/llm"
)

// Provider runs the [llm.Provider] contract suite against providers
// returned by ctor.
//
// Contract preconditions on the ctor'd provider:
//
//   - Stream(ctx, nil, nil) must succeed on a fresh provider and emit
//     a stream that terminates in a [llm.StreamEvent] with Done=true.
//   - The stream may contain any number of intermediate events, but at
//     least one terminal Done=true event must be present.
//   - Repeated Stream calls on the same provider (and on fresh providers
//     from successive ctor calls) must behave identically — the fixture
//     constructs a new provider per subtest and expects the same shape.
//
// The subtests verify invariants documented on [llm.Provider] and
// [llm.StreamEvent]: channel closure on completion, channel closure on
// ctx cancel, no events emitted after Done=true, concurrent-safe Stream
// calls. Providers that violate any invariant fail with a message
// identifying the contract claim.
//
// Apply to a new provider implementation by adding a single test:
//
//	func TestMyProviderContract(t *testing.T) {
//	    contracttest.Provider(t, func() llm.Provider {
//	        return myprovider.New(...)
//	    })
//	}
func Provider(t *testing.T, ctor func() llm.Provider) {
	t.Helper()
	if ctor == nil {
		t.Fatal("contracttest.Provider: ctor must be non-nil")
	}

	t.Run("ChannelEmitsDoneAndCloses", func(t *testing.T) {
		t.Parallel()
		p := ctor()
		ch, err := p.Stream(context.Background(), nil, nil)
		if err != nil {
			t.Fatalf("contract: Stream returned unexpected error on happy path: %v", err)
		}
		if ch == nil {
			t.Fatal("contract: Stream returned nil channel without error")
		}
		evs := drain(t, ch, 2*time.Second)
		if len(evs) == 0 {
			t.Fatal("contract: stream closed without emitting any events")
		}
		if !evs[len(evs)-1].Done {
			t.Fatalf("contract: last emitted event must have Done=true (got %+v)", evs[len(evs)-1])
		}
	})

	t.Run("NoEventsAfterDone", func(t *testing.T) {
		t.Parallel()
		p := ctor()
		ch, err := p.Stream(context.Background(), nil, nil)
		if err != nil {
			t.Fatalf("contract: Stream returned unexpected error: %v", err)
		}
		evs := drain(t, ch, 2*time.Second)
		for i, ev := range evs {
			if ev.Done && i != len(evs)-1 {
				t.Fatalf("contract: Done=true emitted at index %d but %d more events followed", i, len(evs)-i-1)
			}
		}
	})

	t.Run("ChannelClosesOnCtxCancel", func(t *testing.T) {
		t.Parallel()
		p := ctor()
		ctx, cancel := context.WithCancel(context.Background())
		ch, err := p.Stream(ctx, nil, nil)
		if err != nil {
			t.Fatalf("contract: Stream returned unexpected error: %v", err)
		}
		cancel()
		// Drain until close. Implementations that buffer the entire
		// stream into the channel before returning close synchronously
		// regardless of cancel — that is also conforming. The
		// invariant is "channel closes in finite time after either
		// completion or cancel."
		deadline := time.After(2 * time.Second)
		for {
			select {
			case _, ok := <-ch:
				if !ok {
					return
				}
			case <-deadline:
				t.Fatal("contract: channel did not close within 2s after ctx cancel")
			}
		}
	})

	t.Run("ConcurrentStreamCallsSafe", func(t *testing.T) {
		t.Parallel()
		p := ctor()
		const n = 4
		var wg sync.WaitGroup
		wg.Add(n)
		errs := make(chan error, n)
		for range n {
			go func() {
				defer wg.Done()
				ch, err := p.Stream(context.Background(), nil, nil)
				if err != nil {
					errs <- err
					return
				}
				// Drain so the provider can clean up.
				for range ch {
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("contract: concurrent Stream call errored: %v", err)
			}
		}
	})

	t.Run("TruncatedEventNeverCarriesExecutableToolCalls", func(t *testing.T) {
		t.Parallel()
		// Per [llm.StreamEvent.Truncated]'s contract, ToolCalls on a
		// Truncated=true event may have incomplete arguments and "must
		// not be executed." The fixture cannot force a truncation on
		// an arbitrary provider, so this subtest only verifies that
		// IF a Truncated=true event appears on the happy-path stream,
		// the surrounding invariant holds: Truncated implies Done
		// (truncation is a terminal condition). Providers that never
		// emit Truncated=true (the common case) pass trivially.
		p := ctor()
		ch, err := p.Stream(context.Background(), nil, nil)
		if err != nil {
			t.Fatalf("contract: Stream returned unexpected error: %v", err)
		}
		evs := drain(t, ch, 2*time.Second)
		for _, ev := range evs {
			if ev.Truncated && !ev.Done {
				t.Fatalf("contract: Truncated=true event must also have Done=true (got %+v)", ev)
			}
		}
	})
}

// drain reads all events from ch until it closes or the deadline
// elapses. Failing on timeout signals the provider never closed its
// channel, which violates the [llm.Provider.Stream] contract.
func drain(t *testing.T, ch <-chan llm.StreamEvent, timeout time.Duration) []llm.StreamEvent {
	t.Helper()
	out := make([]llm.StreamEvent, 0, 8)
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("contract: provider did not close channel within %s (drained %d events so far)", timeout, len(out))
			return out
		}
	}
}
