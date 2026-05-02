package kit

import (
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	agentevent "github.com/latebit-io/nib/agent/event"
	"github.com/latebit-io/nib/kit/event"
)

// translateEvents drains the foundation's event channel and re-emits
// each event as the closest [event] equivalent on
// [Agent.consumerEvents]. The translator is the SOLE driver of:
//
//   - [event.AgentToken] streaming text
//   - [event.AgentDone] success/failure dispatch (success := !runUnsuccessful)
//   - [event.AgentError] dispatch on foundation [agentevent.Error]
//   - [event.AgentTurnUsage] / [event.AgentInputEstimate] /
//     [event.AgentCompacted] passthrough with field-for-field copy
//
// Foundation events that have no kit equivalent fall through silently:
//
//   - AgentStart, TurnStart, TurnEnd: foundation observability;
//     applications that need them subscribe to the foundation directly.
//   - MessageStart / MessageEnd: streaming brackets; AgentToken carries
//     the deltas, MessageEnd's full assistant message is available via
//     [Agent.State] for applications that snapshot at run-end.
//   - ToolStart / ToolUpdate / ToolEnd: tool dispatch surface. Kit
//     does not translate these because the translation policy is
//     application-specific — coding suppresses [event.AgentToolCall]
//     for blocked-by-gate calls and emits from its BeforeToolCall
//     hook instead. A non-coding consumer that wants every attempted
//     tool call surfaced wires a 3-line BeforeToolCall hook of its own.
//
// The goroutine is started by [New], drains until the foundation
// channel is closed (which the foundation never does today), and runs
// the lifetime of the agent.
func (a *Agent) translateEvents() {
	for ev := range a.foundationEvents {
		switch e := ev.(type) {
		case agentevent.MessageUpdate:
			a.send(event.AgentToken{Text: e.Delta})
		case agentevent.TurnUsage:
			a.send(event.AgentTurnUsage{
				Turn:             e.Turn,
				PromptTokens:     e.PromptTokens,
				CompletionTokens: e.CompletionTokens,
				CachedTokens:     e.CachedTokens,
				ToolCalls:        e.ToolCalls,
				SystemEst:        e.SystemEst,
				ToolsEst:         e.ToolsEst,
				HistoryEst:       e.HistoryEst,
				NewEst:           e.NewEst,
				CompletionEst:    e.CompletionEst,
			})
		case agentevent.InputEstimate:
			a.send(event.AgentInputEstimate{
				System:  e.System,
				Tools:   e.Tools,
				History: e.History,
				New:     e.New,
			})
		case agentevent.Compacted:
			a.send(event.AgentCompacted{
				BeforeTokens: e.BeforeTokens,
				AfterTokens:  e.AfterTokens,
			})
		case agentevent.Error:
			a.markUnsuccessful()
			a.send(event.AgentError{Err: e.Err})
		case agentevent.AgentEnd:
			a.send(event.AgentDone{Success: !a.isUnsuccessful()})
		}
	}
}

// send writes ev to the consumer events channel. Mirrors the
// foundation's send semantics: high-volume streaming events are dropped
// when the channel is full; control-flow events block for up to 5
// seconds before being logged and discarded. An undrained channel that
// long indicates a stalled consumer — the agent cannot make progress
// regardless.
func (a *Agent) send(ev event.Event) {
	switch ev.(type) {
	case event.AgentToken, event.AgentTurnUsage, event.AgentInputEstimate:
		select {
		case a.consumerEvents <- ev:
		default:
			slog.Warn("kit: dropping streaming event, channel full",
				"type", fmt.Sprintf("%T", ev))
		}
	default:
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case a.consumerEvents <- ev:
		case <-timer.C:
			slog.Error("kit: failed to deliver event, channel full",
				"type", fmt.Sprintf("%T", ev))
		}
	}
}

// markUnsuccessful flips the unsuccess flag. Called by [Agent.Abort] and
// by the translator when it observes [agentevent.Error]. The next
// [event.AgentDone] derived from [agentevent.AgentEnd] will report
// Success=false.
func (a *Agent) markUnsuccessful() {
	atomic.StoreUint32(&a.runUnsuccessful, 1)
}

// resetUnsuccessful clears the unsuccess flag at the start of each new
// run. Called by [Agent.Prompt] and [Agent.PromptWithMessages] before
// the foundation begins emitting events for the new run.
func (a *Agent) resetUnsuccessful() {
	atomic.StoreUint32(&a.runUnsuccessful, 0)
}

// isUnsuccessful reads the unsuccess flag. Called by the translator on
// [agentevent.AgentEnd] to set [event.AgentDone].Success.
func (a *Agent) isUnsuccessful() bool {
	return atomic.LoadUint32(&a.runUnsuccessful) == 1
}
