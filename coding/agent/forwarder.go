package agent

import (
	"github.com/latebit-io/nib/coding/event"
)

// Kit-event forwarder.
//
// [Agent.forwardKitEvents] drains the kit agent's translated event
// stream from [Agent.kitEvents] and re-emits each event to the
// frontend channel. Two events get coding-specific treatment:
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
// — AgentWaiting from [GetFollowUpMessages] sets it on; AgentDone
// resets it.
//
// Lifecycle: spawned once in [Agent.buildKitAgent]; exits when
// [Agent.Close] closes [Agent.kitEvents] after kit.Close has waited
// for the foundation to unwind.

// forwardKitEvents drains [Agent.kitEvents] and forwards each event
// (after coding-specific augmentation or filtering) to the frontend
// events channel via [Agent.send].
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
