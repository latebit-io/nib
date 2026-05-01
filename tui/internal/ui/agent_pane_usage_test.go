package ui

import (
	"strings"
	"testing"

	"github.com/latebit-io/nib/engine/event"
)

type mockClipboard struct{ content string }

func (c *mockClipboard) Read() string         { return c.content }
func (c *mockClipboard) Write(s string) error { c.content = s; return nil }

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

func TestFormatTurnUsage(t *testing.T) {
	u := event.AgentTurnUsage{
		Turn:             3,
		PromptTokens:     4521,
		CompletionTokens: 312,
		CachedTokens:     3200,
		ToolCalls:        2,
	}
	got := formatTurnUsage(u, "gemini-2.5-flash")
	checks := []string{"◇", "gemini-2.5-flash", "turn 3", "4.5k↓", "70%⚡", "312↑", "2 tools"}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output: %q", want, got)
		}
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
	// Estimated values with ~ prefix, arrow separators, no model section.
	checks := []string{"turn 1", "~1.0k↓", "~150↑"}
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
	u := usageState{
		totalIn:     24891,
		totalOut:    3421,
		totalCached: 18200,
		hasExact:    true,
		turns:       5,
	}
	got := formatSessionSummary(u)
	checks := []string{"5 turns", "24.9k↓", "73%⚡", "3.4k↑"}
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
	got := formatSessionSummary(u)
	checks := []string{"3 turns", "~5.0k↓", "~1.0k↑"}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output: %q", want, got)
		}
	}
}

func TestFormatSessionSummaryEmpty(t *testing.T) {
	u := usageState{}
	got := formatSessionSummary(u)
	if got != "" {
		t.Errorf("empty usage should produce empty summary, got %q", got)
	}
}

func TestUpdateUsage(t *testing.T) {
	svc := &Services{Clipboard: &mockClipboard{}}
	m := NewAgentPaneModel(svc, true)
	// Two turns with provider data.
	m.UpdateUsage(event.AgentTurnUsage{
		PromptTokens:     1000,
		CompletionTokens: 200,
		CachedTokens:     800,
	})
	m.UpdateUsage(event.AgentTurnUsage{
		PromptTokens:     1500,
		CompletionTokens: 300,
		CachedTokens:     1200,
	})

	if m.usage.totalIn != 2500 {
		t.Errorf("totalIn = %d, want 2500", m.usage.totalIn)
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
	got := formatSessionSummary(m.usage)
	if strings.Contains(got, "~") {
		t.Errorf("mixed run with provider data should not have ~ prefix, got: %s", got)
	}
	if !strings.Contains(got, "2.0k↓") {
		t.Errorf("summary should contain blended input total, got: %s", got)
	}
	if !strings.Contains(got, "350↑") {
		t.Errorf("summary should contain blended output total, got: %s", got)
	}
}
