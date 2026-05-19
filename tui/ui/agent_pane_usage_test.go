package ui

import (
	"strings"
	"testing"

	"github.com/latebit-io/nib/coding/event"
)

type mockClipboard struct{ content string }

func (c *mockClipboard) Read() string         { return c.content }
func (c *mockClipboard) Write(s string) error { c.content = s; return nil }

// TestAppendToolCall_FoldsPendingTurnStats covers the Option B
// happy path: AgentTurnUsage stashes; the next AgentToolCall
// renders a bullet with the inline stats fragment appended.
func TestAppendToolCall_FoldsPendingTurnStats(t *testing.T) {
	svc := &Services{Clipboard: &mockClipboard{}}
	m := NewAgentPaneModel(svc, true)

	m.AppendTurnUsage(event.AgentTurnUsage{
		PromptTokens:     4700,
		CompletionTokens: 2200,
	})
	m.AppendToolCall("write_file")

	transcript := strings.Join(m.RawLines, "\n")
	if !strings.Contains(transcript, "● write_file · ↑4.7k ↓2.2k") {
		t.Errorf("expected bullet with folded stats; transcript = %q", transcript)
	}
	// Verify the stats were CONSUMED — a second bullet should render
	// unadorned (parallel tools in the same batch share one cost
	// attribution on the first bullet).
	m.AppendToolCall("read_file")
	transcript = strings.Join(m.RawLines, "\n")
	if !strings.Contains(transcript, "● read_file\n") && !strings.HasSuffix(transcript, "● read_file") {
		t.Errorf("second bullet should be unadorned; transcript = %q", transcript)
	}
	if strings.Contains(transcript, "read_file · ↑") {
		t.Errorf("second bullet should NOT carry stats (already attributed to first): %q", transcript)
	}
}

// TestFlushPendingTurnUsage_StandaloneOnNoTool covers the
// tool-less-turn case: a final-reply turn (or any turn that just
// streamed text with no tool calls) needs a standalone stats line
// because there's no bullet to fold into.
func TestFlushPendingTurnUsage_StandaloneOnNoTool(t *testing.T) {
	svc := &Services{Clipboard: &mockClipboard{}}
	m := NewAgentPaneModel(svc, true)

	m.AppendTurnUsage(event.AgentTurnUsage{
		PromptTokens:     43_400,
		CompletionTokens: 91,
	})
	// No AppendToolCall — simulate the final reply.
	m.FlushPendingTurnUsage()

	transcript := strings.Join(m.RawLines, "\n")
	if !strings.Contains(transcript, "◇ · ↑43.4k ↓91") {
		t.Errorf("standalone flush should write ◇ + stats line; transcript = %q", transcript)
	}
}

// TestAppendTurnUsage_FlushesPriorPendingFirst covers the back-to-back
// tool-less-turn case: if a second AgentTurnUsage arrives while the
// first is still pending, the first should flush as a standalone
// line before the second stashes. Otherwise stats would be silently
// dropped.
func TestAppendTurnUsage_FlushesPriorPendingFirst(t *testing.T) {
	svc := &Services{Clipboard: &mockClipboard{}}
	m := NewAgentPaneModel(svc, true)

	m.AppendTurnUsage(event.AgentTurnUsage{
		PromptTokens:     1000,
		CompletionTokens: 50,
	})
	// Second usage arrives without a tool call between — prior must flush.
	m.AppendTurnUsage(event.AgentTurnUsage{
		PromptTokens:     2000,
		CompletionTokens: 75,
	})

	transcript := strings.Join(m.RawLines, "\n")
	if !strings.Contains(transcript, "◇ · ↑1.0k ↓50") {
		t.Errorf("prior turn's stats should have flushed: %q", transcript)
	}
}

// TestUsageIndicator_ShowsContextWindowBar pins the status-bar
// content: it must surface the context-occupancy bar with a
// `context window:` label, not the per-turn ↑↓R breakdown (that
// information lives in the pane footer above). Splitting the two
// surfaces means a watcher reads them as complementary instead of
// competing.
func TestUsageIndicator_ShowsContextWindowBar(t *testing.T) {
	svc := &Services{Clipboard: &mockClipboard{}}
	m := NewAgentPaneModel(svc, true)
	m.modelLabel = "gpt-5.4"
	// Simulate a completed turn so lastTurnTotal is populated.
	m.UpdateUsage(event.AgentTurnUsage{
		PromptTokens:     30_000,
		CachedTokens:     10_000,
		CompletionTokens: 3_520, // 43,520 total = 16% of 272k window
	})

	got := m.UsageIndicator()
	if !strings.HasPrefix(got, "context window: ") {
		t.Errorf("status bar should lead with 'context window:' label; got %q", got)
	}
	if !strings.Contains(got, "16% / 272k") {
		t.Errorf("status bar should show 16%% / 272k; got %q", got)
	}
	if !strings.Contains(got, "▓") || !strings.Contains(got, "░") {
		t.Errorf("status bar should contain the ▓/░ bar viz; got %q", got)
	}
	// The bottom bar is dedicated to context occupancy. ↑↓R numbers
	// live in the pane footer above; surfacing them here too would
	// duplicate the same information across competing surfaces.
	for _, unwanted := range []string{"↑", "↓", " R"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("status bar should not duplicate %q (lives in pane footer): %q", unwanted, got)
		}
	}
}

