package ui

import (
	"testing"

	"github.com/latebit-io/junto/engine/event"
)

// TestHasBlockSummary locks the verdict-string comparison used by the
// auto-approve gate. A subtle change here (e.g. case-folding or a
// renamed verdict label) would let a Block edit auto-apply under
// LevelTrusted, which is the regression that corrupted main.lua during
// the Pac-Man rerun.
func TestHasBlockSummary(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   []event.ValidatorSummary
		want bool
	}{
		{
			name: "empty",
			in:   nil,
			want: false,
		},
		{
			name: "all pass",
			in: []event.ValidatorSummary{
				{Stage: "go-parse", Verdict: "pass"},
				{Stage: "tree-sitter", Verdict: "pass"},
			},
			want: false,
		},
		{
			name: "retry only",
			in: []event.ValidatorSummary{
				{Stage: "lint", Verdict: "retry", Feedback: "fix x"},
			},
			want: false,
		},
		{
			name: "block among pass",
			in: []event.ValidatorSummary{
				{Stage: "go-parse", Verdict: "pass"},
				{Stage: "architecture", Verdict: "block", Feedback: "split file"},
			},
			want: true,
		},
		{
			name: "block alone",
			in: []event.ValidatorSummary{
				{Stage: "architecture", Verdict: "block"},
			},
			want: true,
		},
		{
			name: "case-sensitive (uppercase BLOCK does not match)",
			in: []event.ValidatorSummary{
				{Stage: "architecture", Verdict: "BLOCK"},
			},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasBlockSummary(tc.in); got != tc.want {
				t.Errorf("hasBlockSummary(%+v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
