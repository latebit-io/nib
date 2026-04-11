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
		{999999, "1000.0k"},
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
		Turn:       1,
		SystemEst:  500,
		ToolsEst:   300,
		HistoryEst: 0,
		NewEst:     200,
	}
	got := formatTurnUsage(u)
	if !strings.Contains(got, "turn 1") {
		t.Error("should contain turn number")
	}
	// No provider data — should not contain "in" or "out"
	if strings.Contains(got, " in") {
		t.Error("should not contain input tokens when provider reports 0")
	}
	// Should still contain estimates
	if !strings.Contains(got, "sys:500") {
		t.Error("should contain system estimate")
	}
}

func TestFormatSessionSummary(t *testing.T) {
	u := usageState{
		totalPrompt:     24891,
		totalCompletion: 3421,
		totalCached:     18200,
		turns:           5,
	}
	got := formatSessionSummary(u)
	if !strings.Contains(got, "5 turns") {
		t.Error("should contain turn count")
	}
	if !strings.Contains(got, "24.9k in") {
		t.Errorf("should contain total prompt tokens, got: %s", got)
	}
	if !strings.Contains(got, "18.2k cached") {
		t.Error("should contain cached tokens")
	}
	if !strings.Contains(got, "73%") {
		t.Error("should contain cache hit percentage")
	}
	if !strings.Contains(got, "3.4k out") {
		t.Error("should contain total completion tokens")
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

	if m.usage.totalPrompt != 2500 {
		t.Errorf("totalPrompt = %d, want 2500", m.usage.totalPrompt)
	}
	if m.usage.totalCompletion != 500 {
		t.Errorf("totalCompletion = %d, want 500", m.usage.totalCompletion)
	}
	if m.usage.totalCached != 2000 {
		t.Errorf("totalCached = %d, want 2000", m.usage.totalCached)
	}
	if m.usage.turns != 2 {
		t.Errorf("turns = %d, want 2", m.usage.turns)
	}
}
