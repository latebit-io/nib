package agent

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/latebit-io/nib/ai/llm"
)

// P1 — Most recent turn is never capped. A tool result attached to
// the current user turn stays verbatim no matter how large.
func TestCapStaleToolResults_RecentTurnNeverCapped(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", toolOutputCapBytes*4)
	msgs := []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "ask"},
		{Role: "assistant", Content: "ok"},
		{Role: "tool", ToolCallID: "tc1", Content: big},
	}
	out := capStaleToolResults(msgs, toolCapKeepRecent, toolOutputCapBytes)
	if out[3].Content != big {
		t.Errorf("recent tool message was modified (boundary should leave it alone)")
	}
}

// P2 — Older oversize tool results are replaced with the marker.
// With keepRecent=2 and three tool messages, the OLDEST sits in
// the stale window; the two newer ones stay verbatim. This matches
// the typical single-goal agentic loop (one user message, many
// tool↔assistant exchanges) which the production logs surfaced.
func TestCapStaleToolResults_OldOversizeCapped(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("y", toolOutputCapBytes*2)
	msgs := []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "the goal"},
		{Role: "assistant", Content: "a1"},
		{Role: "tool", ToolCallID: "oldest-tc", Content: big}, // idx 3 — stale
		{Role: "assistant", Content: "a2"},
		{Role: "tool", ToolCallID: "mid-tc", Content: big}, // idx 5 — keep
		{Role: "assistant", Content: "a3"},
		{Role: "tool", ToolCallID: "newest-tc", Content: big}, // idx 7 — keep
	}
	out := capStaleToolResults(msgs, toolCapKeepRecent, toolOutputCapBytes)
	if out[3].Content == big {
		t.Errorf("oldest tool result was NOT capped (should have been)")
	}
	if !strings.Contains(out[3].Content, "truncated:") {
		t.Errorf("capped result missing marker: %q", truncatedForLog(out[3].Content))
	}
	if out[5].Content != big {
		t.Errorf("mid tool result was capped; should be inside recent-window")
	}
	if out[7].Content != big {
		t.Errorf("newest tool result was capped; should be inside recent-window")
	}
	if out[3].ToolCallID != "oldest-tc" {
		t.Errorf("ToolCallID lost during cap: %q", out[3].ToolCallID)
	}
}

// P3 — Older small tool results are NOT capped. The cap only fires
// above the byte threshold; small results are cheap to keep.
func TestCapStaleToolResults_OldSmallNotCapped(t *testing.T) {
	t.Parallel()
	small := "task complete"
	msgs := []llm.Message{
		{Role: "user", Content: "u1"},
		{Role: "assistant", Content: "a1"},
		{Role: "tool", ToolCallID: "small-tc", Content: small},
		{Role: "user", Content: "u2"},
		{Role: "assistant", Content: "a2"},
		{Role: "tool", ToolCallID: "small-tc-2", Content: small},
		{Role: "user", Content: "u3 (current)"},
	}
	out := capStaleToolResults(msgs, toolCapKeepRecent, toolOutputCapBytes)
	if !reflect.DeepEqual(out, msgs) {
		t.Errorf("small tool results triggered a copy/mutation; should be no-op")
	}
}

// P4 — Marker text is parseable and self-describing. The LLM gets a
// clear signal that truncation happened and how to recover.
func TestCapStaleToolResults_MarkerTellsLLMHowToRecover(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("z", toolOutputCapBytes*3)
	marker := truncationMarker(content, toolOutputCapBytes)
	for _, want := range []string{"truncated", "original bytes", "re-call"} {
		if !strings.Contains(marker, want) {
			t.Errorf("marker missing %q: %q", want, truncatedForLog(marker))
		}
	}
	// The marker also embeds the original byte count so the LLM can
	// judge how much was lost.
	if !strings.Contains(marker, "12288") { // 4096 * 3
		t.Errorf("marker missing original byte count: %q", truncatedForLog(marker))
	}
}