// TestUsageIndicator_EmptyWhenNoUsage covers the pre-run case:
// no turn data yet means no bar to draw, and an empty string keeps
// the status bar from showing a misleading `context window: ░░░...`
// before the first LLM call has even fired.
func TestUsageIndicator_EmptyWhenNoUsage(t *testing.T) {
	svc := &Services{Clipboard: &mockClipboard{}}
	m := NewAgentPaneModel(svc, true)
	m.modelLabel = "gpt-5.4"
	if got := m.UsageIndicator(); got != "" {
		t.Errorf("pre-run indicator should be empty, got %q", got)
	}
}

// TestFormatContextChip pins the model→window lookup and the
// resulting `N%/<window>` chip format. Matches pi's `4.0%/272k`
// shape so users coming from pi recognize it at a glance.
func TestFormatContextChip(t *testing.T) {
	tests := []struct {
		name           string
		lastTurnTotal  int
		model          string
		wantContains   string
		wantNonEmpty   bool
		wantWindowSize int // for sanity check on the lookup
	}{
		{
			name:           "gpt-5 family → 272k window matches pi",
			lastTurnTotal:  10_880, // 4% of 272k
			model:          "gpt-5.4",
			wantContains:   "4%/272.0k",
			wantNonEmpty:   true,
			wantWindowSize: 272_000,
		},
		{
			name:          "claude-opus-4 → 200k window",
			lastTurnTotal: 20_000, // 10% of 200k
			model:         "claude-opus-4-7",
			wantContains:  "10%/200.0k",
			wantNonEmpty:  true,
		},
		{
			name:          "claude-sonnet-4 → 1M window",
			lastTurnTotal: 50_000, // 5% of 1M
			model:         "claude-sonnet-4-6",
			wantContains:  "5%/1.0M",
			wantNonEmpty:  true,
		},
		{
			name:          "unknown model → defaultContextWindow",
			lastTurnTotal: 20_000, // 10% of 200k default
			model:         "some-future-model-not-listed",
			wantContains:  "10%/200.0k",
			wantNonEmpty:  true,
		},
		{
			name:          "empty model → no chip",
			lastTurnTotal: 1000,
			model:         "",
			wantNonEmpty:  false,
		},
		{
			name:          "zero turn total → no chip",
			lastTurnTotal: 0,
			model:         "claude-sonnet-4",
			wantNonEmpty:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatContextChip(tt.lastTurnTotal, tt.model)
			if tt.wantNonEmpty {
				if got == "" {
					t.Fatalf("want non-empty chip, got empty")
				}
				if !strings.Contains(got, tt.wantContains) {
					t.Errorf("chip = %q, want substring %q", got, tt.wantContains)
				}
			} else if got != "" {
				t.Errorf("want empty chip, got %q", got)
			}
		})
	}
}

