package truncation

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit"
)

// limit is an output limit that can be raised.
type limit struct{ max int }

func (l *limit) MaxTokens() int     { return l.max }
func (l *limit) SetMaxTokens(n int) { l.max = n }

func TestEscalated(t *testing.T) {
	for cur, want := range map[int]int{0: 32768, 4096: 32768, 20000: 40000, 40000: 65536, 65536: 65536, 131072: 131072} {
		if got := escalated(cur); got != want {
			t.Errorf("escalated(%d) = %d, want %d", cur, got, want)
		}
	}
}

// TestHooks_RetriesThenGivesUp: two cut-off responses in a row are
// retried, with the limit raised and each unfinished call answered; the
// third ends the run with an error.
func TestHooks_RetriesThenGivesUp(t *testing.T) {
	out := &limit{max: 4096}
	onTruncated := Hooks(out).OnTruncated
	calls := []llm.ToolCall{{ID: "a"}, {ID: "b"}}

	first, err := onTruncated(context.Background(), kit.TruncationInput{ToolCalls: calls, Consecutive: 1})
	if err != nil || !first.Retry || len(first.Messages) != 2 || first.Messages[0].Role != "tool" || first.Messages[1].ToolCallID != "b" {
		t.Fatalf("first cut-off = %+v, %v; want a retry with a reply per call", first, err)
	}
	if out.max != 32768 {
		t.Errorf("output limit = %d, want it raised to 32768", out.max)
	}
	if second, err := onTruncated(context.Background(), kit.TruncationInput{ToolCalls: calls, Consecutive: 2}); err != nil || !second.Retry || out.max != 65536 {
		t.Fatalf("second cut-off: %+v, %v, limit %d; want a retry at 65536", second, err, out.max)
	}
	third, err := onTruncated(context.Background(), kit.TruncationInput{ToolCalls: calls, Consecutive: 3})
	if err == nil || third.Retry || !strings.Contains(err.Error(), "3 responses in a row") {
		t.Fatalf("third cut-off = %+v, %v; want the turn given up with an error", third, err)
	}
}

// TestHooks_NoToolCallsAndNoLimit: with nothing to refuse, the model is
// told to continue; having no output limit to raise is not a problem.
func TestHooks_NoToolCallsAndNoLimit(t *testing.T) {
	res, err := Hooks(nil).OnTruncated(context.Background(), kit.TruncationInput{Consecutive: 1})
	if err != nil || !res.Retry || len(res.Messages) != 1 || res.Messages[0].Role != "user" {
		t.Errorf("result = %+v, %v; want a retry with one user message", res, err)
	}
}
