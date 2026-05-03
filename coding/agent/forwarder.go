package agent

import (
	"log/slog"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/event"
)

// Kit-event forwarder.
//
// [Agent.forwardKitEvents] drains the kit agent's translated event
// stream from [Agent.kitEvents] and re-emits each event to the
// frontend channel. Two events get coding-specific treatment:
//
//   - [event.AgentTurnUsage] is augmented with client-side estimates
//     and per-turn metadata (turn number, completion estimate). Per-
//     Stream usage accumulation lives on [providerProxy] so the
//     foundation's pre-Stream budget check sees fresh totals
//     synchronously; the post-turn overrun fired here is a backstop
//     for the single-turn overrun case where the agent would
//     otherwise park on AwaitInput before the next TransformContext
//     check could fire.
//   - [event.AgentDone] is intercepted to flip [Agent.running] off
//     and to override Success with [Agent.runUnsuccessful] — kit's
//     per-run outcome only knows about Aborts and foundation Errors,
//     so coding-side AgentErrors emitted via [Agent.send] (autosave
//     failure, RunWithMode rejection) need this wrapper-level flag
//     to surface as Success=false.
//
// The forwarder also flips [Agent.waiting] off when AgentDone fires
// — AgentWaiting from [GetFollowUpMessages] sets it on; AgentDone
// resets it.
//
// Lifecycle: spawned once in [Agent.buildKitAgent]; exits when
// [Agent.Close] closes [Agent.kitEvents] after kit.Close has waited
// for the foundation to unwind.

// forwardKitEvents drains [Agent.kitEvents] and forwards each event
// (after coding-specific augmentation) to the frontend events channel
// via [Agent.send].
//
// Closes [Agent.forwardDone] on return so [Agent.Close] can block on
// it after closing kitEvents — providing callers a synchronous "no
// further writes to the frontend channel" guarantee symmetric to
// kit.Agent.Close's translatorDone wait.
func (a *Agent) forwardKitEvents() {
	defer close(a.forwardDone)
	for ev := range a.kitEvents {
		switch e := ev.(type) {
		case event.AgentTurnUsage:
			ev = a.augmentAndAccumulate(e)
		case event.AgentDone:
			a.mu.Lock()
			unsuccessful := a.runUnsuccessful
			a.running = false
			a.waiting = false
			a.mu.Unlock()
			if unsuccessful && e.Success {
				ev = event.AgentDone{Success: false}
			}
		}
		a.send(ev)
		// Post-turn budget gate. Runs AFTER the augmented AgentTurnUsage
		// reaches the frontend so the cost panel renders the latest
		// totals before the abort error blanks the UI.
		if _, ok := ev.(event.AgentTurnUsage); ok {
			if msg := a.checkTaskBudget(); msg != "" {
				a.send(event.AgentError{Err: msg})
				if a.kit != nil {
					a.kit.Abort()
				}
			}
		}
		// Run-boundary signal: close runDone AFTER AgentDone has been
		// fully forwarded (including any Success override and any
		// post-turn budget side effects above) so a follow-up
		// [Agent.RunWithMode] / [Agent.Reply] resume blocked on
		// [Agent.fenceForwarder] unblocks only once this run's tail
		// has actually settled.
		if _, ok := ev.(event.AgentDone); ok {
			a.runDoneMu.Lock()
			if a.runDone != nil {
				close(a.runDone)
				a.runDone = nil
			}
			a.runDoneMu.Unlock()
		}
	}
}

// augmentAndAccumulate returns a copy of e with per-turn metadata
// (turn number, client-side estimate fields, completion estimate)
// populated. Per-Stream session-usage accumulation lives on
// [providerProxy] so the foundation's pre-Stream budget check sees
// fresh totals synchronously; this function only handles the
// per-turn correlation work that needs to flow through the kit
// pipeline.
//
// Per-turn binding:
//
//   - System/Tools/History/NewEst are popped from [Agent.estimateQueue]
//     in FIFO order so each TurnUsage is bound to the estimate from
//     the TransformContext that paired with its Stream call. A single
//     mutable lastEstimate field would, in multi-turn / tool-chained
//     runs, be overwritten by the next TransformContext before the
//     forwarder drained this turn's TurnUsage.
//
//   - CompletionEst is computed from the [Agent.turnCounter]-th
//     assistant message in the kit transcript. Reading "the most
//     recent assistant message" from kit.State() at forwarder time
//     would, in the same multi-turn race, return turn N+1's content
//     for turn N's event because the foundation can advance several
//     turns ahead of the forwarder.
func (a *Agent) augmentAndAccumulate(e event.AgentTurnUsage) event.AgentTurnUsage {
	a.mu.Lock()
	a.turnCounter++
	turn := a.turnCounter
	var estimate llm.InputEstimate
	queueDesync := len(a.estimateQueue) == 0
	if !queueDesync {
		estimate = a.estimateQueue[0]
		a.estimateQueue = a.estimateQueue[1:]
	}
	a.mu.Unlock()

	// Per-Stream session-usage accumulation lives on [providerProxy] —
	// updated synchronously in the Stream wrapper goroutine when the
	// inner provider emits Done. The forwarder's responsibility here
	// is per-turn metadata (turn number, estimate binding, completion
	// estimate) only; the budget gate reads providerProxy directly so
	// it sees fresh data even when this goroutine is behind.

	// Surface foundation-invariant breaks loudly. The pairing assumes
	// the foundation emits one TurnUsage per TransformContext; if a
	// future refactor decouples them, the queue desyncs and *Est
	// fields silently fall back to zero — the cost panel would show
	// confusing zeros without any signal that something is wrong.
	// Logging here makes the desync visible during development and
	// in production telemetry.
	if queueDesync {
		slog.Warn("agent: AgentTurnUsage with empty estimateQueue; *Est fields will be zero",
			"turn", turn,
			"prompt_tokens", e.PromptTokens,
			"completion_tokens", e.CompletionTokens)
	}

	completionEst := 0
	if a.kit != nil {
		if msg := nthAssistantMessage(a.kit.State().Messages, turn); msg != nil {
			completionEst = llm.EstimateTokens(msg.Content)
		}
	}

	return event.AgentTurnUsage{
		Turn:             turn,
		PromptTokens:     e.PromptTokens,
		CompletionTokens: e.CompletionTokens,
		CachedTokens:     e.CachedTokens,
		ToolCalls:        e.ToolCalls,
		SystemEst:        estimate.System,
		ToolsEst:         estimate.Tools,
		HistoryEst:       estimate.History,
		NewEst:           estimate.New,
		CompletionEst:    completionEst,
	}
}

// nthAssistantMessage returns a pointer to the n-th (1-indexed)
// assistant-role message in msgs, or nil if fewer than n exist. Used
// by [Agent.augmentAndAccumulate] to pin CompletionEst to the
// originating turn's content rather than to the most recent assistant
// message (which could belong to a later turn the foundation has
// already advanced to).
func nthAssistantMessage(msgs []llm.Message, n int) *llm.Message {
	count := 0
	for i := range msgs {
		if msgs[i].Role != "assistant" {
			continue
		}
		count++
		if count == n {
			return &msgs[i]
		}
	}
	return nil
}
