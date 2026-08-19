package agent

import (
	"context"

	"github.com/latebit-io/nib/coding/event"
)

// Kit-event forwarder.
//
// [Agent.forwardKitEvents] drains the kit agent's translated event
// stream from [Agent.kitSub] and re-publishes each event on the agent's
// bus. Two events get coding-specific treatment:
//
//   - [event.AgentTurnUsage] is filtered out. [providerProxy] emits
//     the authoritative AgentTurnUsage (with per-turn estimates and
//     completion estimate) directly to the frontend, synchronously
//     in its Stream wrapper goroutine. Forwarding kit's would
//     duplicate cost-panel ticks and worse: kit's AgentTurnUsage
//     drops on full consumer channels, so any forwarder-side
//     bookkeeping that depended on its delivery would desync.
//   - [event.AgentDone] is intercepted to flip [Agent.running] off
//     and to override Success with [Agent.runUnsuccessful] — kit's
//     per-run outcome only knows about Cancels and foundation Errors,
//     so coding-side AgentErrors emitted via [Agent.send] (autosave
//     failure, RunWithMode rejection) need this wrapper-level flag
//     to surface as Success=false.
//
// The forwarder also flips [Agent.waiting] off when AgentDone fires
// — the BeforePark hook flips it on (synchronously on the foundation
// goroutine, just before [event.AgentParked] is emitted); AgentDone
// resets it.
//
// Lifecycle: spawned once in [Agent.buildKitAgent]; exits when
// [kit.Agent.Close] closes [Agent.kitSub]'s inbox after the kit
// translator has fully exited.

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
		case event.AgentDone:
			a.mu.Lock()
			unsuccessful := a.runUnsuccessful
			a.running = false
			a.waiting = false
			a.mu.Unlock()
			if unsuccessful && e.Success {
				ev = event.AgentDone{Success: false}
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
