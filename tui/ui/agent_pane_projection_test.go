package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/nib/coding/event"
)

// TestProjection_PassthroughWhenCollapseDisabled asserts the
// fail-safe: with [AgentPaneModel.collapseEnabled] toggled off, the
// projection is 1:1 with [AgentPaneModel.Lines] — every wrapped line
// emits a passthrough [projRawLine] row in order, even if a beat has
// its auto-collapse flag set from a prior transition. Production
// defaults the flag to true (step 3), so this test explicitly disables
// it to lock down the kill-switch behavior in case future regressions
// need a way to fall back to the flat-log view.
func TestProjection_PassthroughWhenCollapseDisabled(t *testing.T) {
	m := beatPane()
	m.collapseEnabled = false

	m.AppendUserMessage("first")
	m.AppendToken("agent reply line one\n")
	m.AppendToken("agent reply line two\n")
	m.AppendUserMessage("second")
	m.AppendToken("more agent prose\n")

	// Prior beat should be marked collapsed (from step 1's auto-policy)
	// but collapseEnabled=false should still yield a passthrough
	// projection.
	if !m.beats[0].Collapsed {
		t.Fatalf("expected beat[0] auto-collapsed for the test premise; got false")
	}

	m.projDirty = true
	proj := m.projection()
	if got, want := len(proj), len(m.Lines); got != want {
		t.Fatalf("projection length = %d; want %d (= len(Lines))", got, want)
	}
	for i, pl := range proj {
		if pl.Kind != projRawLine {
			t.Errorf("proj[%d].Kind = %d; want projRawLine", i, pl.Kind)
		}
		if pl.LineIdx != i {
			t.Errorf("proj[%d].LineIdx = %d; want %d (1:1 passthrough)", i, pl.LineIdx, i)
		}
	}
}

// TestProjection_CollapsesBeatsWhenEnabled flips collapseEnabled and
// verifies a beat marked Collapsed contributes exactly one
// [projBeatSummary] row instead of its raw-line range. The other
// (uncollapsed) beats still emit their full range. This is the contract
// step 3 relies on.
func TestProjection_CollapsesBeatsWhenEnabled(t *testing.T) {
	m := beatPane()
	m.collapseEnabled = true

	m.AppendUserMessage("first")
	m.AppendToken("reply\n")
	m.AppendUserMessage("second") // auto-collapses the first user→agent beat and opens the next
	m.AppendToken("more reply\n")

	// Force the rebuild — projection is lazy.
	m.projDirty = true
	proj := m.projection()

	var summaryRows, rawRows int
	var collapsedBeats int
	for i := range m.beats {
		if m.beats[i].Collapsed {
			collapsedBeats++
		}
	}
	for _, pl := range proj {
		switch pl.Kind {
		case projBeatSummary:
			summaryRows++
		case projRawLine:
			rawRows++
		}
	}
	// One summary row per collapsed beat — that's the projection's
	// whole job. The active beat must still contribute raw rows.
	if got, want := summaryRows, collapsedBeats; got != want {
		t.Errorf("summary rows = %d; want %d (one per collapsed beat)", got, want)
	}
	if rawRows == 0 {
		t.Errorf("expected at least one raw row from the active beat; got 0")
	}
	// Projection must be strictly shorter than Lines now — collapsing
	// hides content.
	if len(proj) >= len(m.Lines) {
		t.Errorf("projection length = %d; want < len(Lines)=%d", len(proj), len(m.Lines))
	}
}

