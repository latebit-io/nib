package ui

import (
	"fmt"
	"strings"

	"github.com/latebit-io/nib/coding/event"
)

// Validator-gate helpers used by the EditProposed handler in app.go.
//
// Pure functions over []event.ValidatorSummary — no AppModel state
// touched — so the auto-approve policy is unit-testable in isolation.

// summaryRequiresReview reports whether any validator summary
// carries a non-"pass" verdict. The auto-approve gate uses this to
// fail-closed on anything the validator was unhappy with. The literal
// string comparison matches what validate.Verdict.String() produces
// over the wire.
func summaryRequiresReview(summaries []event.ValidatorSummary) bool {
	for _, s := range summaries {
		if s.Verdict != "pass" {
			return true
		}
	}
	return false
}

// reviewBannerForSummaries renders the agent-pane meta line shown
// when the validator gate refuses auto-approval. Lists the unique
// non-pass verdicts AND each non-pass stage's feedback so the
// developer can decide whether to approve or reject without having
// to guess what the validator flagged. Without per-stage feedback the
// banner only said "auto-approval refused" — leaving the developer
// staring at a diff with no explanation of which rule fired or why.
func reviewBannerForSummaries(summaries []event.ValidatorSummary) string {
	verdicts := joinNonPassVerdicts(summaries)
	if verdicts == "" {
		// Shouldn't be reached — caller gates on summaryRequiresReview
		// — but if a future verdict label reads as empty for some
		// reason, fall back to a generic banner rather than emit
		// "validator:  — ...".
		return "\n[validator: non-pass verdict — auto-approval refused; press Ctrl+O to apply, Esc to reject]\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b,
		"\n[validator: %s — auto-approval refused; press Ctrl+O to apply, Esc to reject]\n",
		verdicts)
	if reasons := formatStageReasons(summaries); reasons != "" {
		b.WriteString(reasons)
	}
	return b.String()
}

// formatStageReasons renders the per-stage feedback of each non-pass
// summary as a bulleted list. Returns "" when no non-pass summary
// carries feedback (e.g. an architecture validator that produces only
// a verdict without a structured reason). The output is wrapped at
// stage boundaries — each stage's feedback is a single bullet so the
// developer reads it as one unit even when feedback spans lines.
func formatStageReasons(summaries []event.ValidatorSummary) string {
	var b strings.Builder
	for _, s := range summaries {
		if s.Verdict == "pass" {
			continue
		}
		feedback := strings.TrimSpace(s.Feedback)
		if feedback == "" {
			continue
		}
		stage := s.Stage
		if stage == "" {
			stage = s.Verdict
		}
		// Indent continuation lines so a multi-line feedback string
		// (e.g. lint findings, several at once) reads as one bullet
		// rather than a list of unrelated lines.
		indented := strings.ReplaceAll(feedback, "\n", "\n    ")
		fmt.Fprintf(&b, "  • %s: %s\n", stage, indented)
	}
	return b.String()
}

// joinNonPassVerdicts returns a comma-separated list of unique
// non-pass verdict labels in document order. Empty when only pass
// verdicts (or no summaries) are present. Shared by the
// review-banner and yolo-override-banner so both surfaces use the
// exact same wording for the verdict list.
func joinNonPassVerdicts(summaries []event.ValidatorSummary) string {
	seen := map[string]bool{}
	var verdicts []string
	for _, s := range summaries {
		if s.Verdict == "pass" || seen[s.Verdict] {
			continue
		}
		seen[s.Verdict] = true
		verdicts = append(verdicts, s.Verdict)
	}
	return strings.Join(verdicts, ", ")
}
