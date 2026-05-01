package agent

import (
	"slices"

	upevent "github.com/latebit-io/nib/agent/event"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/event"
)

// Foundation → engine event translator.
//
// The foundation [upagent.Agent] emits its own loop-lifecycle events
// (MessageStart/Update/End, TurnStart/End, ToolStart/Update/End,
// TurnUsage, AgentStart, AgentEnd, Error). Frontends speak the
// application's [engine/event] vocabulary (AgentToken, AgentStatus,
// AgentTurnUsage, AgentDone, AgentError, …). The translator is the
// goroutine that bridges the two: drains the foundation's events
// channel, re-emits each event as the closest engine equivalent, and
// updates a few wrapper-level state flags (running, savedMessages)
// along the way.
//
// Spawned once in [New] and lives the agent's lifetime; the foundation
// never closes its events channel, and the translator never returns
// from the for-range. This goroutine is the SOLE driver of:
//
//   - [Agent.running] flips (true on AgentStart, false on AgentEnd)
//   - [Agent.savedMessages] / [Agent.savedMode] commits at run-end so
//     [Agent.Reply]'s resume path has somewhere to read from
//   - [Agent.recordTurnUsage] dispatch on TurnUsage events (the
//     concern-#6 accumulator side that 8b deferred to 8c)
//   - [event.AgentToken] streaming text to the frontend
//   - [event.AgentDone] success/failure dispatch (success := !errored)
//   - the inter-turn `\n\n` separator that the inline run loop
//     emitted after AwaitInput
//
// What the translator does NOT do:
//   - Translate ToolStart / ToolEnd / ToolUpdate. The wrapper's
//     BeforeToolCall hook emits [event.AgentToolCall] directly after
//     the lint+single-edit gates clear, mirroring the inline ordering
//     so blocked-by-lint and blocked-by-single-edit calls never reach
//     the frontend.
//   - Translate Compacted. Compaction events come from
//     [streaming.MaybeCompact] inside TransformContext, which sends
//     [event.AgentCompacted] directly to the engine channel — the
//     foundation's own Compacted event is unused by this application.
//   - Translate InputEstimate. Same pattern: the application's
//     TransformContext computes the estimate via
//     [streaming.EstimateAndBroadcast] and emits AgentInputEstimate
//     directly.

// translateFoundationEvents drains [Agent.foundationEvents] and
// re-emits each event as the closest [engine/event] equivalent through
// [Agent.send]. Per-run state (turn count, content estimate) lives in
// local variables so a fresh run starts clean without an explicit
// reset path; the unsuccess decision lives on [Agent.runUnsuccessful]
// because engine-side AgentErrors (truncation, autosave, budget) and
// user cancellation never appear in the foundation's event stream.
func (a *Agent) translateFoundationEvents() {
	var (
		turnCount      int
		lastContentEst int
	)
	for ev := range a.foundationEvents {
		switch e := ev.(type) {
		case upevent.AgentStart:
			a.mu.Lock()
			a.running = true
			a.waiting = false
			a.mu.Unlock()
			turnCount = 0
			lastContentEst = 0
		case upevent.AgentEnd:
			a.commitRunEnd(e)
		case upevent.TurnStart:
			turnCount++
			if turnCount > 1 {
				// Inline behavior: between-turn separator that
				// rendered after AwaitInput before the next turn's
				// tokens arrived. Skipped on the first turn because
				// RunWithMode/Reply already emitted "Thinking..." or
				// "Resuming...".
				a.send(event.AgentToken{Text: "\n\n"})
				a.send(event.AgentStatus{Status: event.StatusThinking})
			}
		case upevent.MessageUpdate:
			a.send(event.AgentToken{Text: e.Delta})
		case upevent.MessageEnd:
			// Capture the streamed assistant content size so the
			// next TurnUsage event can populate
			// AgentTurnUsage.CompletionEst. The foundation's
			// TurnUsage carries provider counts only; this estimate
			// is the inline equivalent of the `tu.CompletionEst +=
			// llm.EstimateTokens(result.Content)` accumulation the
			// turn pipeline did before each record commit.
			lastContentEst = llm.EstimateTokens(e.Message.Content)
		case upevent.TurnEnd:
			// AgentWaiting fires from the GetFollowUpMessages hook;
			// nothing to emit here. TurnEnd is an observability
			// marker for capture/journaling, not part of the engine
			// vocabulary.
		case upevent.TurnUsage:
			a.recordTurnUsage(e, lastContentEst)
			lastContentEst = 0
			// Inline post-turn budget gate. The foundation's
			// TransformContext-side check fires only on the next
			// turn — for a single-turn overrun where the agent
			// would otherwise park at AgentWaiting, the developer
			// would never see the abort because TransformContext
			// never fires while parked. This translator-side check
			// closes that gap: cancel the run immediately so the
			// foundation unwinds via AgentEnd before reaching the
			// awaitReply park.
			if msg := a.checkTaskBudget(); msg != "" {
				a.send(event.AgentError{Err: msg})
				a.Cancel()
			}
		case upevent.Error:
			a.send(event.AgentError{Err: e.Err})
		}
		// Unhandled cases (ToolStart/Update/End, InputEstimate,
		// Compacted) fall through silently — see the package-doc
		// rationale for why they are not translated.
	}
}

// commitRunEnd handles the foundation's [upevent.AgentEnd] under
// [Agent.mu]: snapshots the final transcript into savedMessages so
// [Agent.Reply]'s resume path can pick it up, flips running/waiting
// off, reads the runUnsuccessful flag (set by [Agent.send] on
// AgentError emission and by [Agent.Cancel]), and emits the
// engine-level [event.AgentDone] with the derived success flag.
func (a *Agent) commitRunEnd(e upevent.AgentEnd) {
	a.mu.Lock()
	unsuccessful := a.runUnsuccessful
	a.running = false
	a.waiting = false
	a.savedMessages = slices.Clone(e.Messages)
	a.savedMode = a.mode
	a.mu.Unlock()
	a.send(event.AgentDone{Success: !unsuccessful})
}
