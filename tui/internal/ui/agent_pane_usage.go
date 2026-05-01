package ui

import (
	"fmt"
	"strings"

	"github.com/latebit-io/nib/engine/event"
)

// Token-usage tracking and formatting for AgentPaneModel.
//
// usageState lives on AgentPaneModel; the methods here read and update
// the streaming/turn/session counters and produce display strings. Pure
// state + pure formatters — no UI-framework calls — so this file is the
// natural seat for AgentPaneModel's "what does the user owe / how many
// tokens have we burned" responsibility.

type usageState struct {
	totalIn           int  // best-available input tokens (provider or estimate per turn)
	totalOut          int  // best-available output tokens (provider or estimate per turn)
	totalCached       int  // provider-reported cached tokens (exact, 0 if unavailable)
	hasExact          bool // true if any turn reported provider data
	turns             int
	streamingChars    int // characters received via AppendToken since last turn completed
	streamingInputEst int // current LLM call's input estimate (set before Stream, cleared on turn end)
}

// SetStreamingInput updates the current LLM call's input estimate.
// Called right before Stream() starts so the status bar can show input cost
// in real-time while the response is streaming.
func (m *AgentPaneModel) SetStreamingInput(e event.AgentInputEstimate) {
	m.usage.streamingInputEst = e.System + e.Tools + e.History + e.New
}

// UpdateUsage accumulates token counts from a turn usage event.
// Uses provider-reported values when available, falls back to client-side
// estimates. Resets the streaming counters since the turn data supersedes them.
func (m *AgentPaneModel) UpdateUsage(u event.AgentTurnUsage) {
	turnHasProvider := u.PromptTokens > 0 || u.CompletionTokens > 0
	if turnHasProvider {
		m.usage.totalIn += u.PromptTokens
		m.usage.totalOut += u.CompletionTokens
		m.usage.totalCached += u.CachedTokens
		m.usage.hasExact = true
	} else {
		m.usage.totalIn += u.SystemEst + u.ToolsEst + u.HistoryEst + u.NewEst
		m.usage.totalOut += u.CompletionEst
	}
	m.usage.streamingChars = 0
	m.usage.streamingInputEst = 0
	m.usage.turns++
}

// UsageIndicator returns a compact string for the main editor status bar.
// Updates in real-time: shows streaming input/output estimates while tokens
// arrive. Uses ~ prefix when only estimates are available.
func (m *AgentPaneModel) UsageIndicator() string {
	streamOut := (m.usage.streamingChars + 3) / 4
	streamIn := m.usage.streamingInputEst

	in := m.usage.totalIn + streamIn
	out := m.usage.totalOut + streamOut

	if in == 0 && out == 0 {
		return ""
	}

	prefix := "~"
	if m.usage.hasExact {
		prefix = ""
	}
	s := prefix + formatTokenCount(in) + "↓"
	if m.usage.totalCached > 0 && m.usage.totalIn > 0 {
		pct := m.usage.totalCached * 100 / m.usage.totalIn
		s += fmt.Sprintf("(%d%%⚡)", pct)
	}
	s += " " + prefix + formatTokenCount(out) + "↑"
	return s
}

// ResetUsage clears accumulated usage for a new agent run.
func (m *AgentPaneModel) ResetUsage() {
	m.usage = usageState{}
}

// formatTokenCount renders a token count as a compact string.
// < 1000 → "847", ≥ 1000 → "12.3k", ≥ 999950 → "1.0M".
func formatTokenCount(n int) string {
	switch {
	case n >= 999_950: // %.1f rounds 999950+ to 1000.0k — use M instead
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// formatTurnUsage produces a compact per-turn footer: model · turn N ·
// token counts · tool count. Rendered dim via the AppendMeta path, so the
// reader skims it as chrome. Model is optional — omitted when empty so
// the line still reads well before the model label is known.
func formatTurnUsage(u event.AgentTurnUsage, model string) string {
	var b strings.Builder
	b.WriteString("\n◇ ")
	if model != "" {
		b.WriteString(sanitizeInlineDisplay(model))
		b.WriteString(" · ")
	}
	fmt.Fprintf(&b, "turn %d", u.Turn)

	hasProvider := u.PromptTokens > 0 || u.CompletionTokens > 0
	if hasProvider {
		fmt.Fprintf(&b, " · %s↓", formatTokenCount(u.PromptTokens))
		if u.CachedTokens > 0 && u.PromptTokens > 0 {
			pct := u.CachedTokens * 100 / u.PromptTokens
			fmt.Fprintf(&b, " (%d%%⚡)", pct)
		}
		fmt.Fprintf(&b, " · %s↑", formatTokenCount(u.CompletionTokens))
	} else {
		total := u.SystemEst + u.ToolsEst + u.HistoryEst + u.NewEst
		if total > 0 {
			fmt.Fprintf(&b, " · ~%s↓ · ~%s↑", formatTokenCount(total), formatTokenCount(u.CompletionEst))
		}
	}
	if u.ToolCalls > 0 {
		fmt.Fprintf(&b, " · %d tools", u.ToolCalls)
	}
	b.WriteByte('\n')
	return b.String()
}

// formatCompacted produces a dim metadata line when conversation history
// is compacted. Shows tokens before and after so the developer can see
// how much was saved.
func formatCompacted(e event.AgentCompacted) string {
	saved := e.BeforeTokens - e.AfterTokens
	return fmt.Sprintf("\n[compacted: %s → %s history (saved %s)]\n",
		formatTokenCount(e.BeforeTokens),
		formatTokenCount(e.AfterTokens),
		formatTokenCount(saved))
}

// formatSessionSummary produces the summary shown when the agent finishes.
// Matches the per-turn footer's visual language: middle-dot separators and
// arrow-glyph token counts so session-wide and per-turn metadata feel like
// one layer of chrome, not two styles.
func formatSessionSummary(u usageState) string {
	if u.turns == 0 {
		return ""
	}
	prefix := "~"
	if u.hasExact {
		prefix = ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Session · %d turns", u.turns)
	if u.totalIn > 0 || u.totalOut > 0 {
		fmt.Fprintf(&b, " · %s%s↓", prefix, formatTokenCount(u.totalIn))
		if u.totalCached > 0 && u.totalIn > 0 {
			pct := u.totalCached * 100 / u.totalIn
			fmt.Fprintf(&b, " (%d%%⚡)", pct)
		}
		fmt.Fprintf(&b, " · %s%s↑", prefix, formatTokenCount(u.totalOut))
	}
	return b.String()
}

// inputHeight returns the number of rows reserved for the input area
// (separator + input + status). Uses 1/6 of the pane height, minimum 5.
