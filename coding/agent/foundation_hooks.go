package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	upagent "github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/budget"
	"github.com/latebit-io/nib/coding/nudges"
	"github.com/latebit-io/nib/coding/streaming"
	"github.com/latebit-io/nib/coding/truncation"
	"github.com/latebit-io/nib/engine/event"
)

// errBudgetExceeded is the sentinel TransformContext returns when the
// per-task token cap has been crossed. The foundation loop surfaces it
// as an [agent/event.Error] and ends the run; the application wrapper
// (post-8c) is expected to translate the run-end into the existing
// [event.AgentError] + AgentDone{Success:false} pair so frontends see
// the same shape they do today.
var errBudgetExceeded = errors.New("coding/agent: per-task token budget exceeded")

// Foundation-hook bridge.
//
// FoundationHooks builds the [upagent.Hooks] value the soon-to-be-driver
// of the run loop ([upagent.Agent]) consumes in sub-phase 8c. Each gate
// is a small, stateless method on *Agent that mirrors one decision the
// inline run loop makes today; FoundationHooks composes them and
// captures any per-turn state in closure variables so the Agent struct
// stays free of hook-only fields.
//
// During sub-phase 8b each concern from [coding/agent]'s loop migrates
// into one of these gates. The inline logic in turn.go / run.go stays
// untouched until 8c swaps the loops; the gates are exercised
// independently by tests so each concern is proven in isolation before
// the cutover.

