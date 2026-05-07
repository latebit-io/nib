// Package command holds the coding-domain slash-command
// implementations. The framework — Registry, Definition, parser,
// /help — lives in kit/command; this package contributes commands
// that depend on coding's domain primitives (compaction, history,
// agent state) and therefore cannot live in kit without inverting
// the layering.
//
// Layering: coding/command depends on kit/command (for the interfaces
// and Session types it implements) and on coding/agent (for the
// Compactor surface this command consumes). It must not import tui/
// — frontend-shaped commands live in tui/command.
package command

import (
	"context"
	"errors"

	kitcmd "github.com/latebit-io/nib/kit/command"
)

// Compactor is the minimum surface CompactCommand needs from the
// agent. Exists so tests can drive the command without constructing
// a full *coding/agent.Agent. *coding/agent.Agent satisfies it via
// its Compact method.
type Compactor interface {
	// Compact runs an out-of-band compaction pass over the agent's
	// saved transcript. Implementations should emit AgentCompacted on
	// success and return a non-nil error (typed for "nothing to
	// compact" / "no conversation") otherwise.
	Compact(ctx context.Context) error
}

// CompactCommand is the coding-domain /compact handler. It triggers
// an immediate compaction pass on the active conversation. Lives in
// coding/command (not kit/command) because it depends on coding's
// compaction primitives — kit owns the framework, consumers contribute
// their domain commands.
type CompactCommand struct {
	def       kitcmd.Definition
	compactor Compactor

	// nothingToCompact and noConversation are matched against the
	// compactor's returned error to render appropriate user-facing
	// chrome instead of a generic "command failed" line. Captured at
	// construction so the caller (typically wired to
	// coding/agent.ErrNothingToCompact and ErrNoConversation) controls
	// the contract surface.
	nothingToCompact error
	noConversation   error
}

// NewCompact returns a CompactCommand. compactor must be non-nil; a
// nil compactor would silently no-op /compact, hiding the
// misconfiguration from the user. Constructor panics in that case to
// fail fast at registry-build time.
//
// nothingToCompact and noConversation are the sentinel errors the
// compactor returns for the two informational outcomes. Pass nil to
// route those cases through the generic error path. The standard
// wiring threads coding/agent.ErrNothingToCompact and
// coding/agent.ErrNoConversation through these parameters.
func NewCompact(compactor Compactor, nothingToCompact, noConversation error) *CompactCommand {
	if compactor == nil {
		panic("coding/command: NewCompact requires a non-nil compactor")
	}
	return &CompactCommand{
		def: kitcmd.Definition{
			Name:        "compact",
			Description: "Compact the conversation history to reduce token usage.",
			Source: kitcmd.Source{
				Kind: kitcmd.SourceBuiltin,
				Path: "coding/command",
			},
		},
		compactor:        compactor,
		nothingToCompact: nothingToCompact,
		noConversation:   noConversation,
	}
}

// Definition exposes the command's name, aliases, description, and
// source for /help and registry diagnostics.
func (c *CompactCommand) Definition() kitcmd.Definition { return c.def }

// Handle invokes the captured compactor. On success, surfaces a
// "Conversation compacted." line via Display (the actual before/after
// numbers arrive separately as an AgentCompacted event the frontend
// renders inline). The two informational errors are routed to Display
// instead of returning, so the user sees a readable line rather than
// a generic failure chrome from the registry.
func (c *CompactCommand) Handle(ctx context.Context, sess kitcmd.Session, _ string) error {
	err := c.compactor.Compact(ctx)
	switch {
	case err == nil:
		sess.Display("Conversation compacted.")
		return nil
	case c.nothingToCompact != nil && errors.Is(err, c.nothingToCompact):
		sess.Display("Nothing to compact.")
		return nil
	case c.noConversation != nil && errors.Is(err, c.noConversation):
		sess.Display("No conversation to compact.")
		return nil
	default:
		return err
	}
}

// Compile-time check that CompactCommand satisfies HandlerCommand.
var _ kitcmd.HandlerCommand = (*CompactCommand)(nil)
