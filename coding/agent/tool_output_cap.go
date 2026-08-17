package agent

import (
	"fmt"
	"log/slog"
	"os"
	"unicode/utf8"

	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/ai/llm"
)

// capToolOutputsIfEnabled is the entry point the TransformContext hook
// calls. Reads [brand.EnvKeyToolOutputCapDisabled] each invocation so
// the kill switch can be toggled without restarting nib. Per-turn
// syscall is cheap (one os.Getenv per LLM call) and avoids stale
// state across long-running sessions.
//
// When the env var is set non-empty, returns the input slice
// unchanged (preserves uncapped baseline byte-for-byte). When unset,
// delegates to [capStaleToolResults] with the package-level constants.
func capToolOutputsIfEnabled(msgs []llm.Message) []llm.Message {
	if os.Getenv(brand.EnvKeyToolOutputCapDisabled) != "" {
		return msgs
	}
	return capStaleToolResults(msgs, toolCapKeepRecent, toolOutputCapBytes)
}

// Tool-output cap.
//
// Replaces oversize tool-result content in older turns with a short
// truncation marker so the conversation prefix stops re-shipping
// stale 30 KB read_file outputs on every subsequent turn. The LLM
// sees the full result on the turn the tool was called (and for
// [toolCapKeepRecent] user-turn boundaries after that); only
// older results are eligible.
//
// Cache stability is load-bearing — see /nib/plans/tool-output-cap.md
// for the math. Two invariants make per-turn capping compatible
// with prompt caching (the 30k threshold in [compactHistoryThreshold]
// already documents the same trade-off for compaction):
//
//  1. IDEMPOTENCY. Running [capStaleToolResults] twice on the same
//     slice returns a byte-identical result. The `len > maxBytes`
//     guard skips already-capped messages because the marker text
//     is bounded below `maxBytes` (enforced by the minToolOutputCap
//     floor); [truncationMarker] is a pure function of (content,
//     maxBytes) — no clock reads, no random IDs, no per-session
//     state.
//  2. BOUNDED NEW MUTATIONS PER TURN. Each new LLM call appends one
//     assistant message and zero-or-more tool results (one per
//     tool call in that turn's batch). The recent window advances
//     by exactly the batch size, so AT MOST batch-size previously
//     verbatim tool results newly enter the stale window in a
//     single turn. Typical agent traffic is one tool per turn —
//     one new mutation. Parallel-tool turns (e.g. multiple
//     read_file in one batch) can roll 2-3 results stale in one
//     pass; the cache invalidates from the oldest of those forward.
//     This is unavoidable while still respecting Pillar-6
//     extensibility (the cap can't know batch boundaries without
//     wiring through). Cost stays positive in expectation because
//     each newly capped result saves bytes on every subsequent
//     turn.

// toolOutputCapBytes is the byte cap for tool-result content in
// older turns. Set generously (4 KiB) on the conservative side of
// pi's 2 KB to preserve more useful context per result while still
// bounding the outliers (50 KB read_file, 80 KB build log) that
// drive prefix bloat. Tune empirically — start moderate, tighten if
// smoke testing shows headroom.
const toolOutputCapBytes = 4 * 1024

// toolCapKeepRecent is the number of most-recent tool-result
// messages to keep verbatim. 2 means the result the current
// assistant turn is reasoning about plus the one before it stay
// full-size; older tool messages are eligible for capping.
//
// Counted by tool-message position (not by user-message turns),
// because agentic sessions usually have ONE user message and then
// many assistant↔tool exchanges — a user-turn boundary would
// collapse to 0 and the cap would never fire. Tool-message
// position is the right granularity for "how recently was this
// result useful to the LLM."
const toolCapKeepRecent = 2

// truncationTailReserve is the byte budget reserved at the end of
// the truncation marker for the explanatory tail. 128 leaves
// comfortable headroom for the longest tail we produce (~96 bytes
// at 20-digit byte counts) without eating meaningfully into the
// preview budget.
const truncationTailReserve = 128

// minToolOutputCap is the smallest maxBytes value the cap accepts.
// Below this, the marker text cannot fit inside maxBytes and the
// idempotency invariant (a second pass leaves capped content alone)
// would break — a re-cap would fire on the marker itself,
// generating a different (smaller, deterministic but mutated)
// output every pass, which in turn would invalidate the prompt
// cache every turn. Treating sub-floor maxBytes as a no-op keeps
// the invariant structural rather than implicit.
const minToolOutputCap = 256

