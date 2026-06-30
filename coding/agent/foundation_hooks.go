package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	upagent "github.com/latebit-io/nib/agent"
	upevent "github.com/latebit-io/nib/agent/event"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/coding/nudges"
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
// FoundationHooks builds the [upagent.Hooks] value the foundation
// [upagent.Agent] consumes to drive the run loop. Each gate is a small,
// stateless method on *Agent that owns one decision the loop makes;
// FoundationHooks composes them and captures any per-turn state in
// closure variables so the Agent struct stays free of hook-only fields.

// FoundationHooks returns the [upagent.Hooks] value bound to this
// agent. Closure-captured state (narrativeFired, permissionFired,
// truncationRetries) lives for the lifetime of the returned Hooks;
// a fresh FoundationHooks call yields a fresh closure.
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
	// narrativeFired and permissionFired are the per-developer-input
	// guards. Reset by TransformContext when a fresh user message
	// lands at the tail of the transcript (one not authored by a
	// nudge).
	var (
		narrativeFired  bool
		permissionFired bool
	)
	before := func(ctx context.Context, c upagent.BeforeToolCallInput) (upagent.BeforeToolCallResult, error) {
		// Dispatch order:
		//   1. lint-pending skip
		//   2. flush dirty buffers + emit AgentToolCall
		//   3. planning blocklist
		//   4. lifecycle bundle: activate_task (mutating tools only)
		//   5. active-task gate
		if res := a.lintPendingGate(); res.Block {
			return res, nil
		}
		if err := a.flushDirtyBuffers(ctx); err != nil {
			a.send(event.AgentError{Err: fmt.Sprintf("autosave failed: %v", err)})
			return upagent.BeforeToolCallResult{
				Block:  true,
				Reason: fmt.Sprintf("Skipped — autosave failed: %v", err),
			}, nil
		}
		// Mirrors turn.go:222-224: emit AgentToolCall AFTER the
		// lint-pending gate that suppresses dispatch entirely but
		// BEFORE planning + active-task gates so the frontend sees
		// the call attempt even when it's about to be rejected.
		// Inline behavior: planning + active-task rejections still
		// emit AgentToolCall.
		a.send(event.AgentToolCall{Name: c.Name, Args: c.Args})
		if res := a.planningBlocklistGate(c); res.Block {
			return res, nil
		}
		// Lifecycle activate fires only for mutating tools and only
		// when activate_task is non-empty. A successful activation
		// makes the following active-task gate pass naturally; a
		// failed activate aborts dispatch with the activate error.
		// Strict back-compat: the zero-value bundle is a no-op.
		if mutatingTools[strings.ToLower(c.Name)] {
			bundle := parseLifecycleBundle(c.Args)
			if res := a.runLifecycleActivate(ctx, bundle, c.Name); res.Block {
				return res, nil
			}
		}
		if res := a.activeTaskGate(ctx, c); res.Block {
			return res, nil
		}
		// Plugin PreToolUse runs LAST — only for calls nib's own gates
		// cleared, matching CC's "final gate before execution". A deny
		// becomes a Block, never a returned error (terminal).
		if res := a.pluginPreToolUseGate(ctx, c); res.Block {
			return res, nil
		}
		return upagent.BeforeToolCallResult{}, nil
	}

	after := func(ctx context.Context, c upagent.AfterToolCallInput) (upagent.AfterToolCallResult, error) {
		res := a.foundationAfterToolCall(c)
		// Lifecycle complete fires only for mutating tools that
		// succeeded with complete_task=true set. The wrapper
		// overrides the tool's content text with the auto-complete
		// trailer ("Task completed: <title>" + review findings +
		// "Next task auto-activated: ..."); IsError is left alone.
		// Strict back-compat: the zero-value bundle leaves the
		// result untouched.
		if mutatingTools[strings.ToLower(c.Name)] {
			bundle := parseLifecycleBundle(c.Args)
			if override := a.runLifecycleComplete(ctx, bundle, c.Result); override != nil {
				res.Content = override
			}
		}
		// Plugin PostToolUse runs last; a deny flips the result to an
		// error with the hook's reason (overriding any trailer above).
		res = a.pluginPostToolUse(ctx, c, res)
		return res, nil
	}

	transform := func(ctx context.Context, msgs []llm.Message) ([]llm.Message, error) {
		fresh := isFreshUserInput(msgs)
		if fresh {
			narrativeFired = false
			permissionFired = false
		}
		// Per-task budget gate. TransformContext fires before each
		// Stream so the latched flag halts the loop before another
		// provider call commits more tokens.
		if err := a.foundationBudgetCheck(); err != nil {
			return nil, err
		}
		msgs, err := a.foundationCompactAndLint(ctx, msgs)
		if err != nil {
			return nil, err
		}
		// Refresh the system prompt on fresh developer input so a
		// terse toggle between turns takes effect on the next provider
		// call. Skipped on intra-turn re-entries
		// (steering, follow-up, tool-result-driven loops) where the
		// trailing message is NOT a fresh user input — re-rendering
		// every iteration is wasteful and can race with mid-batch
		// SetTerse/SetStyle calls.
		if fresh && len(msgs) > 0 && msgs[0].Role == "system" {
			msgs[0].Content = a.rebuildSystemPrompt(a.currentMode())
		}
		// AgentInputEstimate emission and per-turn estimate
		// pairing live on [providerProxy] — its Stream wrapper is
		// the only point where we can synchronously commit the
		// estimate next to the matching usage WITHOUT depending on
		// kit's lossy AgentTurnUsage delivery (kit drops streaming
		// events when the consumer channel is full, which would
		// desync any forwarder-side queue indefinitely).
		return msgs, nil
	}

	steering := func(_ context.Context) ([]llm.Message, error) {
		return a.foundationSteering(liveMessages(), &narrativeFired, &permissionFired), nil
	}

	followUp := func(_ context.Context) ([]llm.Message, error) {
		// No follow-up messages — coding has no run-as-a-whole work
		// queue beyond what steering injects mid-turn. The
		// AgentWaiting emission moved to [beforePark] so it flows
		// through the same translator goroutine as AgentToken; sending
		// it from here raced trailing AgentTokens still queued in the
		// foundation→kit translator pipeline.
		return nil, nil
	}

	beforePark := func(_ context.Context) (upevent.AgentParked, error) {
		// BeforePark fires after FollowUpMessages returned nothing
		// and immediately before the foundation parks on awaitReply.
		// We flip [waiting] here (frontends query IsWaiting) and hand
		// the foundation the Finished bit so kit's translator can
		// emit [event.AgentWaiting{Finished}] in stream order.
		finished := a.tasksAllComplete()
		a.mu.Lock()
		a.waiting = true
		a.mu.Unlock()
		return upevent.AgentParked{Finished: finished}, nil
	}

	onTruncated := func(_ context.Context, c upagent.TruncationInput) (upagent.TruncationResult, error) {
		// Delegate to recoverFromTruncation with an empty initial slice so
		// the returned messages are exactly the rejection (and optional
		// user nudge) splice we hand back to the foundation. Recover
		// emits its own AgentError to the frontend on every iteration
		// (recovery banner) and on retries-exhausted (terminal abort);
		// returning Retry=false suppresses the foundation's fallback
		// emission so frontends see one error, not two.
		//
		// The retry counter lives on [Agent.truncationRetries] (reset
		// at each run boundary in RunWithMode/Reply) — closure state
		// would leak across runs because FoundationHooks is built once
		// at [New] time.
		a.mu.Lock()
		retries := a.truncationRetries
		a.mu.Unlock()
		splice, newRetries, err := recoverFromTruncation(nil, c.ToolCalls, retries, a.currentProvider(), a.send)
		a.mu.Lock()
		a.truncationRetries = newRetries
		a.mu.Unlock()
		if err != nil {
			return upagent.TruncationResult{Retry: false, Messages: splice}, nil
		}
		return upagent.TruncationResult{Retry: true, Messages: splice}, nil
	}

	return upagent.Hooks{
		BeforeToolCall:   before,
		AfterToolCall:    after,
		TransformContext: transform,
		SteeringMessages: steering,
		FollowUpMessages: followUp,
		BeforePark:       beforePark,
		OnTruncated:      onTruncated,
	}
}

