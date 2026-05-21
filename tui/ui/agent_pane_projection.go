package ui

import (
	"fmt"
	"strings"
	"time"
)

// Per-frame projection layer for the agent pane.
//
// The redesigned timeline collapses past beats to a single summary row
// while keeping the active beat fully expanded. That changes how the
// pane is *navigated* — scroll math, scrollbar sizing, mouse hit-test,
// selection, and the markdown cache all need to index into the visible
// rows rather than the raw wrapped-line buffer.
//
// The projection layer is the answer. [AgentPaneModel.projection]
// returns a flat slice of [projectedLine] entries, one per visible row.
// Each entry either points at a wrapped line in [AgentPaneModel.Lines]
// (an expanded beat's content) or marks a collapsed beat header. All
// callers — render loop, scrollbar, mouse, selection — operate on this
// slice rather than touching `m.Lines` directly.
//
// The slice is rebuilt lazily and invalidated whenever content changes
// (via [AgentPaneModel.invalidateMdCache]) or a beat's collapse flag
// flips. Rebuild cost is O(rawLines), well under the per-frame budget
// at typical transcript sizes.

// projKind classifies a [projectedLine].
type projKind uint8

const (
	// projRawLine is a passthrough row that renders the wrapped line at
	// [projectedLine.LineIdx]. The render path applies the same
	// per-line classification (user / dim / meta / streaming / markdown)
	// it did pre-projection.
	projRawLine projKind = iota
	// projBeatSummary is a single styled row that replaces a collapsed
	// beat's entire wrapped-line range. [projectedLine.BeatIdx] points
	// at the beat in [AgentPaneModel.beats] whose header should be
	// drawn.
	projBeatSummary
)

// projectedLine is one row of the rendered transcript. Either a
// passthrough for a wrapped line or a synthetic summary row for a
// collapsed beat.
type projectedLine struct {
	// Kind selects which of [LineIdx] / [BeatIdx] is meaningful.
	Kind projKind
	// LineIdx is the index into [AgentPaneModel.Lines] when [Kind] is
	// [projRawLine].
	LineIdx int
	// BeatIdx is the index into [AgentPaneModel.beats] when [Kind] is
	// [projBeatSummary].
	BeatIdx int
}

// projection returns the cached projection slice, rebuilding it if
// stale. Callers should treat the returned slice as read-only and
// re-fetch on subsequent calls — the slice may be reallocated when
// the rebuild happens.
func (m *AgentPaneModel) projection() []projectedLine {
	if m.projDirty || m.proj == nil {
		m.rebuildProjection()
	}
	return m.proj
}

// rebuildProjection walks beats in order and emits projection rows.
// For an expanded beat, every wrapped line in the beat's raw-range
// emits a [projRawLine] entry. For a collapsed beat, one
// [projBeatSummary] row replaces the entire range.
//
// Lines outside any beat (preamble, padding rows AppendText emits)
// still need projection rows so the projection covers all of
// [AgentPaneModel.Lines] — they always emit as [projRawLine] since
// they aren't owned by a collapsible beat.
func (m *AgentPaneModel) rebuildProjection() {
	if cap(m.proj) >= len(m.Lines) {
		m.proj = m.proj[:0]
	} else {
		m.proj = make([]projectedLine, 0, len(m.Lines))
	}

	// Map each raw-line index to the beat that owns it. Beats hold
	// StartRaw inclusively; the end is the next beat's StartRaw (or
	// len(RawLines) for the last beat).
	if len(m.beats) == 0 || len(m.Lines) == 0 {
		// No beats or no content — pass every wrapped line through
		// unchanged. The implicit-first-beat invariant means the first
		// branch shouldn't fire in practice, but the second is the
		// "empty transcript" state at startup.
		for i := range m.Lines {
			m.proj = append(m.proj, projectedLine{Kind: projRawLine, LineIdx: i})
		}
		m.projDirty = false
		return
	}

	for bi := range m.beats {
		beat := &m.beats[bi]
		startRaw := beat.StartRaw
		endRaw := len(m.RawLines)
		if bi+1 < len(m.beats) {
			endRaw = m.beats[bi+1].StartRaw
		}
		startWrap := m.wrappedStart(startRaw)
		endWrap := m.wrappedStart(endRaw)
		if endWrap <= startWrap {
			continue
		}
		if beat.Collapsed && m.collapseEnabled {
			m.proj = append(m.proj, projectedLine{Kind: projBeatSummary, BeatIdx: bi})
			continue
		}
		for wi := startWrap; wi < endWrap; wi++ {
			m.proj = append(m.proj, projectedLine{Kind: projRawLine, LineIdx: wi})
		}
	}

	m.projDirty = false
}

