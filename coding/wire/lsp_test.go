package wire

import (
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/nib/coding/event"
	engineevent "github.com/latebit-io/nib/engine/event"
	"github.com/latebit-io/nib/engine/lsp"
)

// TestFanInEngineEvents_ExitsOnChannelClose pins the leak fix that
// CodeRabbit flagged: the fan-in goroutine ranges over the engine
// event channel and must terminate when that channel closes. Without
// the wire-layer Close that shuts the channel, the goroutine would
// run forever on every test or program run.
func TestFanInEngineEvents_ExitsOnChannelClose(t *testing.T) {
	t.Parallel()

	in := make(chan engineevent.Event, 4)
	out := make(chan event.Event, 4)

	done := make(chan struct{})
	go func() {
		fanInEngineEvents(in, out)
		close(done)
	}()

	in <- engineevent.DiagnosticsUpdated{Path: "x.go"}

	select {
	case ev := <-out:
		if got, ok := ev.(event.DiagnosticsUpdated); !ok || got.Path != "x.go" {
			t.Fatalf("forwarded event = %#v, want event.DiagnosticsUpdated{Path:\"x.go\"}", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("fan-in did not forward event within 1s")
	}

	close(in)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("fan-in goroutine did not exit within 1s of channel close")
	}
}

// TestFanInEngineEvents_DropsOnFullOutChannel locks in the
// drop-on-full semantics that mirror engine/lsp.Manager's existing
// behaviour. A blocking send here would let a stalled consumer
// freeze shutdown — the engine channel could fill, the manager's
// drop-on-full would catch the upstream side, but the fan-in
// goroutine would still hang on its own send and never observe the
// close that wire issues during teardown.
func TestFanInEngineEvents_DropsOnFullOutChannel(t *testing.T) {
	t.Parallel()

	in := make(chan engineevent.Event, 4)
	out := make(chan event.Event) // unbuffered, no reader → every send drops

	done := make(chan struct{})
	go func() {
		fanInEngineEvents(in, out)
		close(done)
	}()

	for i := 0; i < 10; i++ {
		in <- engineevent.DiagnosticsUpdated{Path: "x.go"}
	}
	close(in)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("fan-in goroutine blocked on full out channel; did not exit after close")
	}
}

// TestLSPWithCleanup_CloseIsIdempotent locks in the sync.Once guard
// on Close so a defensive double-Close from a caller (e.g. an outer
// shutdown sequence retrying on error) does not panic by closing
// the engine event channel twice.
func TestLSPWithCleanup_CloseIsIdempotent(t *testing.T) {
	t.Parallel()

	engineEvents := make(chan engineevent.Event, 1)
	w := &lspWithCleanup{
		Manager:      lsp.NewManager(nil, ".", engineEvents),
		engineEvents: engineEvents,
	}

	if err := w.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	// Channel must actually be closed — receive on a closed channel
	// returns the zero value with ok=false. Sanity-check both that we
	// observe the close and that no panic escaped from the second
	// Close above.
	select {
	case _, ok := <-engineEvents:
		if ok {
			t.Fatal("engineEvents channel not closed")
		}
	default:
		t.Fatal("engineEvents channel still open after Close")
	}
}

// TestLSPWithCleanup_CloseRaceSafe pins the close-once invariant
// against concurrent callers — any path that calls Close from
// multiple goroutines (defer chain plus panic-recovery cleanup, for
// example) must not double-close the channel.
func TestLSPWithCleanup_CloseRaceSafe(t *testing.T) {
	t.Parallel()

	engineEvents := make(chan engineevent.Event, 1)
	w := &lspWithCleanup{
		Manager:      lsp.NewManager(nil, ".", engineEvents),
		engineEvents: engineEvents,
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = w.Close()
		}()
	}
	wg.Wait()
}