func TestFormatTokenCount(t *testing.T) {
	tests := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{1, "1"},
		{999, "999"},
		{1000, "1.0k"},
		{1234, "1.2k"},
		{12345, "12.3k"},
		{999999, "1.0M"},
		{1000000, "1.0M"},
		{1234567, "1.2M"},
	}
	for _, tt := range tests {
		got := formatTokenCount(tt.n)
		if got != tt.want {
			t.Errorf("formatTokenCount(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

// TestFormatTurnStatsInline pins the inline stats fragment that
// trails a tool-call bullet (Option B layout). Format:
// ` · ↑X ↓Y [R Z · N%⚡]`. Leading " · " separator so the chunk
// drops cleanly into a `● tool_name` line.
func TestFormatTurnStatsInline(t *testing.T) {
	tests := []struct {
		name string
		u    event.AgentTurnUsage
		want string
	}{
		{
			name: "no cache — ↑↓ only, no R chunk",
			u: event.AgentTurnUsage{
				PromptTokens:     4000,
				CompletionTokens: 14,
			},
			want: " · ↑4.0k ↓14",
		},
		{
			name: "with cache — full breakdown with cache % over gross",
			u: event.AgentTurnUsage{
				PromptTokens:     485,
				CompletionTokens: 45,
				CachedTokens:     4115, // gross = 4600, cache% = 89%
			},
			want: " · ↑485 ↓45 R4.1k · 89%⚡",
		},
		{
			name: "zero provider data — empty (estimate-only paths bypass this fn)",
			u:    event.AgentTurnUsage{},
			want: "",
		},
		{
			name: "cached-only without prompt — still renders R chunk",
			u: event.AgentTurnUsage{
				CompletionTokens: 100,
				CachedTokens:     2000,
			},
			want: " · ↑0 ↓100 R2.0k · 100%⚡",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatTurnStatsInline(tt.u)
			if got != tt.want {
				t.Errorf("formatTurnStatsInline(%+v) = %q, want %q", tt.u, got, tt.want)
			}
		})
	}
}

func TestFormatSessionSummary(t *testing.T) {
	// totalIn = gross input across session = fresh + cached. With
	// totalIn=24891, totalCached=18200: fresh = 6691, total = 28312.
	// Cache% over gross input = 18200/24891 ≈ 73%.
	u := usageState{
		totalIn:     24891,
		totalOut:    3421,
		totalCached: 18200,
		hasExact:    true,
		turns:       5,
	}
	got := formatSessionSummary(u, "")
	checks := []string{
		"5 turns",
		"↑6.7k",       // fresh input
		"↓3.4k",       // output
		"R18.2k",      // cache read
		"28.3k total", // fresh + cached + output
		"73%⚡",
	}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output: %q", want, got)
		}
	}
	if strings.Contains(got, "~") {
		t.Errorf("exact data should not have ~ prefix, got: %s", got)
	}
}

func TestFormatSessionSummaryEstimateOnly(t *testing.T) {
	u := usageState{
		totalIn:  5000,
		totalOut: 1000,
		turns:    3,
	}
	got := formatSessionSummary(u, "")
	// Estimate path: totalCached=0, so no R<cache>; fresh = totalIn.
	// The ~ marker sits right before the number it qualifies so the
	// arrow stays glued to its operand.
	checks := []string{"3 turns", "↑~5.0k", "↓~1.0k"}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output: %q", want, got)
		}
	}
}

func TestFormatSessionSummaryEmpty(t *testing.T) {
	u := usageState{}
	got := formatSessionSummary(u, "")
	if got != "" {
		t.Errorf("empty usage should produce empty summary, got %q", got)
	}
}

func TestUpdateUsage(t *testing.T) {
	svc := &Services{Clipboard: &mockClipboard{}}
	m := NewAgentPaneModel(svc, true)
	// Two turns with provider data. Post-normalization, PromptTokens
	// is fresh-only and CachedTokens is disjoint. totalIn
	// accumulates the GROSS input sum (fresh + cached) so the ↓
	// indicator stays continuous with the historical "what would
	// have been displayed" number.
	m.UpdateUsage(event.AgentTurnUsage{
		PromptTokens:     1000, // fresh
		CompletionTokens: 200,
		CachedTokens:     800, // disjoint cached subset
	})
	m.UpdateUsage(event.AgentTurnUsage{
		PromptTokens:     1500,
		CompletionTokens: 300,
		CachedTokens:     1200,
	})

	// totalIn = (1000+800) + (1500+1200) = 4500 gross input across both turns
	if m.usage.totalIn != 4500 {
		t.Errorf("totalIn = %d, want 4500 (gross input = fresh + cached)", m.usage.totalIn)
	}
	if m.usage.totalOut != 500 {
		t.Errorf("totalOut = %d, want 500", m.usage.totalOut)
	}
	if m.usage.totalCached != 2000 {
		t.Errorf("totalCached = %d, want 2000", m.usage.totalCached)
	}
	// Last-turn snapshot reflects the SECOND turn (1500/300/1200) so
	// the pane footer + status bar show the most recent shape rather
	// than the cumulative sum — pi convention.
	if m.usage.lastTurnFresh != 1500 {
		t.Errorf("lastTurnFresh = %d, want 1500 (latest turn's fresh input)", m.usage.lastTurnFresh)
	}
	if m.usage.lastTurnOut != 300 {
		t.Errorf("lastTurnOut = %d, want 300", m.usage.lastTurnOut)
	}
	if m.usage.lastTurnCached != 1200 {
		t.Errorf("lastTurnCached = %d, want 1200", m.usage.lastTurnCached)
	}
	if m.usage.lastTurnTotal != 3000 {
		t.Errorf("lastTurnTotal = %d, want 3000 (1500+1200+300)", m.usage.lastTurnTotal)
	}
	if !m.usage.hasExact {
		t.Error("hasExact should be true after provider data")
	}
	if m.usage.turns != 2 {
		t.Errorf("turns = %d, want 2", m.usage.turns)
	}
}

