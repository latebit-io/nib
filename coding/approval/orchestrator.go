package approval

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/latebit-io/junto/coding/tools"
	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/lang"
)

// Edit-approval orchestration.
//
// [Orchestrator] runs the review flow the `edit_file` / `replace_file`
// tools trigger via the [tools.Approver.Propose] collaborator method.
// It threads the validation pipeline, EditProposed event delivery,
// approve/reject wait, edit-record callback, context-set update, and
// post-approval cache update with diagnostics injection.
//
// State stays on the agent (taskEdits, validatorRetries, file cache)
// — the Orchestrator only sees those fields through the [Deps]
// callbacks. That preserves the agent's run-state invariants (locks
// around taskEdits + validatorRetries) without forcing this package
// to know about them.
//
// The [Coordinator] is per-run: the agent allocates a fresh one at
// every RunWithMode/Reply-resume and stashes it on ctx (see
// `coding/agent.ctxWithCoord`). The run goroutine threads it down
// to [Orchestrator.Handle] explicitly so a competing run that swaps
// the agent's coord field cannot redirect this proposal to a
// different run's channels.

// Deps bundles the collaborators an [Orchestrator] needs to drive
// the edit-approval flow. Constructed once at agent New() time and
// retained for the agent's lifetime — the Cache and Workspace
// pointers are stable, the func fields close over agent state, and
// the lifetime cost of the struct is one allocation per agent.
type Deps struct {
	// Cache is the agent-shared file-content view. The orchestrator
	// reads it on rejection (to surface "current file" content to
	// the LLM) and writes it after approval (so subsequent tool
	// calls see the post-edit state without re-reading from disk).
	Cache *tools.FileCache

	// Workspace exposes the developer's context set. After approval,
	// the edited file is added to the context set if not already
	// present so subsequent runs include it.
	Workspace tools.ContextSet

	// Send is the best-effort event emitter (drops on full channel,
	// logs the drop). Used for status transitions and progress
	// banners that should not block approval flow on backpressure.
	Send func(event.Event)

	// SendCritical is the timeout-bounded blocking emitter for
	// events that gate the flow. The EditProposed event uses this
	// because if the frontend never sees it, the await-approval
	// step blocks forever with nothing for the user to approve.
	SendCritical func(context.Context, event.Event) error

	// Validate runs the pre-approval validation pipeline (syntactic
	// parse, LSP shadow, style invariants). When retryFeedback is
	// non-empty, [Orchestrator.Handle] short-circuits and returns
	// the feedback as the tool result so the LLM regenerates the
	// proposal. The summaries are attached to the EditProposed
	// event regardless.
	Validate func(ctx context.Context, p tools.EditProposal) (summaries []event.ValidatorSummary, retryFeedback string)

	// RecordEdit is invoked once per approved edit so the agent can
	// append to its taskEdits slice (for end-of-task review) and
	// clear the per-path validatorRetries counter (a successful
	// approval means the retry budget resets).
	RecordEdit func(p tools.EditProposal)

	// DiagProvider is the optional language-server diagnostics
	// injector. When non-nil, post-approval diagnostics are
	// appended to the tool result so the LLM can self-correct
	// errors that the validation pipeline did not catch (LSP
	// findings often arrive after the edit lands).
	DiagProvider lang.DiagnosticProvider

	// DiagDelay is the wait time between approval landing and the
	// diagnostics fetch — language servers need a beat to re-parse
	// the file before they can return findings.
	DiagDelay time.Duration
}

// Orchestrator is the agent-side glue that runs the edit-approval
// flow on top of a [Coordinator]. Construct once via [NewOrchestrator]
// and call [Orchestrator.Handle] per proposal.
type Orchestrator struct {
	deps Deps
}

// NewOrchestrator returns an Orchestrator wired with the given
// dependencies. Panics if any non-optional callback (Validate,
// RecordEdit, Send, SendCritical) is nil — these are load-bearing
// for the flow and a nil callback would silently produce wrong
// behaviour rather than failing visibly.
func NewOrchestrator(deps Deps) *Orchestrator {
	if deps.Cache == nil {
		panic("approval.NewOrchestrator: Cache is required")
	}
	if deps.Workspace == nil {
		panic("approval.NewOrchestrator: Workspace is required")
	}
	if deps.Send == nil || deps.SendCritical == nil {
		panic("approval.NewOrchestrator: Send and SendCritical are required")
	}
	if deps.Validate == nil {
		panic("approval.NewOrchestrator: Validate is required")
	}
	if deps.RecordEdit == nil {
		panic("approval.NewOrchestrator: RecordEdit is required")
	}
	return &Orchestrator{deps: deps}
}

// outcome reports whether a flow path produced a body the agent
// should treat as a tool error or as normal flow. Carrying this
// alongside the body string avoids the brittleness of scanning the
// rendered tool body for marker substrings — cached file content
// could contain the marker text by accident and trigger a false
// isError=true on a normal rejection.
type outcome int

const (
	// outcomeOK marks a body that is normal LLM-facing flow:
	// validator retry, rejection note, applied-successfully
	// message, developer-modified-edit recalibration. The agent
	// should pass these through as ordinary tool results.
	outcomeOK outcome = iota
	// outcomeFatal marks a body produced by a delivery failure,
	// channel close, or ctx cancel. The agent should surface these
	// as tool errors so the frontend renders them distinctively.
	outcomeFatal
)

