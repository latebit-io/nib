package approval

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/latebit-io/junto/coding/tools"
	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/lang"
)

// Edit-approval orchestration.
//
// [Orchestrator] runs the full review-and-continue dance the
// `edit_file` / `replace_file` tools trigger via the
// [tools.Approver.Propose] collaborator method. It threads the
// validation pipeline, EditProposed event delivery, approve/reject
// wait, edit-record callback, context-set update, and the post-
// approval continue with diagnostics injection.
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
	// the LLM) and writes it on Continue (so subsequent tool calls
	// see the post-edit state without re-reading from disk).
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

	// Autonomous reports whether the agent is in autonomous mode.
	// Used only to decide whether to render the "press Ctrl+N to
	// continue" hint — at autonomous-or-higher levels the continue
	// fires automatically and the message is misleading visual
	// noise. The lock-protected getter on the agent is what's
	// passed in; this struct never reads autonomy state directly.
	Autonomous func() bool

	// DiagProvider is the optional language-server diagnostics
	// injector. When non-nil, post-Continue diagnostics are
	// appended to the tool result so the LLM can self-correct
	// errors that the validation pipeline did not catch (LSP
	// findings often arrive after the edit lands).
	DiagProvider lang.DiagnosticProvider

	// DiagDelay is the wait time between Continue arriving and the
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
// RecordEdit, Autonomous, Send, SendCritical) is nil — these are
// load-bearing for the flow and a nil callback would silently
// produce wrong behaviour rather than failing visibly.
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
	if deps.Autonomous == nil {
		panic("approval.NewOrchestrator: Autonomous is required")
	}
	return &Orchestrator{deps: deps}
}

// fatalProposalMarkers are the unrecoverable outcomes [Orchestrator.Handle]
// can produce. Matched as substrings so the IsError flag stays
// accurate even if the upstream message gains a wrapped error
// suffix. Rejection notes ("The developer rejected this edit…") and
// validator recalibration are normal flow and intentionally NOT
// listed here.
var fatalProposalMarkers = []string{
	"Error: agent canceled",
	"Error: could not deliver edit proposal to frontend",
	"Error: continue channel closed",
	"Error: approval channel closed",
}

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
	body = o.handle(ctx, coord, p)
	for _, marker := range fatalProposalMarkers {
		if strings.Contains(body, marker) {
			return body, true
		}
	}
	return body, false
}

// handle is the inner flow that returns just the body string.
// Split from [Handle] so the marker-based isError detection stays at
// a single call site even when the flow grows new return paths.
func (o *Orchestrator) handle(ctx context.Context, coord *Coordinator, p tools.EditProposal) string {
	// Pre-approval validation. A non-empty retryFeedback short-
	// circuits approval and hands the feedback back to the LLM so
	// the proposal is regenerated; exhaustion (or any other
	// outcome) falls through to the comprehension gate with the
	// summaries attached to the event.
	summaries, retryFeedback := o.deps.Validate(ctx, p)
	if retryFeedback != "" {
		return retryFeedback
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
		return fmt.Sprintf("Error: could not deliver edit proposal to frontend: %v", err)
	}

	msg, canceled := o.waitForApproval(ctx, coord, p)
	if canceled {
		return "Error: agent canceled"
	}
	if msg != "" {
		return msg
	}

	// Approved — record for end-of-turn review and add to context set.
	o.deps.RecordEdit(p)
	if !o.deps.Workspace.InContext(p.Path) {
		o.deps.Workspace.AddContext(p.Path)
	}

	// Wait for the developer to continue with updated buffer content.
	return o.waitForContinue(ctx, coord, p)
}

// waitForApproval blocks until the developer approves or rejects
// the edit, ctx is canceled, or the channel is closed. Returns
// (rejectionMsg, false) on reject, ("", true) on cancel or channel
// closed, ("", false) on approve.
func (o *Orchestrator) waitForApproval(ctx context.Context, coord *Coordinator, p tools.EditProposal) (msg string, canceled bool) {
	approved, err := coord.AwaitApproval(ctx)
	if err != nil {
		if errors.Is(err, ErrChannelClosed) {
			return "Error: approval channel closed", true
		}
		return "", true
	}
	if approved {
		return "", false
	}

	o.deps.Send(event.AgentStatus{Status: event.StatusThinking})
	o.deps.Send(event.AgentToken{Text: "\n[Edit rejected]\n\n"})

	content := ""
	if c, ok := o.deps.Cache.Get(p.CanonPath); ok {
		content = c
	}
	return fmt.Sprintf("The developer rejected this edit. Try a different approach or move on.\n\nCurrent file (%s):\n\n%s",
		p.Path, tools.TruncateForPreview(content)), false
}

// waitForContinue blocks until the developer finishes editing and
// signals continue, or the context is canceled. Compares the new
// content against the expected result to detect developer
// modifications and surfaces them to the LLM with a diff.
func (o *Orchestrator) waitForContinue(ctx context.Context, coord *Coordinator, p tools.EditProposal) string {
	o.deps.Send(event.AgentStatus{Status: event.StatusEditing})
	// Render the "press Ctrl+N to continue" prompt only at
	// LevelGuided (where the developer actually has to press
	// Ctrl+N). At LevelCollaborate+ the autonomous flag is set and
	// continue fires automatically — the message is then visual
	// noise that reads like an approval prompt and confused
	// testers ("why did it ask for my approval?"). The
	// StatusEditing chip transition is the signal in autonomous
	// modes.
	if !o.deps.Autonomous() {
		o.deps.Send(event.AgentToken{Text: "\n[Edit approved — waiting for continue]\n"})
	}

	newContent, err := coord.AwaitContinue(ctx)
	if err != nil {
		if errors.Is(err, ErrChannelClosed) {
			return "Error: continue channel closed"
		}
		return "Error: agent canceled"
	}
	o.deps.Cache.Set(p.CanonPath, newContent)
	o.deps.Send(event.AgentStatus{Status: event.StatusThinking})
	o.deps.Send(event.AgentToken{Text: "\n"})

	var result string
	if newContent != p.ExpectedContent {
		diff := tools.SimpleDiff(p.ExpectedContent, newContent)
		result = fmt.Sprintf("Edit applied, but the developer modified your edit. "+
			"IMPORTANT: The file content below is the AUTHORITATIVE current state. "+
			"Do NOT use any earlier version of this file from the conversation — only use what is shown here.\n\n"+
			"Developer's changes (what they changed from your proposal):\n```diff\n%s\n```\n\n"+
			"Recalibrate: study the diff — it signals the developer's intent. "+
			"Align your next steps with their direction. "+
			"If you notice a syntax error or bug in their edit, point it out and propose a fix.\n\n"+
			"Current file (%s):\n```\n%s\n```",
			diff, p.Path, tools.TruncateForPreview(newContent))
	} else {
		result = fmt.Sprintf("Edit applied successfully.\n\nCurrent file (%s):\n\n%s",
			p.Path, tools.TruncateForPreview(newContent))
	}

	// Auto-inject diagnostics so the agent can self-correct errors.
	if o.deps.DiagProvider != nil {
		time.Sleep(o.deps.DiagDelay)
		diagResult := tools.FormatDiagnostics(o.deps.DiagProvider, p.CanonPath, p.Path)
		result += "\n\nDiagnostics after edit:\n" + diagResult
	}

	return result
}
