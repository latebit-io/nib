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
	// Last-turn snapshot fields. Pi (web-ui/Messages.ts) renders
	// `formatUsage(this.message.usage)` per assistant message —
	// i.e., the LATEST turn's per-message breakdown, not the
	// session cumulative. nib's live displays (pane footer + status
	// bar) match that convention so a watcher sees the most recent
	// turn's shape change in real time rather than a slowly-growing
	// running total.
	lastTurnFresh  int // PromptTokens of the latest turn
	lastTurnOut    int // CompletionTokens of the latest turn
	lastTurnCached int // CachedTokens of the latest turn
	// lastTurnTotal is the latest turn's full footprint
	// (PromptTokens + CachedTokens + CompletionTokens). Used as the
	// numerator of the `N%/<window>` chip and bar viz — stands in
	// for "current context occupancy."
	lastTurnTotal int
	// runIn / runOut are the per-RUN gross input + output sums,
	// zeroed at each run boundary by [AgentPaneModel.BeginRun]. They
	// mirror the scope of the agent's per-task token budget gate,
	// which resets on every RunWithMode and Reply
	// ([coding/agent] lifecycle). The session totals (totalIn/totalOut)
	// span the whole conversation and would drift from the gate after
	// the first follow-up, so the budget indicator reads these instead.
	runIn  int
	runOut int
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
		m.usage.runIn += u.PromptTokens + u.CachedTokens
		m.usage.runOut += u.CompletionTokens
		m.usage.lastTurnFresh = u.PromptTokens
		m.usage.lastTurnOut = u.CompletionTokens
		m.usage.lastTurnCached = u.CachedTokens
		m.usage.lastTurnTotal = u.PromptTokens + u.CachedTokens + u.CompletionTokens
		m.usage.hasExact = true
	} else {
		est := u.SystemEst + u.ToolsEst + u.HistoryEst + u.NewEst
		m.usage.totalIn += est
		m.usage.totalOut += u.CompletionEst
		m.usage.runIn += est
		m.usage.runOut += u.CompletionEst
		m.usage.lastTurnFresh = est
		m.usage.lastTurnOut = u.CompletionEst
		m.usage.lastTurnCached = 0
		m.usage.lastTurnTotal = est + u.CompletionEst
	}
	m.usage.streamingChars = 0
	m.usage.streamingInputEst = 0
	m.usage.turns++
}

// UsageIndicator returns the status-bar indicator string for the
// main editor footer. Renders the context-occupancy bar with a
// `context window:` label so the global status row at a glance
// answers "how full is the model's working memory right now."
//
// The pane footer carries the per-turn ↑↓R numerical breakdown;
// the status bar is intentionally dedicated to the bar viz alone
// so the two surfaces don't compete. Empty string when no model
// is known or no usage has been recorded yet.
func (m *AgentPaneModel) UsageIndicator() string {
	bar := formatContextBar(m.usage.lastTurnTotal, m.modelLabel)
	if bar == "" {
		return ""
	}
	return "context window: " + bar
}

// ResetUsage clears accumulated usage for a new agent run.
func (m *AgentPaneModel) ResetUsage() {
	m.usage = usageState{}
}

// BeginRun zeroes the per-run spend counters that back the budget
// indicator. Called at each run boundary (every goal submission,
// including a follow-up that continues an existing conversation) so
// the indicator's scope matches the agent's per-task budget gate,
// which resets on every RunWithMode and Reply. The conversation-wide
// session totals are intentionally left untouched.
func (m *AgentPaneModel) BeginRun() {
	m.usage.runIn = 0
	m.usage.runOut = 0
}

// SetTaskTokenBudget records the armed per-task token cap (0 = the cap
// is disabled). Drives [AgentPaneModel.BudgetIndicator]; it must equal
// the value the agent resolves via [kit/budget.Resolve] so the
// status-bar percentage matches the gate that actually aborts the run.
func (m *AgentPaneModel) SetTaskTokenBudget(cap int) {
	m.taskTokenBudget = cap
}

