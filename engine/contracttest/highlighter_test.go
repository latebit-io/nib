package contracttest

import (
	"testing"

	"github.com/latebit-io/nib/engine/syntax"
)

// nopHighlighter is a minimal in-process [syntax.Highlighter] that
// satisfies the contract trivially: Parse stores nothing,
// HighlightLine always returns nil, Close is a no-op. Used as the
// canonical conforming implementation to verify the [Highlighter]
// fixture is wired correctly.
type nopHighlighter struct{}

func (nopHighlighter) Parse(string)                     {}
func (nopHighlighter) HighlightLine(int) []syntax.Token { return nil }
func (nopHighlighter) Close()                           {}

// TestHighlighter_NopImplementationPasses confirms the fixture passes
// a canonical conforming highlighter.
func TestHighlighter_NopImplementationPasses(t *testing.T) {
	Highlighter(t, func() syntax.Highlighter { return nopHighlighter{} })
}
