package ui

import (
	"fmt"
	"strings"

	"github.com/latebit-io/nib/coding/event"
)

// Token-usage tracking and formatting for AgentPaneModel.
//
// usageState lives on AgentPaneModel; the methods here read and update
// the streaming/turn/session counters and produce display strings. Pure
// state + pure formatters — no UI-framework calls — so this file is the
// natural seat for AgentPaneModel's "what does the user owe / how many
// tokens have we burned" responsibility.

type usageState struct {
	// totalIn is the GROSS input sum across turns: PromptTokens
	// (fresh) + CachedTokens (cache reads), each turn. Matches the
	// historic "↓" semantics — the total bytes that would have been
	// passed to the API as the prompt prefix, regardless of cache
	// status. Fresh-only is derived as totalIn - totalCached.
	totalIn int
	// totalOut is the cumulative output-token sum across turns.
	totalOut int
	// totalCached is the cumulative cached-input subset. Disjoint
	// from PromptTokens after adapter normalization, so totalCached
	// + (totalIn - totalCached) = totalIn.
	totalCached int
	// lastTurnTotal is the most recent turn's full footprint
	// (PromptTokens + CachedTokens + CompletionTokens). Stands in
	// for "current context occupancy" — what would be re-shipped on
	// the next turn's prefix — for the `N%/<window>` chip that
	// matches pi's display.
	lastTurnTotal int
	// hasExact flips true once any turn reported provider data.
	hasExact          bool
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
//
// After the [llm.Usage] normalization, PromptTokens is fresh-only and
// CachedTokens is the disjoint cached subset. totalIn stores the
// GROSS input (fresh + cached) so display arithmetic and the
// running-total ↓ indicator stay numerically continuous with the
// pre-normalization shape; consumers wanting fresh-only derive it
// as totalIn - totalCached.
func (m *AgentPaneModel) UpdateUsage(u event.AgentTurnUsage) {
	turnHasProvider := u.PromptTokens > 0 || u.CompletionTokens > 0 || u.CachedTokens > 0
	if turnHasProvider {
		m.usage.totalIn += u.PromptTokens + u.CachedTokens
		m.usage.totalOut += u.CompletionTokens
		m.usage.totalCached += u.CachedTokens
		m.usage.lastTurnTotal = u.PromptTokens + u.CachedTokens + u.CompletionTokens
		m.usage.hasExact = true
	} else {
		est := u.SystemEst + u.ToolsEst + u.HistoryEst + u.NewEst
		m.usage.totalIn += est
		m.usage.totalOut += u.CompletionEst
		m.usage.lastTurnTotal = est + u.CompletionEst
	}
	m.usage.streamingChars = 0
	m.usage.streamingInputEst = 0
	m.usage.turns++
}

// UsageIndicator returns a compact string for the main editor status bar.
// Updates in real-time: shows streaming input/output estimates while tokens
// arrive. Uses ~ prefix when only estimates are available.
//
// Format: `↑X ↓Y RZ N%⚡` — pi-compatible arrows (↑=input, ↓=output, R=cache
// read) so a developer with both tools open can scan either status bar
// with the same parser. The `⚡` cache-percentage stays nib-specific
// chrome. The streaming input estimate is added to fresh tokens
// in-flight because nothing's been cached for the current call yet.
func (m *AgentPaneModel) UsageIndicator() string {
	streamOut := (m.usage.streamingChars + 3) / 4
	streamIn := m.usage.streamingInputEst

	out := m.usage.totalOut + streamOut
	fresh := m.usage.totalIn - m.usage.totalCached + streamIn

	if fresh == 0 && out == 0 && m.usage.totalCached == 0 {
		return ""
	}

	prefix := "~"
	if m.usage.hasExact {
		prefix = ""
	}
	s := fmt.Sprintf("↑%s%s ↓%s%s",
		prefix, formatTokenCount(fresh),
		prefix, formatTokenCount(out))
	if m.usage.totalCached > 0 && m.usage.totalIn > 0 {
		pct := m.usage.totalCached * 100 / m.usage.totalIn
		s += fmt.Sprintf(" R%s %d%%⚡", formatTokenCount(m.usage.totalCached), pct)
	}
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

// defaultContextWindow is the assumed model context window when the
// model name doesn't match a known entry. 200k covers most modern
// chat models in the wild (GPT-4o, Claude Sonnet 3.5/4.x, GPT-5);
// the chip shows an honest approximate rather than refusing to
// display when the window is unknown.
const defaultContextWindow = 200_000

// modelContextWindow returns the token-context limit for a model
// label, falling back to [defaultContextWindow] when no exact match
// is found. The lookup is intentionally minimal — we only special-
// case the models nib ships configurations for. Add an entry when a
// new model lands in the supported set; never reach for an external
// registry (opencode pulls from models.dev; that's the right
// direction once a meaningful number of models are wired).
//
// Match is case-insensitive substring on the canonical model id,
// most-specific first. The order matters because Codex / GPT
// families share prefixes.
func modelContextWindow(label string) int {
	l := strings.ToLower(label)
	known := []struct {
		match  string
		window int
	}{
		// Anthropic
		{"claude-opus-4", 200_000},
		{"claude-sonnet-4", 1_000_000},
		{"claude-haiku-4", 200_000},
		{"claude-3-5-sonnet", 200_000},
		{"claude-3-5-haiku", 200_000},
		{"claude-3-opus", 200_000},
		// OpenAI / Codex
		{"gpt-5", 272_000}, // matches pi's reported window
		{"gpt-4o", 128_000},
		{"o1", 200_000},
		{"o3", 200_000},
		// Google
		{"gemini-2.5", 1_000_000},
		{"gemini-1.5", 1_000_000},
	}
	for _, k := range known {
		if strings.Contains(l, k.match) {
			return k.window
		}
	}
	return defaultContextWindow
}

// formatContextChip renders the pi-style "N%/<window>" chip showing
// the most-recent turn's footprint as a percentage of the model's
// context window. Returns "" when the model label is empty (no
// window known and falling back to default would still produce a
// number, but without a model anchor the chip is more confusing
// than useful) or when no usage has been recorded yet.
//
// Example: 11k current context against a 272k window prints
// "4%/272k" — directly comparable to pi's `4.0%/272k` chip.
func formatContextChip(lastTurnTotal int, model string) string {
	if lastTurnTotal <= 0 || model == "" {
		return ""
	}
	window := modelContextWindow(model)
	if window <= 0 {
		return ""
	}
	pct := lastTurnTotal * 100 / window
	return fmt.Sprintf("%d%%/%s", pct, formatTokenCount(window))
}

// formatTurnUsage produces a compact per-turn footer: model · turn N ·
// token counts · tool count. Rendered dim via the AppendMeta path, so the
// reader skims it as chrome. Model is optional — omitted when empty so
// the line still reads well before the model label is known.
//
// Arrow convention matches pi (web-ui/format.ts::formatUsage):
//
//	↑X  — fresh input tokens (paid at full rate)
//	↓X  — output tokens
//	RX  — cache-read tokens (paid at the discounted rate)
//
// "Up to the LLM" / "down from the LLM" — picking the same direction
// pi uses means a reader coming from opencode/pi can compare a nib
// session row against theirs without translating axes. The `total`
// + `⚡` chrome stays nib-specific so the line is still legibly ours.
//
// `total` is the full per-turn token footprint (fresh + cached +
// output) — opencode's headline number. Including it gives a single
// scalar for whole-session comparison while the ↑↓R breakdown
// surfaces where the cost actually lands.
func formatTurnUsage(u event.AgentTurnUsage, model string) string {
	var b strings.Builder
	b.WriteString("\n◇ ")
	if model != "" {
		b.WriteString(sanitizeInlineDisplay(model))
		b.WriteString(" · ")
	}
	fmt.Fprintf(&b, "turn %d", u.Turn)

	hasProvider := u.PromptTokens > 0 || u.CompletionTokens > 0 || u.CachedTokens > 0
	if hasProvider {
		total := u.PromptTokens + u.CachedTokens + u.CompletionTokens
		fmt.Fprintf(&b, " · ↑%s ↓%s",
			formatTokenCount(u.PromptTokens),
			formatTokenCount(u.CompletionTokens))
		if u.CachedTokens > 0 {
			grossInput := u.PromptTokens + u.CachedTokens
			pct := u.CachedTokens * 100 / grossInput
			fmt.Fprintf(&b, " R%s · %s total (%d%%⚡)",
				formatTokenCount(u.CachedTokens),
				formatTokenCount(total), pct)
		} else {
			fmt.Fprintf(&b, " · %s total", formatTokenCount(total))
		}
		if chip := formatContextChip(total, model); chip != "" {
			fmt.Fprintf(&b, " %s", chip)
		}
	} else {
		total := u.SystemEst + u.ToolsEst + u.HistoryEst + u.NewEst
		if total > 0 {
			// Estimate-only path: ~ marker glued to each number for
			// consistency with the session-summary line.
			fmt.Fprintf(&b, " · ↑~%s ↓~%s",
				formatTokenCount(total),
				formatTokenCount(u.CompletionEst))
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
// Matches the per-turn footer's pi-style vocabulary (↑input ↓output
// R<cacheRead>) so session-wide and per-turn rows speak one
// language. The `total` headline at the end is opencode-style: the
// full token footprint of the session, comparable in one scalar to
// anything else publishing the same number.
//
// `totalIn` is the GROSS input sum across turns (fresh + cached);
// fresh is derived as totalIn - totalCached. The optional
// model label drives the trailing `N%/<window>` chip that compares
// the most recent turn's footprint to the model's context window
// (matches pi's `4.0%/272k` display).
func formatSessionSummary(u usageState, model string) string {
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
		fresh := u.totalIn - u.totalCached
		fmt.Fprintf(&b, " · ↑%s%s ↓%s%s",
			prefix, formatTokenCount(fresh),
			prefix, formatTokenCount(u.totalOut))
		if u.totalCached > 0 {
			total := u.totalIn + u.totalOut
			pct := u.totalCached * 100 / u.totalIn
			fmt.Fprintf(&b, " R%s · %s%s total (%d%%⚡)",
				formatTokenCount(u.totalCached),
				prefix, formatTokenCount(total), pct)
		} else {
			total := u.totalIn + u.totalOut
			fmt.Fprintf(&b, " · %s%s total", prefix, formatTokenCount(total))
		}
		if chip := formatContextChip(u.lastTurnTotal, model); chip != "" {
			fmt.Fprintf(&b, " %s", chip)
		}
	}
	return b.String()
}

// inputHeight returns the number of rows reserved for the input area
// (separator + input + status). Uses 1/6 of the pane height, minimum 5.
