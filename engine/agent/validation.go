package agent

import (
	"context"
	"log/slog"
	"strings"

	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/validate"
)

// Validation gate for Agent.
//
// runValidationPipeline executes the configured pre-approval validators
// against a candidate edit and decides whether to silently retry,
// surface results, or pass through. The retry budget is per-CanonPath
// and shared across Retry and Block verdicts so Block cannot become
// theatre under autonomous mode (the validator reports a critical
// issue, the LLM never sees the feedback, and LevelTrusted auto-
// approves the broken edit anyway). Both verdicts feed structured
// feedback through the same retry channel; budget exhaustion surfaces
// the proposal so the developer sees the failure.
//
// State remains on Agent (pipeline, validatorRetries, cache, mu) so
// the run loop's existing locking rules carry over unchanged.

// runValidationPipeline runs pre-approval validators against the proposal
// and decides whether to retry, surface results, or pass through silently.
//
// Returns (summaries, feedback). A non-empty feedback string means the
// caller must short-circuit approval and hand the string back to the LLM
// as a tool result — the proposal will be regenerated. A nil/empty
// feedback means the caller should proceed to the comprehension gate
// with the returned summaries attached to AgentEditProposed.
//
// Retry AND Block both consume the per-CanonPath retry budget. Block
// would otherwise be theatre under autonomous mode: the validator
// reports a critical issue (architecture-cap exceeded, must split this
// file) but the LLM never sees the feedback and the TUI's
// LevelTrusted gate auto-approves the broken proposal anyway. Feeding
// Block feedback through the same retry channel gives the LLM a chance
// to self-correct (architectural fixes are big but tractable —
// "extract these functions into a sibling file"), and budget exhaustion
// surfaces the proposal so the developer sees the failure. The TUI
// applies an additional safety valve that refuses auto-approval when
// any summary carries Verdict="block" — see [tui.AppModel.Update]
// where it routes EditProposed events.
func (a *Agent) runValidationPipeline(ctx context.Context, proposal EditProposal) ([]event.ValidatorSummary, string) {
	if a.pipeline == nil {
		return nil, ""
	}
	before, _ := a.cache.Get(proposal.CanonPath)
	results := a.pipeline.Run(ctx, validate.Candidate{
		Path:      proposal.Path,
		CanonPath: proposal.CanonPath,
		Before:    before,
		After:     proposal.ExpectedContent,
		Edit:      proposal.Edit,
	})
	if len(results) == 0 {
		return nil, ""
	}

	summaries := toValidatorSummaries(results)

	worst := validate.WorstVerdict(results)
	if worst == validate.Pass {
		return summaries, ""
	}

	// Non-Pass path (Retry or Block): if the validators produced no
	// actionable feedback, there's nothing to send the LLM — surface
	// the proposal so the developer can intervene. Compute feedback
	// BEFORE touching validatorRetries: an empty-feedback round is a
	// developer-visible surface, not a silent retry, and burning a
	// budget slot for it would exhaust the budget on attempts that
	// never actually retry.
	feedback := aggregateRetryFeedback(results)
	if feedback == "" {
		return summaries, ""
	}

	// Actionable feedback exists — this is a real silent retry. Consume
	// a budget slot; if exhausted, surface so the developer sees what
	// the validators flagged. The map is also mutated by
	// RunWithMode/Reply/recordEdit, so every access is guarded by a.mu —
	// matching the existing recordEdit pattern.
	a.mu.Lock()
	a.validatorRetries[proposal.CanonPath]++
	attempts := a.validatorRetries[proposal.CanonPath]
	a.mu.Unlock()

	if attempts > maxValidatorRetries {
		slog.Warn("validator retry budget exhausted; surfacing proposal",
			"path", proposal.CanonPath, "attempts", attempts, "verdict", worst.String())
		return summaries, ""
	}
	return summaries, feedback
}

// toValidatorSummaries projects validate.Result onto the leaner
// event.ValidatorSummary wire type so the frontend contract does not
// couple to the validate package internals.
func toValidatorSummaries(results []validate.Result) []event.ValidatorSummary {
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

// aggregateRetryFeedback concatenates the non-empty Feedback strings of
// every non-Pass result into a single message for the LLM. Pass results
// are elided so the retry prompt stays focused on the faults.
func aggregateRetryFeedback(results []validate.Result) string {
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
