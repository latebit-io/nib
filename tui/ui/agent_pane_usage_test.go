package ui

import (
	"strings"
	"testing"

	"github.com/latebit-io/nib/coding/event"
)

type mockClipboard struct{ content string }

func (c *mockClipboard) Read() string         { return c.content }
func (c *mockClipboard) Write(s string) error { c.content = s; return nil }

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

// Pi-style format: ↑fresh_input ↓output R<cache_read> · total (cache%).
// Arrow convention matches pi (web-ui/format.ts::formatUsage): ↑=input,
// ↓=output, R=cache-read.
func TestFormatTurnUsage(t *testing.T) {
	// PromptTokens is fresh-only after adapter normalization.
	// Gross input = 4521 + 3200 = 7721; cache% = 3200/7721 ≈ 41%.
	// Total footprint = fresh + cached + output = 8033.
	u := event.AgentTurnUsage{
		Turn:             3,
		PromptTokens:     4521,
		CompletionTokens: 312,
		CachedTokens:     3200,
		ToolCalls:        2,
	}
	got := formatTurnUsage(u, "gemini-2.5-flash")
	checks := []string{
		"◇", "gemini-2.5-flash", "turn 3",
		"↑4.5k",      // fresh input
		"↓312",       // output
		"R3.2k",      // cache read
		"8.0k total", // full footprint
		"41%⚡",       // cache hit ratio over gross input
		"2 tools",
	}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output: %q", want, got)
		}
	}
}

// TestFormatTurnUsage_NoCacheShowsCleanLine pins the "no cache yet"
// case: when CachedTokens=0, the `R<cache>` and `(⚡%)` chunks are
// omitted so the line stays tight.
func TestFormatTurnUsage_NoCacheShowsCleanLine(t *testing.T) {
	u := event.AgentTurnUsage{
		Turn:             1,
		PromptTokens:     4000,
		CompletionTokens: 14,
		CachedTokens:     0,
		ToolCalls:        1,
	}
	got := formatTurnUsage(u, "")
	if !strings.Contains(got, "↑4.0k") {
		t.Errorf("expected fresh input ↑4.0k: %q", got)
	}
	if !strings.Contains(got, "↓14") {
		t.Errorf("expected output ↓14: %q", got)
	}
	if !strings.Contains(got, "4.0k total") {
		t.Errorf("expected total = prompt+completion when no cache: %q", got)
	}
	if strings.Contains(got, "R") {
		t.Errorf("R<cache> should be omitted when cached=0: %q", got)
	}
	if strings.Contains(got, "⚡") {
		t.Errorf("⚡ should be omitted when cached=0: %q", got)
	}
}

func TestFormatTurnUsageNoProviderData(t *testing.T) {
	u := event.AgentTurnUsage{
		Turn:          1,
		SystemEst:     500,
		ToolsEst:      300,
		NewEst:        200,
		CompletionEst: 150,
	}
	got := formatTurnUsage(u, "")
	// Estimated values with ~ marker glued to the number, pi-style
	// arrows, no model section.
	checks := []string{"turn 1", "↑~1.0k", "↓~150"}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output: %q", want, got)
		}
	}
	if strings.Contains(got, "◇ ·") {
		t.Errorf("empty model should not produce leading ◇ · prefix, got: %q", got)
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
	if !m.usage.hasExact {
		t.Error("hasExact should be true after provider data")
	}
	if m.usage.turns != 2 {
		t.Errorf("turns = %d, want 2", m.usage.turns)
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
