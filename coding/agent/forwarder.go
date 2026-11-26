package agent

import (
	"context"

	"github.com/latebit-io/nib/coding/event"
	kitevent "github.com/latebit-io/nib/kit/event"
)

// Kit-event forwarder: [Agent.forwardKitEvents] re-publishes the kit
// agent's events on coding's bus. It drops kit's AgentTurnUsage
// (providerProxy emits the authoritative one) and finalizes AgentDone.

// forwardKitEvents drains [Agent.kitSub]'s inbox and forwards each
// event (after coding-specific augmentation or filtering) to the
// agent's bus via [Agent.send].
//
// Closes [Agent.forwardDone] on return so [Agent.Close] can block on
// it after kit.Close closes the subscription inbox — providing
// callers a synchronous "no further publishes to the bus"
// guarantee symmetric to kit.Agent.Close's translatorDone wait.
func (a *Agent) forwardKitEvents() {
	defer close(a.forwardDone)
	for ev := range a.kitSub.Events() {
		switch e := ev.(type) {
		case event.AgentTurnUsage:
			// Drop. providerProxy emits the authoritative AgentTurnUsage
			// directly to the frontend, with per-turn estimates baked in
			// and synchronized with the originating Stream.
			continue
		case event.AgentToolCall, kitevent.AgentToolResult:
			// Drop. The kit now reports tool calls itself; coding sends its
			// own AgentToolCall from the hook, and forwarding both would
			// show every call twice.
			continue
		case event.AgentDone:
			a.mu.Lock()
			unsuccessful := a.runUnsuccessful
			a.running = false
			a.waiting = false
			a.mu.Unlock()
			// Coding fails a run for reasons the loop's end reason cannot
			// see: an AgentError it emitted itself.
			if unsuccessful {
				e.Success = false
				ev = e
			}
			// Stop fires at run completion. Async (own context) so a slow
			// hook never stalls the forwarder — the sole kitSub reader —
			// nor delays the runDone close a follow-up run waits on. Guard
			// avoids spawning a goroutine when no plugin supplies hooks.
			if a.hooks != nil {
				go a.pluginStop(context.Background())
			}
		}
		a.send(ev)
		// Run-boundary signal: close runDone AFTER AgentDone has been
		// fully forwarded (including any Success override) so a
		// follow-up [Agent.RunWithMode] / [Agent.Reply] resume blocked
		// on [Agent.fenceForwarder] unblocks only once this run's tail
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
