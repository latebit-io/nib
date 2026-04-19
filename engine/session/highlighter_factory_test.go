package session

import (
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