// Handle runs the full approval flow for proposal. Returns the body
// the LLM should see as the tool result, plus a flag reporting
// whether that body should be surfaced as a tool error to the
// frontend. The flag is set only on delivery failures and
// cancellation; rejection feedback and validator recalibration are
// normal flow.
//
// coord is the run's [*Coordinator], captured at run launch by the
// agent's run goroutine. Passing it explicitly (rather than reading
// agent.coord) prevents a competing RunWithMode that swaps the
// agent's coord field from redirecting this proposal to a different
// run's channels.
func (o *Orchestrator) Handle(ctx context.Context, coord *Coordinator, p tools.EditProposal) (body string, isError bool) {
	body, oc := o.handle(ctx, coord, p)
	return body, oc == outcomeFatal
}

// handle is the inner flow that returns the body and an explicit
// outcome marker. The outcome is what [Handle] uses to set the
// public isError flag — never the body string. New fatal exit
// points must return outcomeFatal explicitly; the type system
// catches forgotten paths because the function won't compile
// without an outcome value on each return.
func (o *Orchestrator) handle(ctx context.Context, coord *Coordinator, p tools.EditProposal) (string, outcome) {
	// Pre-approval validation. A non-empty retryFeedback short-
	// circuits approval and hands the feedback back to the LLM so
	// the proposal is regenerated; exhaustion (or any other
	// outcome) falls through to the comprehension gate with the
	// summaries attached to the event.
	summaries, retryFeedback := o.deps.Validate(ctx, p)
	if retryFeedback != "" {
		return retryFeedback, outcomeOK
	}

	o.deps.Send(event.AgentStatus{Status: event.StatusReviewing})

	// The proposal event is critical — if the frontend never sees
	// it, the await-approval step blocks forever with nothing for
	// the user to approve.
	if err := o.deps.SendCritical(ctx, event.AgentEditProposed{
		Edit:               p.Edit,
		ValidatorSummaries: summaries,
	}); err != nil {
		slog.Error("edit proposal delivery failed", "err", err)
		return fmt.Sprintf("Error: could not deliver edit proposal to frontend: %v", err), outcomeFatal
	}

	approved, msg, oc := o.waitForApproval(ctx, coord, p)
	if oc == outcomeFatal {
		return msg, outcomeFatal
	}
	if msg != "" {
		// Rejection note — normal flow. Pass through as outcomeOK
		// so a stray fatal-marker substring inside the cached file
		// content (which the rejection body embeds) cannot fool a
		// caller into treating reject as a tool error.
		return msg, outcomeOK
	}

	// Approved — record for end-of-turn review and add to context set.
	o.deps.RecordEdit(p)
	if !o.deps.Workspace.InContext(p.Path) {
		o.deps.Workspace.AddContext(p.Path)
	}

	return o.afterApproval(p, approved.Content)
}

// waitForApproval blocks until the developer approves or rejects
// the edit, ctx is canceled, or the channel is closed. Returns the
// [Approval] (carrying the approved flag and post-apply content) on
// the approval path so the orchestrator can seed the cache from the
// real buffer state. Outcome matrix:
//
//   - approve  → (approval, "", outcomeOK)
//   - reject   → (zero, rejectionMsg, outcomeOK)
//   - ctx cancel → (zero, "Error: agent canceled", outcomeFatal)
//   - channel closed → (zero, "Error: approval channel closed", outcomeFatal)
//
// The channel-closed branch is preserved as a distinct message so
// the caller can tell the LLM precisely what happened — earlier
// versions collapsed both fatal paths into "Error: agent canceled"
// via a wrapper, which masked an actual closed-channel failure.
func (o *Orchestrator) waitForApproval(ctx context.Context, coord *Coordinator, p tools.EditProposal) (Approval, string, outcome) {
	a, err := coord.AwaitApproval(ctx)
	if err != nil {
		if errors.Is(err, ErrChannelClosed) {
			return Approval{}, "Error: approval channel closed", outcomeFatal
		}
		return Approval{}, "Error: agent canceled", outcomeFatal
	}
	if a.Approved {
		return a, "", outcomeOK
	}

	o.deps.Send(event.AgentStatus{Status: event.StatusThinking})
	o.deps.Send(event.AgentToken{Text: "\n[Edit rejected]\n\n"})

	content := ""
	if c, ok := o.deps.Cache.Get(p.CanonPath); ok {
		content = c
	}
	return Approval{}, fmt.Sprintf("The developer rejected this edit. Try a different approach or move on.\n\nCurrent file (%s):\n\n%s",
		p.Path, tools.TruncateForPreview(content)), outcomeOK
}

// afterApproval seeds the cache with the post-apply buffer content
// (delivered through the Approve signal) and returns the success body
// for the LLM. Using the buffer-derived content rather than
// p.ExpectedContent matters whenever the developer modified the
// replacement text in the diff overlay before approving — the buffer
// holds the true post-apply state; ExpectedContent holds the agent's
// prediction. Diagnostics, when configured, are appended after a
// brief delay so the language server has time to re-parse.
func (o *Orchestrator) afterApproval(p tools.EditProposal, applied string) (string, outcome) {
	o.deps.Cache.Set(p.CanonPath, applied)
	o.deps.Send(event.AgentStatus{Status: event.StatusThinking})

	result := fmt.Sprintf("Edit applied successfully.\n\nCurrent file (%s):\n\n%s",
		p.Path, tools.TruncateForPreview(applied))

	if o.deps.DiagProvider != nil {
		time.Sleep(o.deps.DiagDelay)
		diagResult := tools.FormatDiagnostics(o.deps.DiagProvider, p.CanonPath, p.Path)
		result += "\n\nDiagnostics after edit:\n" + diagResult
	}

	return result, outcomeOK
}
