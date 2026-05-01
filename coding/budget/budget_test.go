package budget

import (
	"strings"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
)

func TestResolve(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"zero falls back to default", 0, DefaultTaskTokens},
		{"negative is unlimited", -1, 0},
		{"large negative is unlimited", -1_000_000, 0},
		{"positive passes through", 500, 500},
		{"large positive passes through", 10_000_000, 10_000_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Resolve(tc.in); got != tc.want {
				t.Errorf("Resolve(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestTurn_AddUsage(t *testing.T) {
	t.Parallel()

	t.Run("nil usage is a no-op", func(t *testing.T) {
		t.Parallel()
		var tu Turn
		tu.AddUsage(nil)
		if tu != (Turn{}) {
			t.Errorf("Turn mutated by nil usage: %+v", tu)
		}
	})

	t.Run("accumulates across calls", func(t *testing.T) {
		t.Parallel()
		var tu Turn
		tu.AddUsage(&llm.Usage{PromptTokens: 100, CompletionTokens: 50, CachedTokens: 20})
		tu.AddUsage(&llm.Usage{PromptTokens: 30, CompletionTokens: 10, CachedTokens: 5})
		if tu.PromptTokens != 130 {
			t.Errorf("PromptTokens = %d, want 130", tu.PromptTokens)
		}
		if tu.CompletionTokens != 60 {
			t.Errorf("CompletionTokens = %d, want 60", tu.CompletionTokens)
		}
		if tu.CachedTokens != 25 {
			t.Errorf("CachedTokens = %d, want 25", tu.CachedTokens)
		}
	})
}

func TestWouldExceed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		committed Session
		pending   Turn
		limit     int
		want      bool
	}{
		{
			name:    "limit zero disables gate",
			pending: Turn{PromptTokens: 1_000_000},
			limit:   0,
			want:    false,
		},
		{
			name:    "negative limit disables gate",
			pending: Turn{PromptTokens: 1_000_000},
			limit:   -1,
			want:    false,
		},
		{
			name:      "committed alone exceeds — fires",
			committed: Session{TotalPromptTokens: 200},
			pending:   Turn{},
			limit:     100,
			want:      true,
		},
		{
			name:      "committed under, pending pushes over — fires",
			committed: Session{TotalPromptTokens: 60},
			pending:   Turn{PromptTokens: 50}, // 60+50 = 110 > 100
			limit:     100,
			want:      true,
		},
		{
			// Boundary: committed + pending == limit. The gate uses >=
			// (not >) so the cap value itself is over the line. Without
			// this row, a refactor flipping the comparator to > would
			// silently let one extra Stream call through.
			name:      "committed + pending exactly at cap — fires",
			committed: Session{TotalPromptTokens: 50},
			pending:   Turn{PromptTokens: 50}, // 50+50 = 100 == 100
			limit:     100,
			want:      true,
		},
		{
			name:    "completion tokens count too",
			pending: Turn{CompletionTokens: 150},
			limit:   100,
			want:    true,
		},
		{
			name:      "under cap returns false",
			committed: Session{TotalPromptTokens: 200},
			pending:   Turn{PromptTokens: 200},
			limit:     1000,
			want:      false,
		},
		{
			name:      "cached tokens are not added separately",
			committed: Session{TotalCachedTokens: 1000},
			limit:     100,
			want:      false, // cached alone does not push over
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := WouldExceed(tc.committed, tc.pending, tc.limit); got != tc.want {
				t.Errorf("WouldExceed(%+v, %+v, %d) = %v, want %v",
					tc.committed, tc.pending, tc.limit, got, tc.want)
			}
		})
	}
}

func TestExceeded(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		committed   Session
		limit       int
		wantMsg     bool
		mustContain []string
	}{
		{
			name:      "limit zero returns no message",
			committed: Session{TotalPromptTokens: 1_000_000},
			limit:     0,
			wantMsg:   false,
		},
		{
			name:      "negative limit returns no message",
			committed: Session{TotalPromptTokens: 1_000_000},
			limit:     -1,
			wantMsg:   false,
		},
		{
			name:      "under cap returns no message",
			committed: Session{TotalPromptTokens: 500},
			limit:     1000,
			wantMsg:   false,
		},
		{
			name:      "exactly at cap fires",
			committed: Session{TotalPromptTokens: 600, TotalCompletionTokens: 400, Turns: 1},
			limit:     1000,
			wantMsg:   true,
			// 1000 used, cap 1000, 1 turn — assert all three are in the message
			mustContain: []string{"1000", "1 turn"},
		},
		{
			name:        "over cap fires with formatted message",
			committed:   Session{TotalPromptTokens: 800, TotalCompletionTokens: 300, Turns: 4},
			limit:       1000,
			wantMsg:     true,
			mustContain: []string{"1100", "1000", "4 turn"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msg, exceeded := Exceeded(tc.committed, tc.limit)
			if exceeded != tc.wantMsg {
				t.Errorf("Exceeded returned exceeded=%v, want %v (msg=%q)", exceeded, tc.wantMsg, msg)
			}
			if !tc.wantMsg && msg != "" {
				t.Errorf("Exceeded returned message %q on a no-fire branch", msg)
			}
			for _, sub := range tc.mustContain {
				if !strings.Contains(msg, sub) {
					t.Errorf("message %q missing substring %q", msg, sub)
				}
			}
		})
	}
}