// FoundationHooks returns the [upagent.Hooks] value bound to this
// agent. Closure-captured state (singleEditFired, narrativeFired,
// permissionFired, truncationRetries) lives for the lifetime of the
// returned Hooks; a fresh FoundationHooks call yields a fresh closure.
//
// liveMessages is the snapshotter the steering hook uses to read the
// transcript including the assistant turn that just ended (TransformContext
// only sees the pre-Stream slice). The wrapper wires it to
// [upagent.Agent.State]'s Messages; tests pass a fixture function.
// Nil is tolerated — treated as "no messages" — for unit tests that
// don't exercise steering.
func (a *Agent) FoundationHooks(liveMessages func() []llm.Message) upagent.Hooks {
	if liveMessages == nil {
		liveMessages = func() []llm.Message { return nil }
	}
	// singleEditFired is reset at the top of each turn (TransformContext
	// fires once per outer iteration of the foundation loop, immediately
	// before Stream) and set when [singleEditGate] permits a file-edit
	// dispatch.
	var singleEditFired bool
	// narrativeFired and permissionFired are the per-developer-input
	// guards mirroring the local bools the inline run loop carried.
	// Reset by TransformContext when a fresh user message lands at the
	// tail of the transcript (one not authored by a nudge).
	var (
		narrativeFired  bool
		permissionFired bool
	)
	// lastWasBlocked records whether the most recent BeforeToolCall
	// returned Block=true. AfterToolCall reads + clears it so the
	// intent-reminder append is skipped on the Block path (mirrors the
	// inline behavior — dispatchTool returned early before
	// [Agent.intentReminder] on planning-mode + active-task rejections,
	// but included the reminder on tool-execute and unknown-tool paths).
	// Safe with a single bool because BeforeToolCall and AfterToolCall
	// always pair within one foundation tool dispatch.
	var lastWasBlocked bool
	// truncationRetries counts consecutive truncated turns within the
	// run. Reset implicitly each new run because FoundationHooks is
	// called fresh in [New]; survives across non-truncated turns
	// because the hook value itself outlives turns.
	var truncationRetries int

	before := func(ctx context.Context, c upagent.BeforeToolCallContext) (upagent.BeforeToolCallResult, error) {
		// Order mirrors the inline path:
		//   1. lint-pending skip (was turn.go:188)
		//   2. single-edit (was turn.go:198)
		//   3. flush dirty buffers + emit AgentToolCall (was turn.go:214-224)
		//   4. planning blocklist (was turn.go:289 inside dispatchTool)
		//   5. active-task gate (was turn.go:295 inside dispatchTool)
		//
		// lastWasBlocked is set on every Block path so AfterToolCall
		// can decide whether to apply the intent-reminder override.
		if res := a.lintPendingGate(); res.Block {
			lastWasBlocked = true
			return res, nil
		}
		if res := a.singleEditGate(c, &singleEditFired); res.Block {
			lastWasBlocked = true
			return res, nil
		}
		if err := a.flushDirtyBuffers(ctx); err != nil {
			a.send(event.AgentError{Err: fmt.Sprintf("autosave failed: %v", err)})
			lastWasBlocked = true
			return upagent.BeforeToolCallResult{
				Block:  true,
				Reason: fmt.Sprintf("Skipped — autosave failed: %v", err),
			}, nil
		}
		// Mirrors turn.go:222-224: emit AgentToolCall AFTER the
		// gates that suppress dispatch entirely (lint, single-edit)
		// but BEFORE planning + active-task gates so the frontend
		// sees the call attempt even when it's about to be
		// rejected. Inline behavior: planning + active-task
		// rejections still emit AgentToolCall.
		a.send(event.AgentToolCall{Name: c.Name, Args: c.Args})
		if res := a.planningBlocklistGate(c); res.Block {
			lastWasBlocked = true
			return res, nil
		}
		if res := a.activeTaskGate(ctx, c); res.Block {
			lastWasBlocked = true
			return res, nil
		}
		lastWasBlocked = false
		return upagent.BeforeToolCallResult{}, nil
	}

	after := func(_ context.Context, c upagent.AfterToolCallContext) (upagent.AfterToolCallResult, error) {
		blocked := lastWasBlocked
		lastWasBlocked = false
		return a.foundationAfterToolCall(c, blocked), nil
	}

	transform := func(ctx context.Context, msgs []llm.Message) ([]llm.Message, error) {
		singleEditFired = false
		fresh := isFreshUserInput(msgs)
		if fresh {
			narrativeFired = false
			permissionFired = false
		}
		// Per-task budget gate. Mirrors the inline post-turn budget
		// abort: TransformContext fires before each Stream so the
		// latched flag halts the loop before another provider call
		// commits more tokens. The hard cap still terminates the run.
		if err := a.foundationBudgetCheck(); err != nil {
			return nil, err
		}
		msgs, err := a.foundationCompactAndLint(ctx, msgs)
		if err != nil {
			return nil, err
		}
		// Refresh the system prompt on fresh developer input so style
		// /terse/autonomous toggles between turns take effect on the
		// next provider call. Mirrors the inline run loop's
		// post-AwaitInput rebuild. Skipped on intra-turn re-entries
		// (steering, follow-up, tool-result-driven loops) where the
		// trailing message is NOT a fresh user input — re-rendering
		// every iteration is wasteful and can race with mid-batch
		// SetTerse/SetStyle calls.
		if fresh && len(msgs) > 0 && msgs[0].Role == "system" {
			msgs[0].Content = a.rebuildSystemPrompt(a.currentMode())
		}
		// Emit AgentInputEstimate + capture the estimate so the
		// translator's TurnUsage handler can populate the *Est fields
		// on AgentTurnUsage. Inline path (turn.go:116) accumulated
		// this into budget.Turn.LastEstimate; under the foundation we
		// stash it on the agent struct, snapshotted under mu.
		est := streaming.EstimateAndBroadcast(msgs, a.activeToolDefs(), a.send)
		a.mu.Lock()
		a.lastEstimate = est
		a.mu.Unlock()
		return msgs, nil
	}

	steering := func(_ context.Context) ([]llm.Message, error) {
		return a.foundationSteering(liveMessages(), &narrativeFired, &permissionFired), nil
	}

	followUp := func(_ context.Context) ([]llm.Message, error) {
		// GetFollowUpMessages fires after a turn with no tool calls AND
		// after steering returned nothing. From here the foundation
		// parks on awaitReply for the developer's next message — which
		// is precisely the inline "AgentWaiting" boundary. Emit it
		// here so frontends know to enable input AND so the wrapper's
		// IsWaiting() flag flips before the foundation parks.
		finished := a.tasksAllComplete()
		a.mu.Lock()
		a.waiting = true
		a.mu.Unlock()
		a.send(event.AgentWaiting{Finished: finished})
		return nil, nil
	}

	onTruncated := func(_ context.Context, c upagent.TruncationContext) (upagent.TruncationResult, error) {
		// Delegate to truncation.Recover with an empty initial slice so
		// the returned messages are exactly the rejection (and optional
		// user nudge) splice we hand back to the foundation. Recover
		// emits its own AgentError to the frontend on every iteration
		// (recovery banner) and on retries-exhausted (terminal abort);
		// returning Retry=false suppresses the foundation's fallback
		// emission so frontends see one error, not two.
		splice, newRetries, err := truncation.Recover(nil, c.ToolCalls, truncationRetries, a.currentProvider(), a.send)
		truncationRetries = newRetries
		if err != nil {
			return upagent.TruncationResult{Retry: false, Messages: splice}, nil
		}
		return upagent.TruncationResult{Retry: true, Messages: splice}, nil
	}

	return upagent.Hooks{
		BeforeToolCall:      before,
		AfterToolCall:       after,
		TransformContext:    transform,
		GetSteeringMessages: steering,
		GetFollowUpMessages: followUp,
		OnTruncated:         onTruncated,
	}
}

