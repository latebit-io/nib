package ui

import (
	"strings"
	"testing"

	"github.com/latebit-io/nib/coding/event"
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

// TestSummaryHasBlock locks the snooze-eligibility gate. Critical:
// retry verdicts (validator exhausted its budget on this specific
// change) must NOT be snooze-eligible — promoting them into
// blockedPaths would auto-apply the next retry on the same file
// without developer review, re-opening a fail-open path.
//
// summaryHasBlock is separate from [summaryRequiresReview] precisely
// to keep these two questions separate: "must surface" (any non-pass)
// vs "may snooze" (explicit block only).
func TestSummaryHasBlock(t *testing.T) {
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
			},
			want: false,
		},
		{
			name: "retry only — must surface but must NOT be snoozable",
			in: []event.ValidatorSummary{
				{Stage: "lint", Verdict: "retry"},
			},
			want: false,
		},
		{
			name: "block alone is snoozable",
			in: []event.ValidatorSummary{
				{Stage: "architecture", Verdict: "block"},
			},
			want: true,
		},
		{
			name: "block among pass and retry — block dominates",
			in: []event.ValidatorSummary{
				{Stage: "go-parse", Verdict: "pass"},
				{Stage: "architecture", Verdict: "block"},
				{Stage: "lint", Verdict: "retry"},
			},
			want: true,
		},
		{
			name: "unknown verdict is NOT snoozable (forward-compat fail-closed)",
			in: []event.ValidatorSummary{
				{Stage: "future-stage", Verdict: "warn"},
			},
			want: false,
		},
		{
			name: "case-sensitive BLOCK does not match (would silently be unsnoozable)",
			in: []event.ValidatorSummary{
				{Stage: "x", Verdict: "BLOCK"},
			},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := summaryHasBlock(tc.in); got != tc.want {
				t.Errorf("summaryHasBlock(%+v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestSummaryHasBlock_NotEqualToSummaryRequiresReview pins the
// invariant that the two predicates are deliberately different.
// Without this, a refactor that "simplifies" summaryHasBlock to
// alias summaryRequiresReview would silently re-introduce the
// retry-snoozes-future-retries fail-open bug.
func TestSummaryHasBlock_NotEqualToSummaryRequiresReview(t *testing.T) {
	t.Parallel()

	// Retry: must surface (true) but must not be snoozable (false).
	retryOnly := []event.ValidatorSummary{{Stage: "lint", Verdict: "retry"}}
	if !summaryRequiresReview(retryOnly) {
		t.Fatal("setup invariant: retry must require review")
	}
	if summaryHasBlock(retryOnly) {
		t.Errorf("retry was reported as snooze-eligible — fail-open regression: " +
			"approving a retry once would auto-apply future retries on the same file")
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

// TestReviewBannerIncludesStageFeedback verifies the per-stage reason
// is rendered alongside the verdict so the developer can decide
// without staring at an unannotated diff. The literal block text
// from the validator must reach the banner intact (path, cap, count).
func TestReviewBannerIncludesStageFeedback(t *testing.T) {
	t.Parallel()

	got := reviewBannerForSummaries([]event.ValidatorSummary{
		{
			Stage:    "architecture",
			Verdict:  "block",
			Feedback: "main.lua exceeds 500-line cap (got 612)",
		},
	})
	wants := []string{
		"validator: block",
		"architecture",
		"main.lua exceeds 500-line cap (got 612)",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("banner missing %q; got %q", w, got)
		}
	}
}

// TestReviewBannerMultiStageFeedback verifies each non-pass stage
// gets its own bullet so a developer reading the banner can tell the
// architecture and lint findings apart at a glance — without per-stage
// labelling, multiple findings would blur into one paragraph.
func TestReviewBannerMultiStageFeedback(t *testing.T) {
	t.Parallel()

	got := reviewBannerForSummaries([]event.ValidatorSummary{
		{Stage: "architecture", Verdict: "block", Feedback: "file too long"},
		{Stage: "lint", Verdict: "retry", Feedback: "unused import"},
	})
	wants := []string{
		"architecture: file too long",
		"lint: unused import",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("banner missing %q; got %q", w, got)
		}
	}
}

// TestReviewBannerPassSummariesElided verifies pass-verdict stages do
// NOT contribute reasons to the bulleted list — "go-parse: <empty>"
// would be noise. Only the non-pass stages explain why review fired.
func TestReviewBannerPassSummariesElided(t *testing.T) {
	t.Parallel()

	got := reviewBannerForSummaries([]event.ValidatorSummary{
		{Stage: "go-parse", Verdict: "pass"},
		{Stage: "architecture", Verdict: "block", Feedback: "cap exceeded"},
	})
	if strings.Contains(got, "go-parse") {
		t.Errorf("banner mentions pass stage go-parse; got %q", got)
	}
	if !strings.Contains(got, "architecture: cap exceeded") {
		t.Errorf("banner missing block stage feedback; got %q", got)
	}
}

// TestReviewBannerEmptyFeedbackOmitted verifies a non-pass stage with
// no Feedback string is silently skipped rather than emitting an
// empty bullet. Some validators (or future stages) may produce only a
// verdict without a structured reason — the banner must still render
// cleanly with just the header line.
func TestReviewBannerEmptyFeedbackOmitted(t *testing.T) {
	t.Parallel()

	got := reviewBannerForSummaries([]event.ValidatorSummary{
		{Stage: "architecture", Verdict: "block", Feedback: ""},
	})
	if strings.Contains(got, "•") {
		t.Errorf("banner emitted an empty bullet for feedback-less summary; got %q", got)
	}
	if !strings.Contains(got, "validator: block") {
		t.Errorf("banner missing the verdict header; got %q", got)
	}
}
