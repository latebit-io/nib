package ui

import (
	"fmt"
	"strings"

	"github.com/latebit-io/junto/engine/event"
)

// Validator-gate helpers used by the EditProposed handler in app.go.
//
// All pure functions over []event.ValidatorSummary — no AppModel state
// touched — so the policy that decides "auto-apply, snooze, or surface
// for review" can be unit-tested in isolation. AppModel still owns the
// per-session blockedPaths set; this file owns the predicates and the
// banner-text formatting they share.

// summaryRequiresReview reports whether any validator summary
// carries a non-"pass" verdict. The auto-approve gate uses this to
// fail-closed on anything the validator was unhappy with — Block
// (must-surface), Retry (LLM exhausted its budget without fixing
// it), and any future verdict that doesn't explicitly map to "pass".
// The literal string comparison matches what validate.Verdict.String()
// produces over the wire.
func summaryRequiresReview(summaries []event.ValidatorSummary) bool {
	for _, s := range summaries {
		if s.Verdict != "pass" {
			return true
		}
	}
	return false
}

// summaryHasBlock reports whether any summary carries an explicit
// "block" verdict. Distinct from [summaryRequiresReview] because
// snoozing is only safe for Block — it carries the "developer has
// eyes-on for this file's recurring architectural concern"
// semantics. Retry (validator exhausted on this specific change),
// or any future non-pass non-block verdict, is per-edit and must be
// reviewed each time; promoting them into blockedPaths would
// re-open a fail-open path for validator-exhausted proposals.
//
// Forward-compat: a future verdict explicitly intended to be
// snoozable must opt in here, not in summaryRequiresReview. The
// default for unknown verdicts stays "must surface, never snooze."
func summaryHasBlock(summaries []event.ValidatorSummary) bool {
	for _, s := range summaries {
		if s.Verdict == "block" {
			return true
		}
	}
	return false
}

// snoozeBannerForSummaries renders the agent-pane meta line shown
// when a Block-bearing edit auto-applies because the developer
// already approved a prior Block on the same file this session. The
// banner names the file AND the verdicts so a session log reviewer
// can tell that the snooze (not Yolo, not LevelTrusted alone) was
// the reason auto-apply fired.
func snoozeBannerForSummaries(path string, summaries []event.ValidatorSummary) string {
	return fmt.Sprintf(
		"\n[validator: %s — auto-applied (snoozed: %s already approved this session)]\n",
		joinNonPassVerdicts(summaries), path)
}

// yoloOverrideBannerForSummaries renders the agent-pane meta line
// shown when LevelYolo auto-applies an edit that would otherwise
// have surfaced under LevelTrusted. The banner names the verdicts
// that were overridden so the developer reviewing the session log
// can see exactly which validator findings were ignored — without
// it, the only signal of an architecture-cap or lint-stage failure
// would be the on-disk result.
func yoloOverrideBannerForSummaries(summaries []event.ValidatorSummary) string {
	return fmt.Sprintf(
		"\n[validator: %s — auto-applied (yolo); review on-disk result]\n",
		joinNonPassVerdicts(summaries))
}

// reviewBannerForSummaries renders the agent-pane meta line shown
// when the validator gate refuses auto-approval. Lists the unique
// non-pass verdicts so the developer knows what kind of failure
// they're being asked to look at — `[validator: block — ...]`,
// `[validator: retry — ...]`, or `[validator: block, retry — ...]`
// when multiple stages flagged the edit.
func reviewBannerForSummaries(summaries []event.ValidatorSummary) string {
	verdicts := joinNonPassVerdicts(summaries)
	if verdicts == "" {
		// Shouldn't be reached — caller gates on summaryRequiresReview
		// — but if a future verdict label reads as empty for some
		// reason, fall back to a generic banner rather than emit
		// "validator:  — ...".
		return "\n[validator: non-pass verdict — auto-approval refused; press Ctrl+O to apply, Esc to reject]\n"
	}
	return fmt.Sprintf(
		"\n[validator: %s — auto-approval refused; press Ctrl+O to apply, Esc to reject]\n",
		verdicts)
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
