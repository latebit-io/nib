package session

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/junto/engine/buffer"
	"github.com/latebit-io/junto/engine/editor"
)

// fakeHighlighter records Close calls so tests can verify the old
// highlighter is released when the factory is swapped.
type fakeHighlighter struct {
	closed int
}

func (f *fakeHighlighter) Parse(string)                     {}
func (f *fakeHighlighter) HighlightLine(int) []editor.Token { return nil }
func (f *fakeHighlighter) Close()                           { f.closed++ }

// newTrackingFactory returns a HighlighterFactory that records every file
// path it's asked to highlight, and the fake highlighters it produced so
// tests can assert Close behavior on swap.
func newTrackingFactory() (editor.HighlighterFactory, *[]string, *[]*fakeHighlighter) {
	var paths []string
	var made []*fakeHighlighter
	fn := func(path string) editor.Highlighter {
		paths = append(paths, path)
		h := &fakeHighlighter{}
		made = append(made, h)
		return h
	}
	return fn, &paths, &made
}

// buildSessionWithOpenFile returns a Session with one editor whose buffer
// has a path (so the factory would decorate it) registered in s.editors.
func buildSessionWithOpenFile(t *testing.T, path string) *Session {
	t.Helper()
	buf := buffer.New()
	buf.Path = path
	e := editor.New(buf)
	s := New(e, "/tmp")
	// New() only registers the initial editor if the path resolves; force
	// the map entry so the test is independent of that resolution.
	s.editors[path] = e
	return s
}

func TestSetHighlighterFactory_DecoratesOpenEditors(t *testing.T) {
	s := buildSessionWithOpenFile(t, "/tmp/a.go")
	fn, paths, _ := newTrackingFactory()
	s.SetHighlighterFactory(fn)

	if len(*paths) != 1 || (*paths)[0] != "/tmp/a.go" {
		t.Errorf("factory should have been called for the open editor; paths=%v", *paths)
	}
}

func TestSetHighlighterFactory_NilClearsAndClosesOpenEditors(t *testing.T) {
	s := buildSessionWithOpenFile(t, "/tmp/a.go")
	fn, _, made := newTrackingFactory()
	s.SetHighlighterFactory(fn)

	if len(*made) != 1 {
		t.Fatalf("expected one highlighter constructed, got %d", len(*made))
	}
	original := (*made)[0]

	// Swap to nil — must clear and close the installed highlighter.
	s.SetHighlighterFactory(nil)
	if original.closed != 1 {
		t.Errorf("nil factory did not close the previous highlighter (closed=%d)", original.closed)
	}
}

func TestSetHighlighterFactory_SwapClosesOld(t *testing.T) {
	s := buildSessionWithOpenFile(t, "/tmp/a.go")
	fn1, _, made1 := newTrackingFactory()
	s.SetHighlighterFactory(fn1)

	fn2, paths2, _ := newTrackingFactory()
	s.SetHighlighterFactory(fn2)

	if (*made1)[0].closed != 1 {
		t.Errorf("old highlighter not closed on swap")
	}
	if len(*paths2) != 1 || (*paths2)[0] != "/tmp/a.go" {
		t.Errorf("new factory not invoked for open editor; paths=%v", *paths2)
	}
}

// TestDecorateEditor_LinearizableWithSwaps hammers SetHighlighterFactory
// concurrently with decorateEditor. Linearizability is verified via close
// observations, not by introspecting the editor's installed highlighter:
// if the implementation is linearizable, exactly one highlighter instance
// remains open at the end (the most recently installed one), and every
// older instance has been closed by SetHighlighter's replace path.
//
// Run under `go test -race` to catch data races on the shared factory/gen
// state and on the Editor.highlighter field.
func TestDecorateEditor_LinearizableWithSwaps(t *testing.T) {
	s := buildSessionWithOpenFile(t, "/tmp/a.go")

	type trackingHighlighter struct {
		fakeHighlighter
		id int64
	}

	var made []*trackingHighlighter
	var madeMu sync.Mutex

	newFactory := func(id int64) editor.HighlighterFactory {
		return func(string) editor.Highlighter {
			h := &trackingHighlighter{id: id}
			madeMu.Lock()
			made = append(made, h)
			madeMu.Unlock()
			return h
		}
	}

	var wg sync.WaitGroup
	const swaps = 200
	const decorations = 200

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := int64(1); i <= swaps; i++ {
			s.SetHighlighterFactory(newFactory(i))
		}
	}()

	e := s.editors["/tmp/a.go"]
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range decorations {
			s.decorateEditor(e)
		}
	}()

	wg.Wait()

	// Pin the final state: one more install from a known factory ID. The
	// test asserts on this deterministic final install.
	final := int64(swaps + 1)
	s.SetHighlighterFactory(newFactory(final))
	s.decorateEditor(e)

	// Close the editor to release the last installed highlighter —
	// otherwise the "exactly one alive" check is off by one.
	e.Close()

	madeMu.Lock()
	defer madeMu.Unlock()
	var alive []*trackingHighlighter
	for _, h := range made {
		if h.closed == 0 {
			alive = append(alive, h)
		}
	}
	if len(alive) != 0 {
		ids := make([]int64, len(alive))
		for i, h := range alive {
			ids[i] = h.id
		}
		t.Errorf("expected all highlighters closed after e.Close(); %d leaked (ids=%v)",
			len(alive), ids)
	}
	// Additionally, every highlighter must have been closed exactly once:
	// intermediates closed by SetHighlighter's replace path, and the final
	// one closed by e.Close() above. A count other than 1 indicates a
	// double-close (Close called twice on the same instance) or a leak
	// (an instance never replaced and never reached by e.Close).
	for _, h := range made {
		if h.closed != 1 {
			t.Errorf("highlighter id=%d closed %d times; want exactly 1", h.id, h.closed)
		}
	}
}