// foundationAfterToolCall mirrors two inline post-dispatch concerns
// from [coding/agent]:
//
//   - bash-tool side effects ([Agent.afterToolDispatch], turn.go:46-54):
//     invalidate the file cache and ask the frontend to reload open
//     buffers because bash can mutate disk outside the edit-approval
//     flow. Fires for every "bash" tool call regardless of dispatch
//     outcome — the inline triggers it after dispatchTool returns,
//     including the planning-blocked + active-task-blocked early
//     returns; we preserve that behavior so the frontend stays
//     synchronized.
//
//   - intent-reminder appending ([Agent.intentReminder], gates.go:107):
//     append the developer's current intent to the tool result so the
//     LLM sees it on every successful or unknown-tool turn. NOT
//     appended on the BeforeToolCall.Block path because the inline
//     dispatchTool returns early before [Agent.intentReminder]
//     appends. The blocked argument carries that signal — true means
//     suppress the reminder.
//
// The reminder is appended via [upagent.AfterToolCallResult.Content]
// override; bash side effects run as method calls on a so they fire
// regardless of whether the result body is rewritten.
func (a *Agent) foundationAfterToolCall(c upagent.AfterToolCallContext, blocked bool) upagent.AfterToolCallResult {
	if strings.ToLower(c.Name) == "bash" {
		a.cache.Reset("", "")
		a.send(event.ReloadBuffers{})
	}
	if blocked {
		return upagent.AfterToolCallResult{}
	}
	reminder := a.intentReminder()
	if reminder == "" {
		return upagent.AfterToolCallResult{}
	}
	merged := c.Result.Content + reminder
	return upagent.AfterToolCallResult{Content: &merged}
}

// isFreshUserInput reports whether the trailing message of msgs is a
// developer input (role=user, content not authored by a known nudge).
// Used as the per-developer-input boundary signal in TransformContext;
// matches the inline reset point in [Agent.runLoop] (run.go:212-213).
//
// The content comparison is conservative — a developer who echoes a
// nudge string verbatim will not refresh their nudge budget. Acceptable
// in practice because the nudge messages are long and specific; a
// follow-up commit can swap in a structural marker (e.g., a
// llm.Message metadata field) if false negatives are observed.
func isFreshUserInput(msgs []llm.Message) bool {
	if len(msgs) == 0 {
		return false
	}
	tail := msgs[len(msgs)-1]
	if tail.Role != "user" {
		return false
	}
	return tail.Content != nudges.OutstandingNudgeMessage &&
		tail.Content != nudges.PermissionNudgeMessage
}

