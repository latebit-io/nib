// Package validation holds the pure helpers used by the agent's
// pre-approval validation pipeline. The orchestration that runs the
// pipeline against a candidate edit and decides whether to silently
// retry or surface results stays on the Agent (it reads agent state
// like the per-CanonPath retry budget); only the projection and
// feedback-aggregation logic lives here so it can be unit-tested
// without standing up an Agent.
package validation

import (
	"strings"

	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/validate"
)

// MaxValidatorRetries caps the per-CanonPath silent-retry budget the
// agent's validation pipeline consumes when validators return a
// non-Pass verdict with actionable feedback. The cap matches the
// search-validation retry budget so the two layers compose
// predictably; bumping it in isolation would let a failing validator
// stage drown out edit-search retries.
const MaxValidatorRetries = 3

// ToValidatorSummaries projects [validate.Result] onto the leaner
// [event.ValidatorSummary] wire type so the frontend contract does
// not couple to the validate package internals.
func ToValidatorSummaries(results []validate.Result) []event.ValidatorSummary {
	out := make([]event.ValidatorSummary, 0, len(results))
	for _, r := range results {
		out = append(out, event.ValidatorSummary{
			Stage:    r.Stage,
			Verdict:  r.Verdict.String(),
			Feedback: r.Feedback,
		})
	}
	return out
}

// AggregateRetryFeedback concatenates the non-empty Feedback strings
// of every non-Pass result into a single message for the LLM. Pass
// results are elided so the retry prompt stays focused on the faults.
func AggregateRetryFeedback(results []validate.Result) string {
	var b strings.Builder
	for _, r := range results {
		if r.Verdict == validate.Pass || r.Feedback == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(r.Feedback)
	}
	return b.String()
}