// P5 — Truncation is rune-safe. A multi-byte rune at the boundary
// must not be split. The Go provider rejects invalid UTF-8 on JSON
// encode, and so do most upstream APIs.
func TestTruncateAtRuneBoundary_NeverSplitsRunes(t *testing.T) {
	t.Parallel()
	// Build a string of 4-byte runes (😀 = U+1F600). At byte budgets
	// that fall in the middle of a rune, we must drop the rune
	// entirely rather than ship invalid UTF-8.
	s := strings.Repeat("\U0001F600", 100) // 400 bytes total
	for budget := 0; budget < 16; budget++ {
		out := truncateAtRuneBoundary(s, budget)
		if !utf8.ValidString(out) {
			t.Errorf("budget=%d produced invalid UTF-8: %q", budget, truncatedForLog(out))
		}
		if len(out) > budget {
			t.Errorf("budget=%d, len(out)=%d (over budget)", budget, len(out))
		}
	}
	// A budget large enough for whole runes should keep complete runes.
	out := truncateAtRuneBoundary(s, 12) // 3 emojis = 12 bytes
	if utf8.RuneCountInString(out) != 3 {
		t.Errorf("budget=12: got %d runes (4 bytes each), want 3", utf8.RuneCountInString(out))
	}
}

// P9 — Idempotency. Running [capStaleToolResults] twice on the same
// slice returns byte-identical output. This is the cache-stability
// prerequisite: turn N+1's prefix at unchanged positions must match
// turn N's prefix byte-for-byte for the provider's prompt cache to
// hit.
func TestCapStaleToolResults_Idempotent(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("a", toolOutputCapBytes*3)
	msgs := []llm.Message{
		{Role: "user", Content: "u1"},
		{Role: "assistant", Content: "a1"},
		{Role: "tool", ToolCallID: "t1", Content: big},
		{Role: "user", Content: "u2"},
		{Role: "assistant", Content: "a2"},
		{Role: "tool", ToolCallID: "t2", Content: big},
		{Role: "user", Content: "u3"},
		{Role: "assistant", Content: "a3"},
		{Role: "tool", ToolCallID: "t3", Content: big},
		{Role: "user", Content: "u4 (current)"},
	}
	once := capStaleToolResults(msgs, toolCapKeepRecent, toolOutputCapBytes)
	twice := capStaleToolResults(once, toolCapKeepRecent, toolOutputCapBytes)
	if !reflect.DeepEqual(once, twice) {
		t.Fatalf("not idempotent: once != twice")
	}
	// Critical: the capped messages' content must equal byte-for-byte
	// between the two runs. Even a one-byte drift (e.g. the marker
	// embedding a turn counter) would break the cache every turn.
	for i := range once {
		if once[i].Content != twice[i].Content {
			t.Errorf("byte mismatch at i=%d: once=%q twice=%q",
				i, truncatedForLog(once[i].Content), truncatedForLog(twice[i].Content))
		}
	}
}

// P10 — Marker determinism. truncationMarker is a pure function of
// (content, maxBytes). Cross-call determinism is what makes
// Invariant A (idempotency) hold structurally; this test pins it
// at the function level.
func TestTruncationMarker_Deterministic(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("hello world ", 1000)
	a := truncationMarker(content, toolOutputCapBytes)
	b := truncationMarker(content, toolOutputCapBytes)
	if a != b {
		t.Errorf("non-deterministic marker:\n a=%q\n b=%q",
			truncatedForLog(a), truncatedForLog(b))
	}
	// Critically — the marker must be SMALLER than maxBytes so a
	// second pass doesn't re-truncate it (would defeat idempotency).
	if len(a) > toolOutputCapBytes {
		t.Errorf("marker (%d bytes) > maxBytes (%d) — re-cap would fire",
			len(a), toolOutputCapBytes)
	}
}

// At-most-one new mutation per turn (Invariant B). Simulate turn N
// then turn N+1 (one new assistant↔tool exchange appended). Exactly
// one previously-verbatim oversize tool result newly enters the
// stale window — and it's the one that should be mutated. The
// previously-capped one must NOT be re-mutated (idempotency).
func TestCapStaleToolResults_OneNewMutationPerTurn(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("b", toolOutputCapBytes*3)
	// Turn N — three tool results; with keepRecent=2 only t1 is
	// in the stale window.
	turnN := []llm.Message{
		{Role: "user", Content: "the goal"},
		{Role: "assistant", Content: "a1"},
		{Role: "tool", ToolCallID: "t1", Content: big}, // idx 2 — stale
		{Role: "assistant", Content: "a2"},
		{Role: "tool", ToolCallID: "t2", Content: big}, // idx 4 — keep
		{Role: "assistant", Content: "a3"},
		{Role: "tool", ToolCallID: "t3", Content: big}, // idx 6 — keep
	}
	outN := capStaleToolResults(turnN, toolCapKeepRecent, toolOutputCapBytes)
	if outN[2].Content == big || outN[4].Content != big || outN[6].Content != big {
		t.Fatalf("turn N setup not as expected: t1 should be capped, t2/t3 verbatim")
	}

	// Turn N+1 — append one new assistant↔tool exchange. t2 rolls
	// from "keep" into the stale window.
	turnNext := append([]llm.Message{}, outN...)
	turnNext = append(turnNext,
		llm.Message{Role: "assistant", Content: "a4"},
		llm.Message{Role: "tool", ToolCallID: "t4", Content: big},
	)
	outNext := capStaleToolResults(turnNext, toolCapKeepRecent, toolOutputCapBytes)

	// IDEMPOTENCY: t1 was already capped at turn N. Its content at
	// turn N+1 must be byte-identical (cache hit at that position).
	if outN[2].Content != outNext[2].Content {
		t.Errorf("previously-capped t1 mutated between turns — would break cache")
	}
	// AT-MOST-ONE-NEW: t2 was verbatim at turn N; must be capped at N+1.
	if outNext[4].Content == big {
		t.Errorf("t2 not capped after rolling into stale window")
	}
	// t3 and t4 are now the two most-recent tool messages — keep verbatim.
	if outNext[6].Content != big {
		t.Errorf("t3 incorrectly capped while still in recent window")
	}
	if outNext[8].Content != big {
		t.Errorf("t4 (newest) incorrectly capped")
	}
}

