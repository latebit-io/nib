package agent

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
)

// TestProviderProxy_LogTurnUsage_EmitsPerTurnBillingLine verifies
// that logTurnUsage produces the structured INFO line a downstream
// billing-comparison harness can grep + parse. The line shape is
// load-bearing for the apples-to-apples comparison effort (vs
// opencode / pi); regressions here break the public methodology
// promise.
func TestProviderProxy_LogTurnUsage_EmitsPerTurnBillingLine(t *testing.T) {
	pp := &providerProxy{}
	pp.recordUsage(&llm.Usage{PromptTokens: 1000, CachedTokens: 800, CompletionTokens: 50})

	buf := captureSlog(t)
	pp.logTurnUsage(2, 1200, 1000, 75, 1)

	got := decodeLastLog(t, buf)
	if got["msg"] != "provider usage: turn settled" {
		t.Errorf("msg = %v, want %q", got["msg"], "provider usage: turn settled")
	}

	wantPerTurn := map[string]float64{
		"turn":              2,
		"prompt_tokens":     1200,
		"cached_tokens":     1000,
		"uncached_tokens":   200, // prompt - cached
		"completion_tokens": 75,
		"tool_calls":        1,
	}
	for k, want := range wantPerTurn {
		if got[k] != want {
			t.Errorf("%s = %v, want %v", k, got[k], want)
		}
	}

	// Session totals must include the prior recorded turn (1000/800/50)
	// — the LAST log line of a run is the billing summary, so totals
	// have to be live.
	if got["session_prompt_tokens"] != float64(1000) {
		t.Errorf("session_prompt_tokens = %v, want 1000", got["session_prompt_tokens"])
	}
	if got["session_cached_tokens"] != float64(800) {
		t.Errorf("session_cached_tokens = %v, want 800", got["session_cached_tokens"])
	}
	if got["session_completion_tokens"] != float64(50) {
		t.Errorf("session_completion_tokens = %v, want 50", got["session_completion_tokens"])
	}
	if got["session_turns"] != float64(1) {
		t.Errorf("session_turns = %v, want 1", got["session_turns"])
	}
}

// TestProviderProxy_LogTurnUsage_ClampsNegativeUncached verifies the
// defensive clamp: if a provider mis-reports cached > prompt, the
// uncached field must NOT go negative (would distort cumulative
// billing) and the anomaly must be surfaced as a WARN so it can be
// caught in trace review.
func TestProviderProxy_LogTurnUsage_ClampsNegativeUncached(t *testing.T) {
	pp := &providerProxy{}
	buf := captureSlog(t)
	pp.logTurnUsage(1, 500, 700, 25, 0) // cached > prompt (provider bug)

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("want 2 log lines (WARN + INFO), got %d", len(lines))
	}

	var warn map[string]any
	if err := json.Unmarshal(lines[0], &warn); err != nil {
		t.Fatalf("parse warn: %v", err)
	}
	if warn["level"] != "WARN" {
		t.Errorf("first line should be WARN, got level=%v", warn["level"])
	}
	if !strings.Contains(warn["msg"].(string), "cached > prompt") {
		t.Errorf("warn msg missing anomaly description: %v", warn["msg"])
	}

	var info map[string]any
	if err := json.Unmarshal(lines[1], &info); err != nil {
		t.Fatalf("parse info: %v", err)
	}
	if info["uncached_tokens"] != float64(0) {
		t.Errorf("uncached_tokens should be clamped to 0; got %v", info["uncached_tokens"])
	}
}

// TestProviderProxy_LogTurnUsage_NilProviderUsageStillLogs verifies
// the per-turn boundary is preserved even when the provider does
// not report usage (e.g. an adapter we haven't fully wired). All
// fields are zeros, but the line still appears so the grep-based
// tally can still count turns.
func TestProviderProxy_LogTurnUsage_NilProviderUsageStillLogs(t *testing.T) {
	pp := &providerProxy{}
	buf := captureSlog(t)
	pp.logTurnUsage(1, 0, 0, 0, 0)

	got := decodeLastLog(t, buf)
	if got["msg"] != "provider usage: turn settled" {
		t.Errorf("missing per-turn line for nil-usage provider")
	}
	if got["prompt_tokens"] != float64(0) {
		t.Errorf("prompt_tokens = %v, want 0", got["prompt_tokens"])
	}
}

// captureSlog replaces the default slog logger with a JSON handler
// writing to a buffer, restoring the prior logger at test end.
// JSON-formatted output is what TestProviderProxy_LogTurnUsage*
// assertions parse against.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	buf := &bytes.Buffer{}
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	return buf
}

// decodeLastLog parses the final JSON record in buf so multi-line
// captures (WARN + INFO) can still extract the INFO line at the
// end. Fails the test on parse error.
func decodeLastLog(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	raw := bytes.TrimSpace(buf.Bytes())
	if len(raw) == 0 {
		t.Fatalf("no log output captured")
	}
	lines := bytes.Split(raw, []byte("\n"))
	last := lines[len(lines)-1]
	var got map[string]any
	if err := json.Unmarshal(last, &got); err != nil {
		t.Fatalf("decode: %v\nraw: %s", err, last)
	}
	return got
}

// TestProviderProxy_RecordUsage_CountsTurnWithoutUsage: a provider that
// omits Usage on Done must still advance the turn counter — MaxTurns and
// per-turn labels depend on it.
func TestProviderProxy_RecordUsage_CountsTurnWithoutUsage(t *testing.T) {
	pp := &providerProxy{}
	if got := pp.recordUsage(nil); got != 1 {
		t.Fatalf("recordUsage(nil) turn = %d, want 1", got)
	}
	if got := pp.recordUsage(&llm.Usage{PromptTokens: 5}); got != 2 {
		t.Fatalf("second recordUsage turn = %d, want 2", got)
	}
	snap := pp.Snapshot()
	if snap.Turns != 2 || snap.TotalPromptTokens != 5 {
		t.Fatalf("snapshot = %+v, want Turns=2 PromptTokens=5", snap)
	}
}