// BudgetIndicator returns the status-bar segment reporting this run's
// cumulative token spend — the same prompt+cached+completion footprint
// the budget gate accumulates. When a cap is armed it shows progress
// toward it (`budget 1.2M/2M 60%`, flagged with ⚠ past 90%); with the
// cap disabled it shows the bare per-run tally (`tokens 1.2M`) so the
// runaway-cost signal survives even though nothing will abort the run.
// Empty string before any tokens land in the current run.
func (m *AgentPaneModel) BudgetIndicator() string {
	spent := m.usage.runIn + m.usage.runOut
	if spent <= 0 {
		return ""
	}
	if m.taskTokenBudget <= 0 {
		return "tokens " + formatTokenCount(spent)
	}
	pct := spent * 100 / m.taskTokenBudget
	seg := fmt.Sprintf("budget %s/%s %d%%",
		formatTokenCount(spent), formatRoundCount(m.taskTokenBudget), pct)
	if pct >= 90 {
		seg += " ⚠"
	}
	return seg
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

// formatRoundCount renders integer-friendly token counts (model
// window sizes are always round numbers; rendering them as "272k"
// instead of "272.0k" keeps the status-bar label compact and
// matches the convention users see in opencode/pi when window
// sizes are quoted.
func formatRoundCount(n int) string {
	switch {
	case n >= 1_000_000 && n%1_000_000 == 0:
		return fmt.Sprintf("%dM", n/1_000_000)
	case n >= 1000 && n%1000 == 0:
		return fmt.Sprintf("%dk", n/1000)
	default:
		// Non-round value — fall back to the standard compact form
		// so we never lose precision on an unexpected window size.
		return formatTokenCount(n)
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

// contextBarCells is the visual width of the context-occupancy
// bar. 20 cells = 5% per cell, granular enough to see 1-cell
// growth on a turn-by-turn basis at typical session sizes.
const contextBarCells = 20

// formatContextBar renders an ASCII bar visualization of context
// occupancy: ▓ for filled cells, ░ for empty, scaled to
// [contextBarCells]. Trailing label `X%/Yk` matches the chip
// elsewhere so the bar reads as a richer version of the same
// number. Empty string when no model is known or usage is zero
// (same guard as [formatContextChip]).
//
// Example: 16% of 272k →  "▓▓▓░░░░░░░░░░░░░░░░░ 16%/272.0k"
func formatContextBar(lastTurnTotal int, model string) string {
	if lastTurnTotal <= 0 || model == "" {
		return ""
	}
	window := modelContextWindow(model)
	if window <= 0 {
		return ""
	}
	filled := lastTurnTotal * contextBarCells / window
	if filled > contextBarCells {
		filled = contextBarCells
	}
	if filled < 0 {
		filled = 0
	}
	bar := strings.Repeat("▓", filled) + strings.Repeat("░", contextBarCells-filled)
	pct := lastTurnTotal * 100 / window
	return fmt.Sprintf("%s %d%% / %s", bar, pct, formatRoundCount(window))
}

// formatTurnStatsInline renders the per-turn stats chunk that
// trails a tool bullet: ` · ↑X ↓Y [R Z · N%⚡]`. Empty string when
// the turn has no provider data (estimate-only fallback handles
// this via formatTurnUsage's standalone path).
//
// Designed to fold into a `● tool_name` line so one row per turn
// carries both the tool and its cost — the visually-dense Option B
// layout the user picked. Matches the pi convention of putting
// usage stats inline with the message-level marker.
func formatTurnStatsInline(u event.AgentTurnUsage) string {
	hasProvider := u.PromptTokens > 0 || u.CompletionTokens > 0 || u.CachedTokens > 0
	if !hasProvider {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, " · ↑%s ↓%s",
		formatTokenCount(u.PromptTokens),
		formatTokenCount(u.CompletionTokens))
	if u.CachedTokens > 0 {
		grossInput := u.PromptTokens + u.CachedTokens
		pct := u.CachedTokens * 100 / grossInput
		fmt.Fprintf(&b, " R%s · %d%%⚡",
			formatTokenCount(u.CachedTokens), pct)
	}
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
		if bar := formatContextBar(u.lastTurnTotal, model); bar != "" {
			fmt.Fprintf(&b, " · %s", bar)
		}
	}
	return b.String()
}

// inputHeight returns the number of rows reserved for the input area
// (separator + input + status). Uses 1/6 of the pane height, minimum 5.
