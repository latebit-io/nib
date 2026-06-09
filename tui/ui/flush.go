package ui

import (
	"context"

	tea "charm.land/bubbletea/v2"
)

// FlushDirtyBuffersMsg requests an autosave round-trip from the agent's
// pre-tool-dispatch flush step. The handler in [AppModel.Update] calls
// [session.Session.SaveDirtyBuffers] synchronously on the Bubble Tea
// Update goroutine — the same goroutine that mutates buffers via
// keystroke handlers — and writes the outcome to Result.
//
// Routing through this message (rather than calling SaveDirtyBuffers
// directly from the agent's tool-dispatch goroutine) preserves the
// load-bearing invariant that all [engine/buffer.Buffer] mutations
// happen on the Update goroutine. [engine/buffer.Buffer] holds no
// internal lock; the goroutine-affinity rule is how concurrent
// keystroke + agent-edit access stays race-free.
//
// Result MUST be a buffered channel with capacity ≥ 1 — the handler
// runs on the Update goroutine and must never block. The sender
// (typically [tui.App.FlushDirtyBuffers]) owns the channel and is
// responsible for receiving from it (or letting it be GC'd if the
// caller's ctx fires first).
type FlushDirtyBuffersMsg struct {
	// Result receives the flush outcome.
	Result chan<- FlushResult
}

// FlushResult carries the outcome of a [FlushDirtyBuffersMsg] dispatch:
// the canonical paths of saved files and the first error (nil on full
// success).
type FlushResult struct {
	// Saved lists the canonical paths of files that were saved.
	Saved []string
	// Err is the first error encountered (nil on full success).
	Err error
}

// handleFlushDirtyBuffers runs on the Update goroutine in response to a
// [FlushDirtyBuffersMsg]. Calls [session.Session.SaveDirtyBuffers] and
// signals the result. Returns no follow-up [tea.Cmd] — autosave has no
// Bubble Tea side effects beyond the disk write.
//
// The handler uses [context.Background] internally; the sender's
// cancellation contract is enforced at its receive end in
// [tui.App.FlushDirtyBuffers] (sender returns ctx.Err and stops
// listening). The non-blocking send below ensures Update never wedges
// on a sender that has already given up.
func (m *AppModel) handleFlushDirtyBuffers(msg FlushDirtyBuffersMsg) tea.Cmd {
	saved, err := m.Session.SaveDirtyBuffers(context.Background())
	select {
	case msg.Result <- FlushResult{Saved: saved, Err: err}:
	default:
	}
	return nil
}