// TestBeatSummary_RendersStructuredHeader asserts the collapsed-beat
// summary row carries the caret, beat label, and meta cluster the plan
// mockup specifies. Probes the *plain* text rather than ANSI bytes so a
// hue swap doesn't break the test — that's a separate concern locked
// down by the palette tests.
func TestBeatSummary_RendersStructuredHeader(t *testing.T) {
	m := beatPane()
	clock := time.Unix(1_700_000_000, 0)
	m.nowFunc = func() time.Time { return clock }

	m.AppendUserMessage("kickoff")
	m.AppendToolCall("apply_patch")
	m.AppendToken("agent reply\n")
	m.AppendTurnUsage(event.AgentTurnUsage{PromptTokens: 8200, CompletionTokens: 312})

	clock = clock.Add(42 * time.Second)
	m.AppendUserMessage("follow up")
	// The first user→agent beat is now collapsed and should produce a
	// summary row through the projection.

	m.projDirty = true
	text := m.beatSummaryText(&m.beats[1])
	for _, want := range []string{"▸", "♩ beat 2", "1 task", "42s", "↑8.2k", "↓312"} {
		if !strings.Contains(text, want) {
			t.Errorf("summary text missing %q: %q", want, text)
		}
	}
}

// TestBeatSummary_OmitsEmptyMetaCells verifies the meta cluster degrades
// gracefully: a beat with no tool calls, no elapsed time, and no token
// usage renders just the caret + label, no trailing " · ". Keeps the
// summary row honest when the underlying signal is genuinely empty.
func TestBeatSummary_OmitsEmptyMetaCells(t *testing.T) {
	m := beatPane()
	// Implicit first beat — no content, no tools, no tokens, no
	// StartedAt. Summary should be the bare header.
	text := m.beatSummaryText(&m.beats[0])
	if strings.Contains(text, "·") {
		t.Errorf("expected no meta separator on an empty beat; got %q", text)
	}
	if !strings.Contains(text, "♩ beat 1") {
		t.Errorf("summary missing label: %q", text)
	}
}

// TestRender_CollapsedBeatProducesSummaryRow probes the full render
// pipeline: with a collapsed past beat, the rendered output contains
// the summary row's structured payload AND the active beat's content,
// but NOT the past beat's wrapped lines. This is the "scannable
// timeline" payoff the redesign is for.
func TestRender_CollapsedBeatProducesSummaryRow(t *testing.T) {
	m := beatPane()
	m.AppendUserMessage("first question with unique-keyword-XYZZY")
	m.AppendToken("agent thinking\n")
	m.AppendUserMessage("second question with marker-PQRS")
	m.AppendToken("more agent prose\n")

	out := m.Render()

	// Active beat content is still present.
	if !strings.Contains(out, "marker-PQRS") {
		t.Errorf("active beat user message missing from render: %q", out)
	}
	// Past beat content is hidden behind the summary.
	if strings.Contains(out, "unique-keyword-XYZZY") {
		t.Errorf("collapsed beat content leaked into render: %q", out)
	}
	// Summary structure is present.
	if !strings.Contains(out, "♩ beat") {
		t.Errorf("collapsed-beat summary row missing from render")
	}
}

// TestFormatBeatElapsed_BoundaryTruncation verifies the elapsed-time
// formatter respects its case boundaries. A previous version used
// d.Round(time.Second) — for inputs like 59.5s that rounded to 60s,
// producing "60s" instead of falling into the minutes branch as "1m".
// The same bug recurred at the hour boundary (59m59.5s → "59m60s").
// Truncation by integer division keeps each cell within its band.
func TestFormatBeatElapsed_BoundaryTruncation(t *testing.T) {
	cases := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"sub-second", 500 * time.Millisecond, "<1s"},
		{"exact second", time.Second, "1s"},
		{"just under minute rounds-up boundary", 59*time.Second + 500*time.Millisecond, "59s"},
		{"exact minute", time.Minute, "1m"},
		{"minute plus seconds", 2*time.Minute + 13*time.Second, "2m13s"},
		{"just under hour rounds-up boundary", 59*time.Minute + 59*time.Second + 500*time.Millisecond, "59m59s"},
		{"exact hour", time.Hour, "1h"},
		{"hours and minutes", 2*time.Hour + 30*time.Minute, "2h30m"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatBeatElapsed(tc.d); got != tc.want {
				t.Errorf("formatBeatElapsed(%v) = %q; want %q", tc.d, got, tc.want)
			}
		})
	}
}