// instrumentedHighlighter detects UAF-style overlaps between HighlightLine
// and Close. HighlightLine bumps inUse around its work (and sleeps briefly
// to widen the overlap window so swaps are likely to land mid-call); Close
// asserts inUse == 0 or fails the test. It also writes to a shared field
// (sentinel) so `go test -race` flags a data race if HighlightLine and
// Close run concurrently.
type instrumentedHighlighter struct {
	inUse    atomic.Int32
	violated atomic.Bool
	// sentinel is written by Close and read by HighlightLine. Under -race,
	// concurrent access without external synchronization produces a report.
	sentinel int
}

func (h *instrumentedHighlighter) Parse(string) {}

func (h *instrumentedHighlighter) HighlightLine(int) []editor.Token {
	h.inUse.Add(1)
	defer h.inUse.Add(-1)
	// Read the shared field and spend a moment here to widen the window.
	_ = h.sentinel
	time.Sleep(5 * time.Microsecond)
	return nil
}

func (h *instrumentedHighlighter) Close() {
	if h.inUse.Load() > 0 {
		h.violated.Store(true)
	}
	h.sentinel++ // race detector will flag this against HighlightLine's read
}

// TestEditor_HighlightLineRaceWithSetHighlighter stresses the UAF window:
// one goroutine calls HighlightLine in a tight loop (render path), another
// swaps+closes the highlighter. The instrumented fake fails the test if
// Close is ever called while HighlightLine is mid-flight; -race catches
// any data race on the shared sentinel field.
func TestEditor_HighlightLineRaceWithSetHighlighter(t *testing.T) {
	buf := buffer.New()
	buf.Path = "/tmp/race.go"
	buf.Insert(0, 0, "package main\nfunc main() {}\n")
	e := editor.New(buf)

	var made []*instrumentedHighlighter
	var madeMu sync.Mutex
	newInstrumented := func() editor.Highlighter {
		h := &instrumentedHighlighter{}
		madeMu.Lock()
		made = append(made, h)
		madeMu.Unlock()
		return h
	}

	e.SetHighlighter(newInstrumented())

	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = e.HighlightLine(0)
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 500 {
			e.SetHighlighter(newInstrumented())
		}
		close(stop)
	}()

	wg.Wait()
	e.Close()

	madeMu.Lock()
	defer madeMu.Unlock()
	for i, h := range made {
		if h.violated.Load() {
			t.Errorf("highlighter #%d: Close() overlapped with HighlightLine()", i)
		}
	}
}

func TestEditor_SetSameHighlighterIsSafe(t *testing.T) {
	// Installing the already-attached highlighter must not close it.
	// Prior behavior: Close() then reassign → next Parse/HighlightLine
	// call went through a closed tree-sitter instance. Observed via
	// close count: self-reinstall must leave closed == 0.
	buf := buffer.New()
	buf.Path = "/tmp/same.go"
	e := editor.New(buf)
	h := &fakeHighlighter{}
	e.SetHighlighter(h)

	e.SetHighlighter(h) // self-reinstall — must no-op

	if h.closed != 0 {
		t.Errorf("same-instance reinstall should not close highlighter; closed=%d", h.closed)
	}
	// Closing the editor now should close the highlighter exactly once —
	// confirming it was still the active instance (not replaced).
	e.Close()
	if h.closed != 1 {
		t.Errorf("expected highlighter to close exactly once on editor Close; got closed=%d", h.closed)
	}
}

func TestSetHighlighterFactory_SkipsPathlessEditors(t *testing.T) {
	// Scratch buffer (no path) must be silently skipped — SetHighlighter
	// would be a no-op anyway, but the factory shouldn't be invoked with
	// an empty path.
	buf := buffer.New()
	e := editor.New(buf)
	s := New(e, "/tmp")
	s.editors[""] = e

	fn, paths, _ := newTrackingFactory()
	s.SetHighlighterFactory(fn)

	if len(*paths) != 0 {
		t.Errorf("factory should not be called for path-less editor; paths=%v", *paths)
	}
}
