package ui

import (
	"strings"
	"testing"
)

// TestClassifyUserMessage_DeterministicStates covers the three v1 glyph
// outcomes. Heuristic-state regression check too: text containing
// "approve", "plan", "?", "Error" must NOT trigger an Attached glyph
// — those were cut from v1, and accidentally classifying on them
// would resurrect the false-positive risk.
func TestClassifyUserMessage_DeterministicStates(t *testing.T) {
	cases := []struct {
		name string
		text string
		want UserGlyph
	}{
		{"plain text → neutral", "fix the parser bug", UserGlyphNeutral},
		{"approval prose → neutral", "ship it", UserGlyphNeutral},
		{"question prose → neutral", "what does this do?", UserGlyphNeutral},
		{"plan prose → neutral", "let's plan the refactor", UserGlyphNeutral},
		{"email is not a file ref", "ping me at fred@example.com", UserGlyphNeutral},
		{"@-prefixed path → attached", "check @main.go for the bug", UserGlyphAttached},
		{"@-prefixed abs path → attached", "look at @/etc/hosts", UserGlyphAttached},
		{"multiple @-refs → attached", "@a.go and @b.go", UserGlyphAttached},
		{"bare @ → neutral", "@ what?", UserGlyphNeutral},
		{"@@ → neutral", "ping @@channel", UserGlyphNeutral},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyUserMessage(tc.text); got != tc.want {
				t.Errorf("classifyUserMessage(%q) = %d; want %d", tc.text, got, tc.want)
			}
		})
	}
}

// TestAppendUserMessage_RecordsClassifiedGlyph verifies the wiring:
// AppendUserMessage classifies the text and stamps the resulting kind
// onto the rawLineMark of the prefix-bearing raw line. Without this
// link, the render path's glyph-color split has nothing to look up.
func TestAppendUserMessage_RecordsClassifiedGlyph(t *testing.T) {
	m := beatPane()
	m.AppendUserMessage("check @main.go")

	// Find the raw line carrying the prefix — exactly one mark must carry
	// a glyph and the kind must match classification.
	glyphs := glyphMarks(m)
	if got := len(glyphs); got != 1 {
		t.Fatalf("glyph-bearing marks = %d; want 1", got)
	}
	for raw, glyph := range glyphs {
		if glyph != UserGlyphAttached {
			t.Errorf("glyph at raw %d = %d; want UserGlyphAttached", raw, glyph)
		}
		// The prefix-bearing raw must actually contain the prefix rune.
		if !strings.HasPrefix(m.RawLines[raw], userGlyphPrefix(UserGlyphAttached)) {
			t.Errorf("raw line %d does not start with attached glyph prefix: %q",
				raw, m.RawLines[raw])
		}
	}
}

// TestAppendInterruptUserMessage_BypassesClassification verifies the
// interrupt entry point always stamps [UserGlyphInterrupt] regardless
// of message text — the path is what makes the kind deterministic, not
// the content. A message that would otherwise classify as Attached
// (contains @-refs) must still come out as Interrupt.
func TestAppendInterruptUserMessage_BypassesClassification(t *testing.T) {
	m := beatPane()
	m.AppendInterruptUserMessage("stop working on @main.go")

	glyphs := glyphMarks(m)
	if got := len(glyphs); got != 1 {
		t.Fatalf("glyph-bearing marks = %d; want 1", got)
	}
	for _, glyph := range glyphs {
		if glyph != UserGlyphInterrupt {
			t.Errorf("glyph = %d; want UserGlyphInterrupt (path wins over content)", glyph)
		}
	}
}

// TestRender_AttachedGlyph_UsesProposalTextHue verifies the render-side
// split: the leading glyph cell on an Attached user message renders
// with ProposalText hue (#AFA9EC) — distinct from the user body's
// Accent — so the eye can tell at a glance "this message had files
// attached".
func TestRender_AttachedGlyph_UsesProposalTextHue(t *testing.T) {
	m := beatPane()
	m.AppendUserMessage("check @main.go")

	out := m.Render()
	// ProposalText is #AFA9EC = 175;169;236 in lipgloss TrueColor.
	const proposalTextSeq = "38;2;175;169;236"
	if !strings.Contains(out, proposalTextSeq) {
		t.Errorf("attached glyph missing ProposalText color sequence %q in render output", proposalTextSeq)
	}
}

// glyphMarks returns raw-line index -> glyph for every mark carrying a
// non-neutral glyph.
func glyphMarks(m *AgentPaneModel) map[int]UserGlyph {
	out := map[int]UserGlyph{}
	for raw, mark := range m.rawMarks {
		if mark.glyph != UserGlyphNeutral {
			out[raw] = mark.glyph
		}
	}
	return out
}
