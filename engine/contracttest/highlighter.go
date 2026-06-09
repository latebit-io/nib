package contracttest

import (
	"testing"

	"github.com/latebit-io/nib/engine/syntax"
)

// Highlighter runs the [syntax.Highlighter] contract suite against
// highlighters returned by ctor.
//
// Contract preconditions on the ctor'd highlighter:
//
//   - The returned highlighter is freshly constructed and not yet closed.
//   - It is configured for some language (the fixture is source-agnostic;
//     it parses arbitrary text, including the empty string, and verifies
//     no panic).
//
// Subtests verify the documented invariants on [syntax.Highlighter]:
// Parse handles arbitrary input (including empty), HighlightLine
// returns empty for out-of-range line numbers without panicking,
// repeated Parse calls are safe, Close releases resources.
//
// Each subtest constructs a fresh highlighter via ctor and is
// responsible for calling Close on its own copy. The fixture does NOT
// auto-Close because Close is unspecified to be idempotent ("After
// Close, the Highlighter must not be used") and the CloseDoesNotPanic
// subtest exercises it directly.
//
// Apply to a new highlighter implementation by adding a single test:
//
//	func TestMyHighlighterContract(t *testing.T) {
//	    contracttest.Highlighter(t, func() syntax.Highlighter {
//	        return myhl.New("file.go")
//	    })
//	}
func Highlighter(t *testing.T, ctor func() syntax.Highlighter) {
	t.Helper()
	if ctor == nil {
		t.Fatal("contracttest.Highlighter: ctor must be non-nil")
	}
	t.Run("ParseEmptySourceDoesNotPanic", func(t *testing.T) { hlParseEmpty(t, ctor) })
	t.Run("ParseArbitrarySourceDoesNotPanic", func(t *testing.T) { hlParseArbitrary(t, ctor) })
	t.Run("HighlightLineNegativeReturnsEmpty", func(t *testing.T) { hlNegativeLine(t, ctor) })
	t.Run("HighlightLineOutOfRangeReturnsEmpty", func(t *testing.T) { hlOutOfRangeLine(t, ctor) })
	t.Run("HighlightLineWithoutParseReturnsEmpty", func(t *testing.T) { hlBeforeParse(t, ctor) })
	t.Run("RepeatedParseDoesNotPanic", func(t *testing.T) { hlRepeatedParse(t, ctor) })
	t.Run("CloseDoesNotPanic", func(t *testing.T) { hlClose(t, ctor) })
}

func hlParseEmpty(t *testing.T, ctor func() syntax.Highlighter) {
	t.Parallel()
	h := ctor()
	defer h.Close()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("contract: Parse(\"\") panicked: %v", r)
		}
	}()
	h.Parse("")
}

func hlParseArbitrary(t *testing.T, ctor func() syntax.Highlighter) {
	t.Parallel()
	h := ctor()
	defer h.Close()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("contract: Parse(arbitrary text) panicked: %v", r)
		}
	}()
	// Mix valid-looking and gibberish lines so the test surfaces
	// panics from any code path that assumes well-formed source.
	h.Parse("line one\n\xff\xfe garbage\n  // last line ")
}

func hlNegativeLine(t *testing.T, ctor func() syntax.Highlighter) {
	t.Parallel()
	h := ctor()
	defer h.Close()
	h.Parse("a\nb\nc\n")
	if got := h.HighlightLine(-1); len(got) != 0 {
		t.Fatalf("contract: HighlightLine(-1) returned %d tokens, want 0", len(got))
	}
}

func hlOutOfRangeLine(t *testing.T, ctor func() syntax.Highlighter) {
	t.Parallel()
	h := ctor()
	defer h.Close()
	h.Parse("a\nb\nc\n")
	if got := h.HighlightLine(1000); len(got) != 0 {
		t.Fatalf("contract: HighlightLine(1000) returned %d tokens, want 0", len(got))
	}
}

func hlBeforeParse(t *testing.T, ctor func() syntax.Highlighter) {
	t.Parallel()
	h := ctor()
	defer h.Close()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("contract: HighlightLine before Parse panicked: %v", r)
		}
	}()
	if got := h.HighlightLine(0); len(got) != 0 {
		t.Fatalf("contract: HighlightLine(0) before Parse returned %d tokens, want 0", len(got))
	}
}

func hlRepeatedParse(t *testing.T, ctor func() syntax.Highlighter) {
	t.Parallel()
	h := ctor()
	defer h.Close()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("contract: repeated Parse panicked: %v", r)
		}
	}()
	h.Parse("first version\n")
	h.Parse("second version\nwith more lines\n")
	h.Parse("")
	h.Parse("third\n")
}

// hlClose owns its highlighter and Closes it directly — the subtest
// IS the close test, so no t.Cleanup wrapper.
func hlClose(t *testing.T, ctor func() syntax.Highlighter) {
	t.Parallel()
	h := ctor()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("contract: Close panicked on a freshly-parsed highlighter: %v", r)
		}
	}()
	h.Parse("hello\n")
	h.Close()
}
