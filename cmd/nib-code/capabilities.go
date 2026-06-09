package main

import (
	"context"

	kitcmd "github.com/latebit-io/nib/kit/command"
)

// capabilitiesCommand is the in-TUI counterpart of `--plugins`: it
// displays the wired capability manifest (provider, memory store,
// tools, skills, and commands) inline in the agent pane so the user
// can see what the agent can actually do without leaving the session.
//
// Lives in cmd/nib-code (not kit/command) because the manifest is
// assembled from coding-agent state the composition root owns
// (ag.Tools(), the skill Result). The command captures a thunk so the
// manifest reflects current state at invocation — e.g. after a
// provider hot-swap — rather than a startup snapshot.
type capabilitiesCommand struct {
	def      kitcmd.Definition
	manifest func() string
}

// newCapabilitiesCommand returns the /capabilities handler. manifest
// produces the rendered manifest string on demand; it must be non-nil.
func newCapabilitiesCommand(manifest func() string) kitcmd.HandlerCommand {
	return &capabilitiesCommand{
		def: kitcmd.Definition{
			Name:        "capabilities",
			Aliases:     []string{"plugins"},
			Description: "Show wired capabilities: provider, memory, tools, skills, commands.",
			Source: kitcmd.Source{
				Kind: kitcmd.SourceBuiltin,
				Path: "cmd/nib-code",
			},
		},
		manifest: manifest,
	}
}

// Definition implements [kitcmd.Command].
func (c *capabilitiesCommand) Definition() kitcmd.Definition { return c.def }

// Handle renders the current capability manifest into the agent pane.
func (c *capabilitiesCommand) Handle(_ context.Context, sess kitcmd.Session, _ string) error {
	sess.Display(c.manifest())
	return nil
}
