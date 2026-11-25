package kit

import (
	agentevent "github.com/latebit-io/nib/agent/event"
	"github.com/latebit-io/nib/kit/event"
)

// translateEvents drains the foundation's events and publishes each as
// its kit equivalent on [Agent.bus]; events without one are dropped.
// Runs until [Agent.Close], which drains what the last run left behind.
func (a *Agent) translateEvents() {
	// Signal Close that the translator has fully exited and will not
	// publish to the bus again, so Close can safely call Bus.Close
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

// translate maps one foundation event to its kit equivalent. Shared by
// the steady-state and post-Close drain paths, so the final AgentEnd of
// a run interrupted by [Agent.Close] is translated the same way.
func (a *Agent) translate(ev agentevent.Event) {
	switch e := ev.(type) {
	case agentevent.MessageUpdate:
		a.bus.Publish(event.AgentToken{Text: e.Delta})
	case agentevent.TurnUsage:
		a.bus.Publish(event.AgentTurnUsage{
			Turn:             e.Turn,
			PromptTokens:     e.PromptTokens,
			CompletionTokens: e.CompletionTokens,
			CachedTokens:     e.CachedTokens,
			ToolCalls:        e.ToolCalls,
		})
	case agentevent.Error:
		a.bus.Publish(event.AgentError{Err: e.Err})
	case agentevent.AgentParked:
		a.bus.Publish(event.AgentWaiting{Finished: e.Finished})
	case agentevent.MaxTurnsReached:
		a.bus.Publish(event.AgentMaxTurns{Turns: e.Turns})
	case agentevent.AgentEnd:
		a.bus.Publish(doneFrom(e))
	}
}

// doneFrom translates the foundation's run-end record. Success is derived
// from the reason alone: completed, or stopped by policy rather than by
// failure. The usage conversion stops compiling if the two structs drift.
func doneFrom(e agentevent.AgentEnd) event.AgentDone {
	reason := event.EndReason(e.Reason)
	return event.AgentDone{
		Success:   reason == event.EndCompleted || reason == event.EndMaxTurns || reason == event.EndTerminated,
		Reason:    reason,
		Usage:     event.RunUsage(e.Usage),
		FinalText: e.FinalText,
	}
}