// wrappedStart returns the first wrapped-line index for raw line rawIdx,
// or len(Lines) when rawIdx is past the end. Walks [wrappedIndex] —
// the same lookup table [AgentPaneModel.rawIndexOf] inverts — but in
// the forward direction.
func (m *AgentPaneModel) wrappedStart(rawIdx int) int {
	if rawIdx <= 0 {
		return 0
	}
	if rawIdx >= len(m.wrappedIndex) {
		return len(m.Lines)
	}
	return m.wrappedIndex[rawIdx]
}

// beatSummaryText returns the plain-text content of a collapsed beat's
// summary row — what mouse hit-testing and clipboard copy see. The
// returned string is **caret + label + meta only**; the horizontal
// rule between the label and meta is emitted by
// [renderBeatSummaryRow] because its length depends on the terminal
// width, which the layout phase doesn't see.
//
// For reference, the fully styled row reads:
//
//	▸ ♩ beat 6 ─────────────────── 4 tasks · 42s · ↑8.2k ↓312
//
// The right-side meta cluster drops cells when the underlying signal
// is zero — a beat with no tool calls omits "N tasks"; a beat with no
// token usage omits the arrows.
func (m *AgentPaneModel) beatSummaryText(b *Beat) string {
	return m.buildBeatSummaryLayout(b).plain()
}

// beatSummaryLayout precomputes the styled fragments of a beat summary
// row so [renderBeatSummaryRow] and [beatSummaryText] share a single
// layout pass. Holding the parts separately lets the renderer style
// each cluster independently without re-running format strings.
type beatSummaryLayout struct {
	caret   string // "▸ "
	label   string // "♩ beat N "
	meta    string // " 4 tasks · 42s · ↑8.2k ↓312"
	dimHues bool   // true → use the AccentDim/SecondaryTextDim family
}

// plain renders the layout's text without ANSI styling — suitable for
// mouse hit-testing and clipboard copy.
func (l beatSummaryLayout) plain() string {
	return l.caret + l.label + l.meta
}

// buildBeatSummaryLayout assembles the fragments. Named distinctly
// from the [beatSummaryLayout] type so the doc reference and call site
// disambiguate; the horizontal rule is emitted by the renderer (not
// here) since its length depends on the terminal width, which the
// layout phase doesn't see.
func (m *AgentPaneModel) buildBeatSummaryLayout(b *Beat) beatSummaryLayout {
	var tasks int
	for i := range b.Blocks {
		if b.Blocks[i].Kind == BlockToolCall {
			tasks++
		}
	}

	var elapsed time.Duration
	if !b.StartedAt.IsZero() {
		end := b.EndedAt
		if end.IsZero() {
			end = m.now()
		}
		elapsed = end.Sub(b.StartedAt)
	}

	var meta strings.Builder
	first := true
	addMeta := func(s string) {
		if first {
			meta.WriteString(" ")
		} else {
			meta.WriteString(" · ")
		}
		meta.WriteString(s)
		first = false
	}
	if tasks > 0 {
		if tasks == 1 {
			addMeta("1 task")
		} else {
			addMeta(fmt.Sprintf("%d tasks", tasks))
		}
	}
	if elapsed > 0 {
		addMeta(formatBeatElapsed(elapsed))
	}
	if b.TokensIn > 0 || b.TokensOut > 0 {
		addMeta(fmt.Sprintf("↑%s ↓%s",
			formatTokenCount(b.TokensIn), formatTokenCount(b.TokensOut)))
	}

	return beatSummaryLayout{
		caret:   "▸ ",
		label:   fmt.Sprintf("♩ beat %d ", b.Number),
		meta:    meta.String(),
		dimHues: b.Status != BeatRunning,
	}
}

// formatBeatElapsed renders a wall-clock duration as a short label for
// the collapsed-beat summary. Sub-second durations show "<1s" so a
// near-zero beat doesn't read as an empty cell.
//
// Integer truncation (d / time.Second) is used in place of
// d.Round(time.Second) — rounding can push a value past the case
// ceiling (59.5s rounds to 60s, which would print "60s" instead of
// falling through to the minutes branch as "1m"). Truncation keeps
// each cell strictly within its band.
func formatBeatElapsed(d time.Duration) string {
	switch {
	case d < time.Second:
		return "<1s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		mins := int(d / time.Minute)
		secs := int((d % time.Minute) / time.Second)
		if secs == 0 {
			return fmt.Sprintf("%dm", mins)
		}
		return fmt.Sprintf("%dm%ds", mins, secs)
	default:
		hours := int(d / time.Hour)
		mins := int((d % time.Hour) / time.Minute)
		if mins == 0 {
			return fmt.Sprintf("%dh", hours)
		}
		return fmt.Sprintf("%dh%dm", hours, mins)
	}
}