// TestFormatContextBar pins the ASCII bar shape so a regression on
// the visualization (cell count, fill character, percentage math)
// fails loud rather than rendering a misleading bar in the TUI.
func TestFormatContextBar(t *testing.T) {
	tests := []struct {
		name          string
		lastTurnTotal int
		model         string
		wantCellsFull int
		wantContains  string
		wantEmpty     bool
	}{
		{
			name:          "16% of 272k → 3 filled cells (matches user's screenshot reading)",
			lastTurnTotal: 43_520, // 16% × 272_000
			model:         "gpt-5.4",
			wantCellsFull: 3,
			wantContains:  "16% / 272k",
		},
		{
			name:          "0% → bar is all empty cells",
			lastTurnTotal: 1, // barely above zero — rounds to 0 cells
			model:         "claude-opus-4",
			wantCellsFull: 0,
			wantContains:  "0% / 200k",
		},
		{
			name:          "100% → bar is full",
			lastTurnTotal: 200_000,
			model:         "claude-opus-4",
			wantCellsFull: 20,
			wantContains:  "100% / 200k",
		},
		{
			name:          "overflow clamps to 100% / full bar",
			lastTurnTotal: 500_000, // 250% of a 200k window
			model:         "claude-opus-4",
			wantCellsFull: 20,
			wantContains:  "250% / 200k",
		},
		{
			name:          "empty model → no bar",
			lastTurnTotal: 50_000,
			model:         "",
			wantEmpty:     true,
		},
		{
			name:          "zero total → no bar",
			lastTurnTotal: 0,
			model:         "gpt-5",
			wantEmpty:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatContextBar(tt.lastTurnTotal, tt.model)
			if tt.wantEmpty {
				if got != "" {
					t.Errorf("want empty bar, got %q", got)
				}
				return
			}
			if got == "" {
				t.Fatalf("want non-empty bar")
			}
			filled := strings.Count(got, "▓")
			if filled != tt.wantCellsFull {
				t.Errorf("filled cells = %d, want %d; bar = %q", filled, tt.wantCellsFull, got)
			}
			if !strings.Contains(got, tt.wantContains) {
				t.Errorf("bar missing %q: %q", tt.wantContains, got)
			}
			// Total bar width is always [contextBarCells]; mix of ▓ and ░.
			emptyCells := strings.Count(got, "░")
			if filled+emptyCells != contextBarCells {
				t.Errorf("bar width = %d, want %d; bar = %q",
					filled+emptyCells, contextBarCells, got)
			}
		})
	}
}

func TestUpdateUsageMixed(t *testing.T) {
	svc := &Services{Clipboard: &mockClipboard{}}
	m := NewAgentPaneModel(svc, true)
	// Turn 1: provider data.
	m.UpdateUsage(event.AgentTurnUsage{
		PromptTokens:     1000,
		CompletionTokens: 200,
	})
	// Turn 2: no provider data, only estimates.
	m.UpdateUsage(event.AgentTurnUsage{
		SystemEst:     500,
		ToolsEst:      300,
		HistoryEst:    100,
		NewEst:        100,
		CompletionEst: 150,
	})

	// totalIn = 1000 (provider) + 1000 (estimates sum) = 2000
	if m.usage.totalIn != 2000 {
		t.Errorf("totalIn = %d, want 2000", m.usage.totalIn)
	}
	// totalOut = 200 (provider) + 150 (estimate) = 350
	if m.usage.totalOut != 350 {
		t.Errorf("totalOut = %d, want 350", m.usage.totalOut)
	}
	if m.usage.turns != 2 {
		t.Errorf("turns = %d, want 2", m.usage.turns)
	}

	// Rendered summary: mixed run with provider data should show exact-style
	// (no ~ prefix) and contain the blended totals.
	got := formatSessionSummary(m.usage, "")
	if strings.Contains(got, "~") {
		t.Errorf("mixed run with provider data should not have ~ prefix, got: %s", got)
	}
	// In the new pi-style format the headline is fresh-input (↑)
	// rather than gross-input. With no CachedTokens recorded,
	// fresh = totalIn = 2000.
	if !strings.Contains(got, "↑2.0k") {
		t.Errorf("summary should contain blended fresh input ↑2.0k, got: %s", got)
	}
	if !strings.Contains(got, "↓350") {
		t.Errorf("summary should contain blended output ↓350, got: %s", got)
	}
}
