package ui

import (
	"strings"
	"testing"

	"github.com/latebit-io/junto/engine/event"
)

// TestSummaryRequiresReview locks the verdict-string comparison used
// by the auto-approve gate. A failure here would let a non-pass
// edit auto-apply under LevelTrusted — the regression that
// corrupted main.lua during the Pac-Man rerun, plus the variant CR
// caught for retry-exhausted summaries (which the original Block-only
// helper missed).
//
// The gate is fail-closed: anything that isn't literally "pass"
// requires review. A future verdict label has to opt into
// auto-apply by being mapped to "pass" — slipping past this gate
// must be deliberate, not silent.
func TestSummaryRequiresReview(t *testing.T) {
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
			name: "retry surfaces (budget exhausted by agent)",
			in: []event.ValidatorSummary{
				{Stage: "lint", Verdict: "retry", Feedback: "fix x"},
			},
			want: true,
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
			name: "unknown verdict fails closed (forward-compat)",
			in: []event.ValidatorSummary{
				{Stage: "future-stage", Verdict: "warn"},
			},
			want: true,
		},
		{
			name: "case-sensitive PASS does not match (BUG: would auto-apply if it did)",
			in: []event.ValidatorSummary{
				{Stage: "x", Verdict: "PASS"},
			},
			want: true, // "PASS" != "pass" → not pass → requires review
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := summaryRequiresReview(tc.in); got != tc.want {
				t.Errorf("summaryRequiresReview(%+v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestSnoozeBannerForSummaries verifies the snooze banner names
// both the verdicts and the path that triggered the snooze, so a
// session log reviewer can attribute auto-applies correctly. The
// previous-approval marker ("already approved this session") is
// load-bearing — it distinguishes snooze from Yolo override.
func TestSnoozeBannerForSummaries(t *testing.T) {
	t.Parallel()

	got := snoozeBannerForSummaries("src/main.lua",
		[]event.ValidatorSummary{
			{Stage: "architecture", Verdict: "block"},
		})
	for _, want := range []string{"src/main.lua", "block", "snoozed", "already approved"} {
		if !strings.Contains(got, want) {
			t.Errorf("snooze banner missing %q; got %q", want, got)
		}
	}
}

// TestYoloOverrideBannerForSummaries verifies the LevelYolo path's
// banner names the verdicts that were overridden so a session log
// reviewer can see exactly which validator findings were ignored.
// Without it, a Block override would land silently — the on-disk
// result would be the only signal.
func TestYoloOverrideBannerForSummaries(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		in          []event.ValidatorSummary
		wantSubstrs []string
	}{
		{
			name: "block override",
			in: []event.ValidatorSummary{
				{Stage: "architecture", Verdict: "block"},
			},
			wantSubstrs: []string{"validator: block", "yolo", "auto-applied"},
		},
		{
			name: "multi-verdict",
			in: []event.ValidatorSummary{
				{Stage: "architecture", Verdict: "block"},
				{Stage: "lint", Verdict: "retry"},
			},
			wantSubstrs: []string{"block", "retry", "yolo"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := yoloOverrideBannerForSummaries(tc.in)
			for _, want := range tc.wantSubstrs {
				if !strings.Contains(got, want) {
					t.Errorf("banner missing %q; got %q", want, got)
				}
			}
		})
	}
}

// TestReviewBannerForSummaries verifies the banner enumerates the
// unique non-pass verdicts so the developer immediately sees what
// kind of failure they're being asked to look at. Forward-compat:
// an unknown verdict shows up by name in the banner without any
// special-casing here.
func TestReviewBannerForSummaries(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		in          []event.ValidatorSummary
		wantSubstrs []string
	}{
		{
			name: "block only",
			in: []event.ValidatorSummary{
				{Stage: "go-parse", Verdict: "pass"},
				{Stage: "architecture", Verdict: "block"},
			},
			wantSubstrs: []string{"validator: block", "Ctrl+O"},
		},
		{
			name: "retry only",
			in: []event.ValidatorSummary{
				{Stage: "lint", Verdict: "retry"},
			},
			wantSubstrs: []string{"validator: retry"},
		},
		{
			name: "block and retry both surfaced",
			in: []event.ValidatorSummary{
				{Stage: "architecture", Verdict: "block"},
				{Stage: "lint", Verdict: "retry"},
			},
			wantSubstrs: []string{"block", "retry"},
		},
		{
			name: "duplicate verdicts deduped",
			in: []event.ValidatorSummary{
				{Stage: "go-parse", Verdict: "retry"},
				{Stage: "lint", Verdict: "retry"},
			},
			wantSubstrs: []string{"validator: retry"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reviewBannerForSummaries(tc.in)
			for _, want := range tc.wantSubstrs {
				if !strings.Contains(got, want) {
					t.Errorf("banner missing %q; got %q", want, got)
				}
			}
		})
	}
}