// foundationAfterToolCall handles bash-tool side effects: invalidate
// the file cache and ask the frontend to reload open buffers because
// bash can mutate disk outside the edit-approval flow. Fires for
// every "bash" tool call regardless of dispatch outcome (including
// planning-blocked and active-task-blocked) so the frontend stays
// synchronized.
func (a *Agent) foundationAfterToolCall(c upagent.AfterToolCallInput) upagent.AfterToolCallResult {
	if strings.ToLower(c.Name) == "bash" {
		a.cache.Reset("", "")
		a.send(event.ReloadBuffers{})
	}
	return upagent.AfterToolCallResult{}
}

// isFreshUserInput reports whether the trailing message of msgs is a
// developer input (role=user, content not authored by a known nudge).
// Used as the per-developer-input boundary signal in TransformContext.
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

// foundationSteering returns the steering message slice (or nil) for
// the SteeringMessages hook. Each gate is one-shot per developer
// input; firedFlags is shared with TransformContext via the closure so
// resets cross hook boundaries.
//
// Order: narrative first, then permission.
func (a *Agent) foundationSteering(msgs []llm.Message, narrativeFired, permissionFired *bool) []llm.Message {
	if !*narrativeFired && a.shouldNudgeOutstanding(msgs) {
		*narrativeFired = true
		a.send(event.AgentToken{Text: "\n[Nudge: outstanding-work language detected — track or scrub it]\n"})
		a.send(event.AgentStatus{Status: event.StatusThinking})
		return []llm.Message{{Role: "user", Content: nudges.OutstandingNudgeMessage}}
	}
	if !*permissionFired && nudges.ShouldNudgePermissionQuestion(msgs) {
		*permissionFired = true
		a.send(event.AgentToken{Text: "\n[Nudge: permission-seeking question detected — act, don't ask]\n"})
		a.send(event.AgentStatus{Status: event.StatusThinking})
		return []llm.Message{{Role: "user", Content: nudges.PermissionNudgeMessage}}
	}
	return nil
}

