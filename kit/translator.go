package kit

import (
	"sync/atomic"

	agentevent "github.com/latebit-io/nib/agent/event"
	"github.com/latebit-io/nib/kit/event"
)

// translateEvents drains the foundation's event channel and publishes
// each event as the closest [event] equivalent on [Agent.bus], which
// fans it out to every registered [Subscription]. The translator is
// the SOLE driver of:
//
//   - [event.AgentToken] streaming text
//   - [event.AgentDone] success/failure dispatch (success := !runUnsuccessful)
//   - [event.AgentError] dispatch on foundation [agentevent.Error]
//   - [event.AgentWaiting] dispatch on foundation [agentevent.AgentParked].
//     Routing AgentWaiting through the translator (rather than letting
//     hooks send it directly to the consumer channel) preserves order
//     with trailing AgentToken/AgentDone — both writers are now the
//     translator goroutine, so a consumer's AgentWaiting can never
//     overtake an in-flight AgentToken from the same turn.
//   - [event.AgentTurnUsage] passthrough of the provider-reported
//     counts; the estimate fields stay zero here (coding's provider
//     proxy emits the estimating variant directly)
//
// Foundation events that have no kit equivalent fall through silently:
//
//   - AgentStart, TurnStart, TurnEnd: foundation observability;
//     applications that need them subscribe to the foundation directly.
//   - MessageStart / MessageEnd: streaming brackets; AgentToken carries
//     the deltas, MessageEnd's full assistant message is available via
//     [Agent.State] for applications that snapshot at run-end.
//   - ToolStart / ToolEnd: tool dispatch surface. Kit
//     does not translate these because the translation policy is
//     application-specific — coding suppresses [event.AgentToolCall]
//     for blocked-by-gate calls and emits from its BeforeToolCall
//     hook instead. A non-coding consumer that wants every attempted
//     tool call surfaced wires a 3-line BeforeToolCall hook of its own.
//
// Lifecycle: started by [New], runs until [Agent.Close] signals
// shutdown by closing [Agent.done]. On the shutdown branch the
// translator drains any events the foundation enqueued before Close
// returned — Close has already waited for the foundation to unwind
// via [agent.Agent.WaitForIdle], so the drain is bounded and the
// final [agentevent.AgentEnd] still reaches every subscriber as
// [event.AgentDone]. Without the drain, a consumer that called Close
// while a run was unwinding would miss the final lifecycle event.
func (a *Agent) translateEvents() {
	// Signal Close that the translator has fully exited and will not
	// publish to the bus again, so Close can safely call bus.close
	// without racing a still-running drain.
	defer close(a.translatorDone)
	for {
		select {
		case ev := <-a.foundationEvents:
			a.translate(ev)
		case <-a.done:
			for {
				select {
				case ev := <-a.foundationEvents:
					a.translate(ev)
				default:
					return
				}
			}
		}
	}
}

// translate dispatches a single foundation event through the
// foundation→kit mapping table. Extracted from [Agent.translateEvents]
// so the steady-state path and the post-Close drain path share one
// switch — divergence between them would silently mistranslate the
// final [agentevent.AgentEnd] of a run interrupted by [Agent.Close].
//
// Per-run outcome lifecycle:
//
//   - AgentStart binds [Agent.pendingOutcome] (parked by Prompt) into
//     [Agent.currentOutcome]. From this point on, [Agent.Cancel] and
//     this function's Error case mark the bound outcome.
//   - Error sets the current outcome's unsuccess bit. If no outcome
//     is bound (Error somehow fired before AgentStart was processed),
//     the bit is dropped — better than panicking, and the consumer
//     still observes [event.AgentError].
//   - AgentEnd consumes [Agent.currentOutcome], reads its unsuccess
//     bit, emits [event.AgentDone] with the derived Success, and
//     unbinds. After AgentEnd the agent has no outcome bound until
//     the next Prompt + AgentStart.
func (a *Agent) translate(ev agentevent.Event) {
	switch e := ev.(type) {
	case agentevent.AgentStart:
		a.bindOutcome()
	case agentevent.MessageUpdate:
		a.bus.publish(event.AgentToken{Text: e.Delta})
	case agentevent.TurnUsage:
		a.bus.publish(event.AgentTurnUsage{
			Turn:             e.Turn,
			PromptTokens:     e.PromptTokens,
			CompletionTokens: e.CompletionTokens,
			CachedTokens:     e.CachedTokens,
			ToolCalls:        e.ToolCalls,
		})
	case agentevent.Error:
		a.markCurrentUnsuccess()
		a.bus.publish(event.AgentError{Err: e.Err})
	case agentevent.AgentParked:
		a.bus.publish(event.AgentWaiting{Finished: e.Finished})
	case agentevent.AgentEnd:
		a.bus.publish(event.AgentDone{Success: !a.consumeCurrentUnsuccess()})
	}
}

// bindOutcome moves [Agent.pendingOutcome] into [Agent.currentOutcome]
// when the translator processes [agentevent.AgentStart]. After binding
// the outcome is the run's identity for kit's success-flag accounting.
//
// If pendingOutcome is nil at AgentStart time (the foundation emitted
// AgentStart without a preceding kit.Prompt — should be impossible
// today but defensive against future foundation paths), bindOutcome
// allocates a fresh outcome so AgentEnd has something to consume.
func (a *Agent) bindOutcome() {
	a.outcomeMu.Lock()
	o := a.pendingOutcome
	if o == nil {
		o = &runOutcome{}
	}
	a.currentOutcome = o
	a.pendingOutcome = nil
	a.outcomeMu.Unlock()
}

// markCurrentUnsuccess sets the bound outcome's unsuccess bit. Called
// by the translator on [agentevent.Error]. A nil currentOutcome means
// Error fired without a bound run — drop the mark rather than panic;
// the consumer still observes the AgentError event itself.
//
// Lock scope mirrors [Agent.markCurrentOrPendingUnsuccess]: the lock
// guards the pointer read; the atomic store happens after release
// because o stays stable until [Agent.consumeCurrentUnsuccess] (same
// translator goroutine, ordered after this call), and the atomic
// synchronizes the read on AgentEnd.
func (a *Agent) markCurrentUnsuccess() {
	a.outcomeMu.Lock()
	o := a.currentOutcome
	a.outcomeMu.Unlock()
	if o != nil {
		atomic.StoreUint32(&o.unsuccess, 1)
	}
}

// consumeCurrentUnsuccess reads and unbinds the current outcome.
// Called by the translator on [agentevent.AgentEnd] to derive
// [event.AgentDone].Success. Returns false (treat as success) if no
// outcome is bound — defensive against AgentEnd-without-AgentStart,
// same reasoning as [Agent.bindOutcome]'s nil-pending fallback.
func (a *Agent) consumeCurrentUnsuccess() bool {
	a.outcomeMu.Lock()
	o := a.currentOutcome
	a.currentOutcome = nil
	a.outcomeMu.Unlock()
	if o == nil {
		return false
	}
	return atomic.LoadUint32(&o.unsuccess) == 1
}
