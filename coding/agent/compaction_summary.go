package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/latebit-io/nib/ai/llm"
)

// keepRecentTokens is the size of the verbatim tail [Agent.maybeCompact]
// preserves when Tier 2 summarization fires. Anything older than this
// boundary gets collapsed into a single summary message. Sized to keep
// at least the developer's most recent goal + a few turns of agent
// response available to the model without re-querying.
const keepRecentTokens = 10_000

// summarizationThreshold is the post-Tier-1 history token count at or
// above which Tier 2 fires. Set higher than [compactHistoryThreshold]
// so the extra LLM round-trip is only paid when Tier 1 alone couldn't
// keep history under control (Q&A-heavy sessions, deep tool chains
// with small individual results that Tier 1 cannot shrink).
const summarizationThreshold = 30_000

// summaryPrefix is the prefix tag the summarization message carries so
// future readers (the next LLM turn, debug logs, the developer
// scrolling back) can identify it as a compacted summary rather than a
// verbatim agent response. Stable text so tests can match against it.
const summaryPrefix = "[Conversation summary — earlier turns compacted to save context]"

// summarizationSystemPrompt is the system prompt the summarizer sees.
// Kept short and stable so the cacheable prefix on the underlying
// provider call stays warm across summarizations within a session.
const summarizationSystemPrompt = `You compress multi-turn conversations between a developer and a coding agent into short, accurate summaries. You preserve concrete details (file paths, function names, commands, decisions) and discard pleasantries.`

// summarizationUserPromptTemplate frames the transcript for the
// summarizer. The placeholder is the rendered transcript.
const summarizationUserPromptTemplate = `Summarize the conversation below for an agent that will keep working from where it left off. The agent has lost everything older than the summary, so the summary IS the agent's only memory of these turns.

Cover, in this order, only what applies:

- The developer's high-level intent / goal for this session.
- Decisions made (architecture, approach, file structure).
- Files read, written, or edited — preserve EXACT paths.
- Commands run that matter (builds, tests, formatters) and their outcomes.
- Errors hit and how they were resolved, or that they remain open.
- The most recent unresolved question or next step, if any.

Be concrete. Do not invent details. Skip sections with nothing to say. Keep it under 500 words. Do not add a preamble or a sign-off; output the summary directly.

--- CONVERSATION ---
%s
`

// errSummarizeNothing is returned by [splitForSummarization] when the
// message slice has no user-boundary at which the old range would
// contain anything worth summarizing. Treated as a soft-degrade in
// [Agent.maybeCompact]: not an error, just "nothing to do."
var errSummarizeNothing = errors.New("compaction: nothing to summarize")

// splitForSummarization finds the index [splitIdx] in messages such
// that messages[1:splitIdx] is the range to summarize and
// messages[splitIdx:] is the verbatim recent tail.
//
// The split lands on either a "user" message OR an "assistant" message
// — never on a "tool" message. This invariant prevents the new range
// from beginning with an orphan tool_result whose matching tool_call
// is in the (now-summarized) old range. The foundation appends all
// tool_result messages for a given assistant before the next assistant
// message, so an assistant-boundary cut is always preceded by either a
// tool_result, a user message, or another assistant — all of which
// leave the old range cleanly closed and the new range cleanly opened.
//
// Walks from the end of messages backward, accumulating estimated
// tokens until at least [keepRecentTokens] have been seen. Once the
// budget is met, picks the latest user-message boundary if one exists
// past the cutoff; otherwise falls back to the latest assistant
// boundary. The user-preferred / assistant-fallback ordering matters
// for autonomous-mode sessions where the only user message is the
// initial goal at index 1 — without the fallback, those sessions
// could never summarize.
//
// Returns (splitIdx, nil) when a viable split exists with
// splitIdx > 1 (non-empty old range), or (0, errSummarizeNothing) when
// the slice is too short, no boundary past the cutoff is valid, or
// the only boundary lands at index 1 (would leave an empty old range).
//
// The msgs slice is assumed to begin with the system prompt at index 0
// (never split); when len(msgs) < 3 the function returns
// errSummarizeNothing immediately.
func splitForSummarization(msgs []llm.Message) (int, error) {
	if len(msgs) < 3 {
		return 0, errSummarizeNothing
	}
	running := 0
	userSplit, asstSplit := -1, -1
	for i := len(msgs) - 1; i >= 1; i-- {
		running += approxMessageTokens(msgs[i])
		if running < keepRecentTokens {
			// Still inside the recent-tail budget — never split here.
			continue
		}
		switch msgs[i].Role {
		case "user":
			if userSplit < 0 {
				userSplit = i
			}
		case "assistant":
			if asstSplit < 0 {
				asstSplit = i
			}
		}
		// User-boundary preferred. The first user we hit past the
		// cutoff is the latest valid one; no need to keep walking.
		if userSplit > 1 {
			break
		}
	}
	splitIdx := userSplit
	if splitIdx <= 1 && asstSplit > 1 {
		splitIdx = asstSplit
	}
	if splitIdx <= 1 {
		return 0, errSummarizeNothing
	}
	return splitIdx, nil
}