// planningBlocklistGate enforces the planning-mode tool blocklist.
// Stateless: reads [Agent.mode] and [Agent.planningBlocklist]. The
// schema filter ([Agent.planningToolDefs]) already removes blocked
// tools from the advertised list, but a model can still dispatch one
// that name-collides; this gate is the dispatch-time backstop.
func (a *Agent) planningBlocklistGate(c upagent.BeforeToolCallInput) upagent.BeforeToolCallResult {
	name := strings.ToLower(c.Name)
	if a.currentMode() != event.ModePlanning {
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
// missing-TaskReader carve-out, and the work-tree-not-loaded
// carve-out — see [Agent.enforceActiveTaskGate] for the full table.
func (a *Agent) activeTaskGate(ctx context.Context, c upagent.BeforeToolCallInput) upagent.BeforeToolCallResult {
	msg := a.enforceActiveTaskGate(ctx, strings.ToLower(c.Name))
	if msg == "" {
		return upagent.BeforeToolCallResult{}
	}
	return upagent.BeforeToolCallResult{Block: true, Reason: msg}
}

// lintPendingGate skips dispatch when a previous edit's lint run set
// [Agent.pendingLint]. Every tool call in the batch is rejected with
// the "fix lint first" placeholder so the LLM cannot work around the
// diagnostic.
//
// Distinct from [Agent.foundationCompactAndLint] which DRAINS
// pendingLint into the message slice at TransformContext time: drain
// runs once per Stream, this gate runs once per tool call within the
// resulting batch. The two cooperate — the drained message arrives at
// the LLM's next turn while the gate keeps the current batch from
// making progress on the broken state.
func (a *Agent) lintPendingGate() upagent.BeforeToolCallResult {
	if !a.hasLintPending() {
		return upagent.BeforeToolCallResult{}
	}
	return upagent.BeforeToolCallResult{
		Block:  true,
		Reason: "Skipped — fix style lint violations first.",
	}
}

// foundationCompactAndLint runs the per-Stream context shaping:
// the per-tool-output cap, [maybeCompact] for token-budget
// compaction, and [Agent.drainPendingLint] for surfacing pending
// lint as a user message. All three cluster on TransformContext
// because they decide what the message slice looks like just
// before the provider sees it.
//
// Order matters:
//
//  1. [capStaleToolResults] runs FIRST so the compaction threshold
//     check sees post-cap sizes. Capping plus compaction firing
//     redundantly on the same content would waste the cap (the
//     compactor would re-mutate already-truncated stubs, breaking
//     the cache twice).
//  2. [maybeCompact] runs second; its token-threshold guard
//     short-circuits when nothing has changed, so the per-Stream
//     cost is one [llm.EstimateMessageTokens] call when the cap
//     already bounded growth.
//  3. drainPendingLint appends the trailing user message last so it
//     never enters the cap's stale window (it's by definition the
//     newest content).
//
// The cap is idempotent (see [capStaleToolResults]) and disabled
// when [brand.EnvKeyToolOutputCapDisabled] is set non-empty — the
// commit-2 kill switch for the smoke-testing soak.
func (a *Agent) foundationCompactAndLint(ctx context.Context, msgs []llm.Message) ([]llm.Message, error) {
	msgs = capToolOutputsIfEnabled(msgs)
	msgs = maybeCompact(msgs, a.activeToolDefs(), a.send, func() { a.pluginPreCompact(ctx) })
	if lint := a.drainPendingLint(); lint != "" {
		msgs = append(msgs, llm.Message{Role: "user", Content: lint})
	}
	return msgs, nil
}

// activeToolDefs returns the tool-definition slice the LLM sees for
// the agent's current mode. event.ModePlanning gets the blocklist-
// filtered subset; everything else returns the full set unchanged.
func (a *Agent) activeToolDefs() []llm.ToolDef {
	if a.currentMode() == event.ModePlanning {
		return a.planningToolDefs()
	}
	return a.toolDefs
}

// foundationBudgetCheck enforces the per-task token cap. When crossed,
// the latch flips and a wrapped [errBudgetExceeded] carrying the
// formatted budget message is returned; the foundation's
// TransformContext-error path emits [event.Error] which the translator
// re-emits as [event.AgentError] — single user-facing emission, no
// double-error.
//
// Returns nil when the cap is disabled or not yet crossed. The latch
// (a.budgetExceeded) prevents the abort from re-firing on subsequent
// TransformContext calls if a turn somehow re-enters past the abort
// signal.
func (a *Agent) foundationBudgetCheck() error {
	msg, exceeded := a.evaluateBudgetLatch()
	if !exceeded {
		return nil
	}
	if msg == "" {
		// Already latched on a prior call (re-entrant TransformContext);
		// abort with the bare sentinel — the message was emitted on the
		// first crossing.
		return errBudgetExceeded
	}
	slog.Warn("agent: task token budget exceeded; aborting (foundation hook)", "msg", msg)
	return fmt.Errorf("%w: %s", errBudgetExceeded, msg)
}