// foundationSteering mirrors [Agent.tryInjectPostTurnNudge] in
// GetSteeringMessages shape: returns the steering message slice (or
// nil) instead of mutating a shared *[]llm.Message. Each gate is
// one-shot per developer input; firedFlags is shared with
// TransformContext via the closure so resets cross hook boundaries.
//
// Order matches inline: narrative first, then permission. Permission
// is autonomous-only.
func (a *Agent) foundationSteering(msgs []llm.Message, narrativeFired, permissionFired *bool) []llm.Message {
	if !*narrativeFired && a.shouldNudgeOutstanding(msgs) {
		*narrativeFired = true
		a.send(event.AgentToken{Text: "\n[Nudge: outstanding-work language detected — track or scrub it]\n"})
		a.send(event.AgentStatus{Status: event.StatusThinking})
		return []llm.Message{{Role: "user", Content: nudges.OutstandingNudgeMessage}}
	}
	if !*permissionFired && a.currentAutonomous() && nudges.ShouldNudgePermissionQuestion(msgs) {
		*permissionFired = true
		a.send(event.AgentToken{Text: "\n[Nudge: permission-seeking question detected in autonomous mode — act, don't ask]\n"})
		a.send(event.AgentStatus{Status: event.StatusThinking})
		return []llm.Message{{Role: "user", Content: nudges.PermissionNudgeMessage}}
	}
	return nil
}

// planningBlocklistGate mirrors the planning-mode check at turn.go:289.
// Stateless: reads [Agent.mode] and [Agent.planningBlocklist]. The
// schema filter ([Agent.planningToolDefs]) already removes blocked
// tools from the advertised list, but a model can still dispatch one
// that name-collides; this gate is the dispatch-time backstop.
func (a *Agent) planningBlocklistGate(c upagent.BeforeToolCallContext) upagent.BeforeToolCallResult {
	name := strings.ToLower(c.Name)
	if a.currentMode() != ModePlanning {
		return upagent.BeforeToolCallResult{}
	}
	if !a.planningBlocklist[name] {
		return upagent.BeforeToolCallResult{}
	}
	return upagent.BeforeToolCallResult{
		Block:  true,
		Reason: fmt.Sprintf("Error: tool %q is not available in planning mode", c.Name),
	}
}

// activeTaskGate wraps [Agent.enforceActiveTaskGate] in the
// [upagent.BeforeToolCallResult] shape. The underlying gate handles
// the planning-mode carve-out, the non-mutating-tool carve-out, the
// missing-TaskReader carve-out, and the work-tree-not-loaded carve-out
// — see gates.go:80 for the full table.
func (a *Agent) activeTaskGate(ctx context.Context, c upagent.BeforeToolCallContext) upagent.BeforeToolCallResult {
	msg := a.enforceActiveTaskGate(ctx, strings.ToLower(c.Name))
	if msg == "" {
		return upagent.BeforeToolCallResult{}
	}
	return upagent.BeforeToolCallResult{Block: true, Reason: msg}
}

// singleEditGate mirrors the one-file-edit-per-turn enforcement at
// turn.go:198. In non-autonomous, non-headless (interactive) mode the
// agent must wait for the developer to review the previous edit before
// proposing the next one; the gate flips firedThisTurn the first time
// a file-edit tool dispatches in a turn and rejects every subsequent
// file-edit until TransformContext clears the flag.
//
// firedThisTurn is a pointer so the closure in [Agent.FoundationHooks]
// owns the state; the gate function itself is stateless.
func (a *Agent) singleEditGate(c upagent.BeforeToolCallContext, firedThisTurn *bool) upagent.BeforeToolCallResult {
	if !a.shouldEnforceSingleEdit() {
		return upagent.BeforeToolCallResult{}
	}
	name := strings.ToLower(c.Name)
	if !fileEditTools[name] {
		return upagent.BeforeToolCallResult{}
	}
	if *firedThisTurn {
		slog.Info("agent: rejecting extra file-edit in same turn",
			"tool", name, "id", c.CallID)
		return upagent.BeforeToolCallResult{
			Block: true,
			Reason: "Skipped — only ONE file-edit per turn in interactive mode. " +
				"Wait for the developer to review the previous edit, then make this change " +
				"in a follow-up turn. The next tool result will include the updated file content.",
		}
	}
	*firedThisTurn = true
	return upagent.BeforeToolCallResult{}
}