// Boundary edge: keepRecent=2 and only 2 tool messages total means
// the entire conversation is "recent" — nothing capped, slice is
// returned unmodified.
func TestCapStaleToolResults_NoOpWhenFewerToolMessagesThanWindow(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("c", toolOutputCapBytes*4)
	msgs := []llm.Message{
		{Role: "user", Content: "the goal"},
		{Role: "assistant", Content: "a1"},
		{Role: "tool", ToolCallID: "t1", Content: big},
		{Role: "assistant", Content: "a2"},
		{Role: "tool", ToolCallID: "t2", Content: big},
	}
	out := capStaleToolResults(msgs, toolCapKeepRecent, toolOutputCapBytes)
	if &out[0] != &msgs[0] {
		t.Errorf("no-op should return the input slice; got a fresh copy")
	}
}

// Non-tool messages in the stale window are left alone — capping
// only applies to role="tool".
func TestCapStaleToolResults_OnlyToolMessagesAffected(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("d", toolOutputCapBytes*4)
	msgs := []llm.Message{
		{Role: "user", Content: big}, // huge user input, kept verbatim
		{Role: "assistant", Content: big},
		{Role: "tool", ToolCallID: "t1", Content: big}, // idx 2 — stale
		{Role: "assistant", Content: "a2"},
		{Role: "tool", ToolCallID: "t2", Content: big}, // idx 4 — keep
		{Role: "assistant", Content: "a3"},
		{Role: "tool", ToolCallID: "t3", Content: big}, // idx 6 — keep
	}
	out := capStaleToolResults(msgs, toolCapKeepRecent, toolOutputCapBytes)
	if out[0].Content != big {
		t.Errorf("user message was modified (cap should only touch tool messages)")
	}
	if out[1].Content != big {
		t.Errorf("assistant message was modified (cap should only touch tool messages)")
	}
	if out[2].Content == big {
		t.Errorf("oldest tool message NOT capped")
	}
}

// Zero / negative / sub-floor inputs are accepted as no-ops rather
// than panics. The sub-floor cases (maxBytes < minToolOutputCap)
// are critical: below the floor the marker text wouldn't fit
// inside maxBytes and a second pass would re-cap the marker
// itself, generating a different output every turn and breaking
// the prompt cache. The floor guard makes that impossible by
// declining to cap at all.
func TestCapStaleToolResults_DegenerateInputs(t *testing.T) {
	t.Parallel()
	msgs := []llm.Message{{Role: "user", Content: "x"}}
	tests := []struct {
		name      string
		msgs      []llm.Message
		keepTurns int
		maxBytes  int
	}{
		{"empty slice", nil, 2, 4096},
		{"empty slice literal", []llm.Message{}, 2, 4096},
		{"negative keepTurns", msgs, -1, 4096},
		{"zero maxBytes", msgs, 2, 0},
		{"negative maxBytes", msgs, 2, -100},
		{"maxBytes 1 (sub-floor)", msgs, 2, 1},
		{"maxBytes 127 (just under truncationTailReserve)", msgs, 2, 127},
		{"maxBytes 255 (one below minToolOutputCap)", msgs, 2, 255},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := capStaleToolResults(tt.msgs, tt.keepTurns, tt.maxBytes)
			if !reflect.DeepEqual(out, tt.msgs) {
				t.Errorf("degenerate input should be no-op; got %v, want %v", out, tt.msgs)
			}
		})
	}
}

