package agent

import (
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
//     (the foundation does not populate them) and accumulated into
//     [Agent.sessionUsage]. Post-turn budget overrun fires AgentError
//     + Abort here so a single-turn overrun does not get hidden by
//     the wrapper parking on AwaitInput before TransformContext can
//     re-fire its pre-Stream check.
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
func (a *Agent) forwardKitEvents() {
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

// augmentAndAccumulate accumulates one [event.AgentTurnUsage] into
// [Agent.sessionUsage] and returns a copy with the client-side
// estimate fields populated. The foundation does not set the *Est
// fields — applications that want them layer their own estimation in
// a TransformContext hook (see [estimateAndBroadcast] in
// foundation_hooks.go) and pair it back to the matching turn here.
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
	a.sessionUsage.TotalPromptTokens += e.PromptTokens
	a.sessionUsage.TotalCompletionTokens += e.CompletionTokens
	a.sessionUsage.TotalCachedTokens += e.CachedTokens
	a.sessionUsage.Turns = turn
	var estimate llm.InputEstimate
	if len(a.estimateQueue) > 0 {
		estimate = a.estimateQueue[0]
		a.estimateQueue = a.estimateQueue[1:]
	}
	a.mu.Unlock()

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