// TestSelection_SkipsCollapsedBeatSilently locks down the v1
// cross-collapse selection contract: dragging from a row before a
// collapsed beat to a row after it copies the surrounding raw content
// as if the collapsed range weren't there. The summary row itself
// contributes nothing — selecting across it doesn't smuggle the
// chrome text into the clipboard.
func TestSelection_SkipsCollapsedBeatSilently(t *testing.T) {
	m := beatPane()

	// Force the implicit first beat expanded so it contributes raw
	// rows before the first collapsed-beat summary. Without this,
	// the implicit beat gets auto-collapsed on the very first
	// AppendUserMessage and the summary row lands at projection[0].
	m.beats[0].UserOverride = true
	m.AppendText("PROLOGUE LINE\n")

	m.AppendUserMessage("UNIQUE-BEFORE")
	m.AppendToken("hidden body line one\n")
	m.AppendToken("hidden body line two\n")
	// Open the next beat — auto-collapses the previous.
	m.AppendUserMessage("UNIQUE-AFTER")
	m.AppendToken("visible body\n")

	proj := m.projection()
	if len(proj) == 0 {
		t.Fatalf("empty projection")
	}

	// Find the summary row and pick endpoints that bracket it.
	summaryRow := -1
	for i, pl := range proj {
		if pl.Kind == projBeatSummary {
			summaryRow = i
			break
		}
	}
	if summaryRow < 0 {
		t.Fatalf("no summary row in projection; cannot test cross-collapse selection")
	}
	if summaryRow == 0 || summaryRow == len(proj)-1 {
		t.Fatalf("summary row at boundary; pick a longer transcript fixture")
	}

	// Span from the row before the summary to the row after.
	m.selActive = true
	m.selStartLn = summaryRow - 1
	m.selStartCol = 0
	m.cursorLn = summaryRow + 1
	m.cursorCol = 0

	got := m.SelectedText()
	if strings.Contains(got, "♩ beat") {
		t.Errorf("summary row leaked into selection text: %q", got)
	}
	if strings.Contains(got, "hidden body") {
		t.Errorf("collapsed beat body leaked into selection text: %q", got)
	}
}

// TestSelection_SummaryRowOnlyReturnsEmpty checks the degenerate
// single-row case: a selection entirely on a summary row produces
// empty copy text. This guards against the summary row contributing
// chrome characters when the user accidentally drags within just one
// header.
func TestSelection_SummaryRowOnlyReturnsEmpty(t *testing.T) {
	m := beatPane()
	m.AppendUserMessage("first")
	m.AppendToken("hidden\n")
	m.AppendUserMessage("second")
	m.AppendToken("active\n")

	proj := m.projection()
	summaryRow := -1
	for i, pl := range proj {
		if pl.Kind == projBeatSummary {
			summaryRow = i
			break
		}
	}
	if summaryRow < 0 {
		t.Fatalf("no summary row to test")
	}

	m.selActive = true
	m.selStartLn = summaryRow
	m.selStartCol = 0
	m.cursorLn = summaryRow
	m.cursorCol = 100 // past end; clamp should handle

	if got := m.SelectedText(); got != "" {
		t.Errorf("expected empty copy text from a summary-only selection; got %q", got)
	}
}

// TestProjection_InvalidatedOnContentAppend asserts the cache picks up
// new content. Without this hook, the projection slice would stale to
// the old [Lines] snapshot and the new agent output would never appear.
func TestProjection_InvalidatedOnContentAppend(t *testing.T) {
	m := beatPane()
	m.AppendToken("first\n")
	before := len(m.projection())

	m.AppendToken("second\n")
	after := len(m.projection())

	if after <= before {
		t.Errorf("projection length did not grow after content append: %d -> %d", before, after)
	}
}