// Sub-floor maxBytes on a payload that WOULD trigger capping at a
// healthy maxBytes is the precise case where a naive implementation
// breaks idempotency. The floor guard turns this into a no-op
// instead, preserving cache stability for any caller that hands in
// an undersized cap (a misconfiguration or a future
// per-tool-override that picks too aggressive a value).
func TestCapStaleToolResults_SubFloorMaxBytesIsNoOp(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("z", toolOutputCapBytes*2)
	msgs := []llm.Message{
		{Role: "user", Content: "the goal"},
		{Role: "assistant", Content: "a1"},
		{Role: "tool", ToolCallID: "t1", Content: big},
		{Role: "assistant", Content: "a2"},
		{Role: "tool", ToolCallID: "t2", Content: big},
		{Role: "assistant", Content: "a3"},
		{Role: "tool", ToolCallID: "t3", Content: big},
	}
	out := capStaleToolResults(msgs, toolCapKeepRecent, minToolOutputCap-1)
	if !reflect.DeepEqual(out, msgs) {
		t.Errorf("sub-floor maxBytes should produce a no-op even with cap-eligible payload")
	}
}

// Multi-tool batch: when the previous LLM call emitted N>1 tool
// calls, N tool results land in history in one turn. If keepRecent
// is smaller than the batch, the oldest results of the batch (plus
// any pre-existing kept-but-now-stale ones) all newly enter the
// stale window in a single pass. Greptile P2 #3: the "at most one
// mutation per turn" framing was overstated; the test pins the
// real behaviour so future changes can't quietly re-introduce the
// stronger (false) claim.
func TestCapStaleToolResults_MultiToolBatchRollsMultipleStale(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("m", toolOutputCapBytes*2)
	// Turn N pre-batch state: two existing tool results, both kept
	// (within recent window with keepRecent=2).
	turnN := []llm.Message{
		{Role: "user", Content: "the goal"},
		{Role: "assistant", Content: "a1"},
		{Role: "tool", ToolCallID: "t1", Content: big}, // idx 2 — kept at turn N
		{Role: "assistant", Content: "a2"},
		{Role: "tool", ToolCallID: "t2", Content: big}, // idx 4 — kept at turn N
	}
	outN := capStaleToolResults(turnN, toolCapKeepRecent, toolOutputCapBytes)
	if !reflect.DeepEqual(outN, turnN) {
		t.Fatalf("turn N: with only 2 tool messages and keepRecent=2, expected no-op")
	}

	// Turn N+1: assistant emits a batch of 3 tool calls. History
	// gains assistant + 3 tools.
	turnNext := append([]llm.Message{}, turnN...)
	turnNext = append(turnNext,
		llm.Message{Role: "assistant", Content: "a3 (batch)"},
		llm.Message{Role: "tool", ToolCallID: "t3", Content: big},
		llm.Message{Role: "tool", ToolCallID: "t4", Content: big},
		llm.Message{Role: "tool", ToolCallID: "t5", Content: big}, // newest
	)
	outNext := capStaleToolResults(turnNext, toolCapKeepRecent, toolOutputCapBytes)

	// With 5 tool messages and keepRecent=2, indices 0..6 are
	// stale (boundary lands at the 2nd-newest tool, t4 @ idx 7).
	// That means t1, t2, AND t3 newly mutate this turn — the
	// "bounded" not "single" invariant.
	if outNext[2].Content == big {
		t.Errorf("t1 not capped (should be — fell out of recent window)")
	}
	if outNext[4].Content == big {
		t.Errorf("t2 not capped (should be — fell out of recent window)")
	}
	if outNext[6].Content == big {
		t.Errorf("t3 not capped (should be — pushed out by t4/t5 in the same batch)")
	}
	if outNext[7].Content != big {
		t.Errorf("t4 was capped; should be kept (2nd most recent)")
	}
	if outNext[8].Content != big {
		t.Errorf("t5 (newest) was capped; should always be kept")
	}
	// Critical for cache stability: the second pass must reproduce
	// turn N+1's output byte-for-byte (idempotency) — three
	// newly-capped messages don't break idempotency, only the
	// "one mutation" framing.
	again := capStaleToolResults(outNext, toolCapKeepRecent, toolOutputCapBytes)
	if !reflect.DeepEqual(outNext, again) {
		t.Errorf("multi-tool-batch path not idempotent — cache would thrash")
	}
}

// truncatedForLog snips long error messages so test failures stay
// readable when the actual content is multi-KB.
func truncatedForLog(s string) string {
	if len(s) <= 200 {
		return s
	}
	return s[:200] + "…"
}