// approxMessageTokens approximates the token cost of a single message
// for the purposes of summarization split decisions. Mirrors the
// 4-bytes-per-token heuristic [llm.EstimateMessageTokens] uses for
// content; intentionally cheap and per-message because we do not need
// the full estimator's role/tool-def accounting here.
func approxMessageTokens(m llm.Message) int {
	bytes := len(m.Content)
	for _, tc := range m.ToolCalls {
		bytes += len(tc.Function.Name) + len(tc.Function.Arguments)
	}
	// 4 bytes ≈ 1 token is the same coarse ratio
	// [llm.EstimateMessageTokens] applies; tested-against in practice
	// so the split decision is consistent with the threshold check.
	return bytes/4 + 4 // +4 for role/structural overhead per message
}

// renderTranscript formats msgs as a plain-text transcript for the
// summarizer to read. Each message becomes a labeled block; tool calls
// inline beneath the assistant message that issued them; tool_call_id
// is preserved so the summarizer can correlate calls and results in
// the same transcript.
func renderTranscript(msgs []llm.Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		sb.WriteString("\n### ")
		sb.WriteString(strings.ToUpper(m.Role))
		if m.ToolCallID != "" {
			fmt.Fprintf(&sb, " (tool_call_id=%s)", m.ToolCallID)
		}
		sb.WriteString("\n")
		if m.Content != "" {
			sb.WriteString(m.Content)
			sb.WriteString("\n")
		}
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&sb, "[tool_call id=%s name=%s args=%s]\n",
				tc.ID, tc.Function.Name, tc.Function.Arguments)
		}
	}
	return sb.String()
}

// runSummarization makes a single non-streaming LLM call against
// provider to compress the transcript into a summary. Returns the
// trimmed summary text (sans prefix) on success.
//
// Errors propagate so the caller can soft-degrade. The provider call
// uses no tool definitions and no streaming consumers — the agent's
// session-level accounting (turn usage, budget) intentionally does
// NOT see this call, treating summarization as overhead outside the
// developer's per-turn budget.
func runSummarization(ctx context.Context, provider llm.Provider, transcript string) (string, error) {
	if provider == nil {
		return "", fmt.Errorf("compaction: nil provider")
	}
	req := []llm.Message{
		{Role: "system", Content: summarizationSystemPrompt},
		{Role: "user", Content: fmt.Sprintf(summarizationUserPromptTemplate, transcript)},
	}
	ch, err := provider.Stream(ctx, req, nil)
	if err != nil {
		return "", fmt.Errorf("compaction: stream open: %w", err)
	}
	var sb strings.Builder
	for evt := range ch {
		if evt.Token != "" {
			sb.WriteString(evt.Token)
		}
		if evt.Truncated {
			// A truncated summarization output is still usable — the
			// model just ran into the output cap before finishing.
			// Log but accept whatever text we got.
			slog.Warn("compaction: summarization truncated by output cap",
				"bytes_received", sb.Len())
		}
	}
	text := strings.TrimSpace(sb.String())
	if text == "" {
		return "", fmt.Errorf("compaction: empty summary")
	}
	return text, nil
}
