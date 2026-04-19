package session

import (
	"sync"
	"sync/atomic"
	"testing"

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
// concurrently with decorateEditor and asserts that the editor ends up
// carrying a highlighter built by the *final* installed factory — i.e.
// no stale factory wins the race. Run under `go test -race` to catch
// data races on the shared factory/gen state.
func TestDecorateEditor_LinearizableWithSwaps(t *testing.T) {
	s := buildSessionWithOpenFile(t, "/tmp/a.go")

	// Factories tagged with a generation id so we can recover which
	// factory the editor ended up with.
	type taggedHighlighter struct {
		fakeHighlighter
		id int64
	}
	newFactory := func(id int64) editor.HighlighterFactory {
		return func(string) editor.Highlighter {
			return &taggedHighlighter{id: id}
		}
	}

	var currentID atomic.Int64

	var wg sync.WaitGroup
	const swaps = 200
	const decorations = 200

	// Swapper goroutine: flips the factory repeatedly.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := int64(1); i <= swaps; i++ {
			currentID.Store(i)
			s.SetHighlighterFactory(newFactory(i))
		}
	}()

	// Decorator goroutine: decorates the same editor repeatedly in parallel.
	e := s.editors["/tmp/a.go"]
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range decorations {
			s.decorateEditor(e)
		}
	}()

	wg.Wait()

	// Do one final swap and decoration to pin the expected state; without
	// this, the test is racy by construction (the two goroutines above may
	// finish in either order).
	final := currentID.Add(1)
	s.SetHighlighterFactory(newFactory(final))
	s.decorateEditor(e)

	h, ok := e.Highlighter().(*taggedHighlighter)
	if !ok {
		t.Fatalf("expected *taggedHighlighter, got %T", e.Highlighter())
	}
	if h.id != final {
		t.Errorf("editor ended up with stale factory id=%d, expected %d", h.id, final)
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
