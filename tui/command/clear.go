package command

import (
	"context"

	kitcmd "github.com/latebit-io/nib/kit/command"
)

// HistoryResetter is the minimum surface ClearCommand needs from the
// agent. *coding/agent.Agent satisfies it via its ResetHistory method.
// The interface lives here (not coding/agent) because the consumer —
// /clear — owns the contract and the test fakes that drive it.
type HistoryResetter interface {
	// ResetHistory drops the saved conversation transcript so the
	// next user message starts a fresh run. Implementations are
	// expected to cancel any active run and clear domain-side
	// per-conversation state (intent, pending lint, etc.).
	ResetHistory(ctx context.Context) error
}

// ClearCommand is the TUI's /clear handler. Resets the agent's
// conversation history AND the agent pane's transcript view, in that
// order: cancelling the run synchronously means any
// transcript-touching events (AgentDone, status updates) settle
// before the pane is wiped. Clearing the pane first would race
// inbound events that arrive between Clear and ResetHistory.
//
// Lives in tui/command (not kit/command) because it bridges two
// frontend-shaped concerns — a coding-domain history reset and a
// TUI-specific view clear — that don't generalize to non-TUI
// consumers (a server frontend has no transcript to clear; a
// research agent has no history to reset).
type ClearCommand struct {
	def      kitcmd.Definition
	resetter HistoryResetter
	pane     Pane
}

// NewClear returns a ClearCommand. Both resetter and pane must be
// non-nil; nil arguments would silently no-op /clear, leaving stale
// transcript state visible to the user. Constructor panics in that
// case to fail fast at registry-build time.
func NewClear(resetter HistoryResetter, pane Pane) *ClearCommand {
	if resetter == nil {
		panic("tui/command: NewClear requires a non-nil HistoryResetter")
	}
	if pane == nil {
		panic("tui/command: NewClear requires a non-nil Pane")
	}
	return &ClearCommand{
		def: kitcmd.Definition{
			Name:        "clear",
			Description: "Clear the conversation and reset the agent's history.",
			Source: kitcmd.Source{
				Kind: kitcmd.SourceBuiltin,
				Path: "tui/command",
			},
		},
		resetter: resetter,
		pane:     pane,
	}
}

// Definition exposes the command's name, aliases, description, and
// source for /help and registry diagnostics.
func (c *ClearCommand) Definition() kitcmd.Definition { return c.def }

// Handle resets the agent history first, then clears the pane. If
// ResetHistory fails, the pane is NOT cleared — surfacing the error
// against the existing transcript is more useful than leaving the
// user with a blank pane and no signal of what went wrong.
func (c *ClearCommand) Handle(ctx context.Context, _ kitcmd.Session, _ string) error {
	if err := c.resetter.ResetHistory(ctx); err != nil {
		return err
	}
	c.pane.Clear()
	return nil
}

// Compile-time check that ClearCommand satisfies HandlerCommand.
var _ kitcmd.HandlerCommand = (*ClearCommand)(nil)
