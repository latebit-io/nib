package ui

import (
	"strings"
	"testing"

	"github.com/latebit-io/junto/engine/event"
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
		SystemEst:        1800,
		ToolsEst:         1200,
		HistoryEst:       1100,
		NewEst:           400,
	}
	got := formatTurnUsage(u)
	if !strings.Contains(got, "turn 3") {
		t.Error("should contain turn number")
	}
	if !strings.Contains(got, "4.5k in") {
		t.Errorf("should contain prompt tokens, got: %s", got)
	}
	if !strings.Contains(got, "3.2k cached") {
		t.Error("should contain cached tokens")
	}
	if !strings.Contains(got, "312 out") {
		t.Error("should contain completion tokens")
	}
	if !strings.Contains(got, "2 tools") {
		t.Error("should contain tool call count")
	}
	if !strings.Contains(got, "sys:1.8k") {
		t.Error("should contain system estimate")
	}
	if !strings.Contains(got, "hist:1.1k") {
		t.Error("should contain history estimate")
	}
}

func TestFormatTurnUsageNoProviderData(t *testing.T) {
	u := event.AgentTurnUsage{
		Turn:          1,
		SystemEst:     500,
		ToolsEst:      300,
		HistoryEst:    0,
		NewEst:        200,
		CompletionEst: 150,
	}
	got := formatTurnUsage(u)
	if !strings.Contains(got, "turn 1") {
		t.Error("should contain turn number")
	}
	// Should show estimated values with ~ prefix
	if !strings.Contains(got, "~1.0k in") {
		t.Errorf("should contain estimated input with ~ prefix, got: %s", got)
	}
	if !strings.Contains(got, "~150 out") {
		t.Errorf("should contain estimated output with ~ prefix, got: %s", got)
	}
	// Should still contain composition estimates
	if !strings.Contains(got, "sys:500") {
		t.Error("should contain system estimate")
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
	if !strings.Contains(got, "5 turns") {
		t.Error("should contain turn count")
	}
	if !strings.Contains(got, "24.9k in") {
		t.Errorf("should contain total input tokens, got: %s", got)
	}
	if !strings.Contains(got, "18.2k cached") {
		t.Error("should contain cached tokens")
	}
	if !strings.Contains(got, "73%") {
		t.Error("should contain cache hit percentage")
	}
	if !strings.Contains(got, "3.4k out") {
		t.Error("should contain total output tokens")
	}
	// Exact data — no ~ prefix.
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
	if !strings.Contains(got, "3 turns") {
		t.Error("should contain turn count")
	}
	if !strings.Contains(got, "~5.0k in") {
		t.Errorf("should contain estimated input with ~ prefix, got: %s", got)
	}
	if !strings.Contains(got, "~1.0k out") {
		t.Errorf("should contain estimated output with ~ prefix, got: %s", got)
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
}