// lintPendingGate mirrors the per-call skip at
// [Agent.executeToolCalls] (turn.go:188-194). When a previous edit's
// lint run set [Agent.pendingLint] mid-batch, every subsequent tool
// in the batch is rejected with the "fix lint first" placeholder so
// the LLM cannot work around the diagnostic.
//
// Distinct from [Agent.foundationCompactAndLint] which DRAINS
// pendingLint into the message slice at TransformContext time
// (concern #3): drain runs once per Stream, this gate runs once per
// tool call within the resulting batch. The two cooperate — the
// drained message arrives at the LLM's next turn while the gate
// keeps the current batch from making progress on the broken state.
func (a *Agent) lintPendingGate() upagent.BeforeToolCallResult {
	if !a.hasLintPending() {
		return upagent.BeforeToolCallResult{}
	}
	return upagent.BeforeToolCallResult{
		Block:  true,
		Reason: "Skipped — fix style lint violations first.",
	}
}

// shouldEnforceSingleEdit returns true when one-edit-per-turn applies:
// non-autonomous, non-headless mode. Mirrors the inline expression in
// [Agent.executeToolCalls] (turn.go:184).
func (a *Agent) shouldEnforceSingleEdit() bool {
	return !a.currentAutonomous() && a.interactionMode != Headless
}

// foundationCompactAndLint runs the per-Stream context-shaping the
// inline path performs in two places: [streaming.MaybeCompact] at the
// outer runLoop boundary (run.go:137) and [Agent.drainPendingLint]
// inside processLLMTurn (turn.go:112). Both cluster naturally on
// TransformContext because they decide what the message slice looks
// like just before the provider sees it.
//
// Cadence note: TransformContext fires once per Stream, so
// MaybeCompact runs more often than the inline once-per-processLLMTurn
// call. The token-threshold guard inside MaybeCompact short-circuits
// when nothing's changed, so the only added cost is one
// [llm.EstimateMessageTokens] call per Stream.
//
// Out of scope (the plan groups them under concern #3 but they
// don't fit TransformContext):
//   - Intent reminder appends to every tool result Content; routed
//     to AfterToolCall (concern #5) so the application-level shape
//     mirrors inline.
//   - [streaming.EstimateAndBroadcast] (frontend input-estimate
//     event); deferred until the post-cutover wrapper can accumulate
//     [budget.Turn].LastEstimate where the inline accumulator lives.
func (a *Agent) foundationCompactAndLint(_ context.Context, msgs []llm.Message) ([]llm.Message, error) {
	msgs = streaming.MaybeCompact(msgs, a.activeToolDefs(), a.send)
	if lint := a.drainPendingLint(); lint != "" {
		msgs = append(msgs, llm.Message{Role: "user", Content: lint})
	}
	return msgs, nil
}

// activeToolDefs returns the tool-definition slice the LLM sees for
// the agent's current mode. ModePlanning gets the blocklist-filtered
// subset; everything else returns the full set unchanged. Mirrors the
// activeDefs computation at [Agent.runLoop] (run.go:121-124).
func (a *Agent) activeToolDefs() []llm.ToolDef {
	if a.currentMode() == ModePlanning {
		return a.planningToolDefs()
	}
	return a.toolDefs
}

// foundationBudgetCheck mirrors the inline post-turn budget gate
// without the retired runID race guard. When the per-task cap is
// crossed the latch flips and a wrapped [errBudgetExceeded] carrying
// the formatted budget message is returned; the foundation's
// TransformContext-error path emits [event.Error] which the
// translator re-emits as [event.AgentError] — single user-facing
// emission, no double-error.
//
// Returns nil when the cap is disabled or not yet crossed. The latch
// (a.budgetExceeded) prevents the abort from re-firing on subsequent
// TransformContext calls if a turn somehow re-enters past the abort
// signal — defensive parity with the inline check.
func (a *Agent) foundationBudgetCheck() error {
	a.mu.Lock()
	if a.budgetExceeded {
		a.mu.Unlock()
		return errBudgetExceeded
	}
	msg, exceeded := budget.Exceeded(a.sessionUsage, a.taskTokenBudget)
	if !exceeded {
		a.mu.Unlock()
		return nil
	}
	a.budgetExceeded = true
	a.mu.Unlock()

	slog.Warn("agent: task token budget exceeded; aborting (foundation hook)", "msg", msg)
	return fmt.Errorf("%w: %s", errBudgetExceeded, msg)
}