// capStaleToolResults replaces oversize tool-result content with a
// truncation marker for tool messages older than the most recent
// keepRecent tool-message boundaries. Pure function; returns the
// input slice unchanged when no message qualifies.
//
// Idempotent: running this twice on the same slice returns a
// byte-identical result, so the provider-side prompt cache stays
// stable across turns.
func capStaleToolResults(msgs []llm.Message, keepRecent, maxBytes int) []llm.Message {
	if len(msgs) == 0 || keepRecent < 0 || maxBytes < minToolOutputCap {
		return msgs
	}
	boundary := findKeepBoundary(msgs, keepRecent)
	if boundary <= 0 {
		return msgs
	}
	var (
		out         []llm.Message
		mutated     int
		bytesBefore int
		bytesAfter  int
	)
	for i := 0; i < boundary; i++ {
		m := msgs[i]
		if m.Role != "tool" || len(m.Content) <= maxBytes {
			continue
		}
		if out == nil {
			out = make([]llm.Message, len(msgs))
			copy(out, msgs)
		}
		marker := truncationMarker(m.Content, maxBytes)
		out[i] = llm.Message{
			Role:       "tool",
			ToolCallID: m.ToolCallID,
			Content:    marker,
		}
		mutated++
		bytesBefore += len(m.Content)
		bytesAfter += len(marker)
	}
	if out == nil {
		return msgs
	}
	// Log once per mutation pass so debug.log records when the cap
	// actually fires. INFO level because it's a meaningful state
	// change (history prefix mutated, cache invalidates from the
	// earliest mutation onward); rare enough not to be noisy.
	slog.Info("tool-output cap: mutated stale results",
		"mutated", mutated,
		"bytes_before", bytesBefore,
		"bytes_after", bytesAfter,
		"saved", bytesBefore-bytesAfter,
		"boundary", boundary,
		"total_msgs", len(msgs),
	)
	return out
}

// findKeepBoundary returns the EXCLUSIVE upper bound of the "stale"
// window: the index of the keepRecent-th most recent tool message
// — i.e., the FIRST message that stays verbatim. Callers iterate
// `for i := 0; i < boundary; i++` so every message at index <
// boundary is eligible for truncation and every message at index
// >= boundary is preserved.
//
// Example with keepRecent=2 and tool messages at indices 3, 5, 7:
// returns 5. Indices 0..4 are stale (index 3 is the only tool
// there, so it gets capped); indices 5 and 7 stay verbatim.
//
// Walks backward counting tool-role messages (not user messages,
// which would collapse to 0 on single-goal agentic sessions where
// only the initial goal carries role="user"). Returns 0 when the
// slice contains fewer than keepRecent tool messages — meaning the
// whole conversation is "recent" and nothing is eligible for
// truncation yet.
func findKeepBoundary(msgs []llm.Message, keepRecent int) int {
	if keepRecent <= 0 {
		return len(msgs)
	}
	seen := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "tool" {
			seen++
			if seen == keepRecent {
				return i
			}
		}
	}
	return 0
}

// truncationMarker is a PURE function of content and maxBytes. No
// clock reads, no random IDs, no per-session state. Same input →
// byte-identical output across calls, processes, and machines.
//
// The marker preserves the head of the content (first
// maxBytes-truncationTailReserve bytes, rounded down to a UTF-8
// boundary) followed by a literal explanation that names the
// original byte count and tells the LLM how to recover the rest.
// The byte-count guarantee is that len(marker) < maxBytes for any
// content that triggered truncation (len(content) > maxBytes), so
// re-running [capStaleToolResults] on the result is a no-op.
func truncationMarker(content string, maxBytes int) string {
	budget := max(maxBytes-truncationTailReserve, 0)
	preview := truncateAtRuneBoundary(content, budget)
	return fmt.Sprintf(
		"%s\n[truncated: %d original bytes, %d kept; re-call the tool to retrieve full content]",
		preview, len(content), len(preview))
}

// truncateAtRuneBoundary returns the longest prefix of s that fits
// in maxBytes bytes AND ends on a valid UTF-8 boundary. Splitting a
// multi-byte rune would produce an invalid string that some
// providers reject during JSON encode.
func truncateAtRuneBoundary(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	// Decode forward until the next rune would push past maxBytes.
	end := 0
	for end < maxBytes {
		_, size := utf8.DecodeRuneInString(s[end:])
		if size == 0 || end+size > maxBytes {
			break
		}
		end += size
	}
	return s[:end]
}
