package command

import (
	"context"

	kitcmd "github.com/latebit-io/nib/kit/command"
)

// QuitCommand is the TUI's /quit handler. It calls the captured
// QuitFn to terminate the process; the binary supplies the
// concrete shutdown mechanism (typically program.Quit() or a
// context cancel). QuitCommand has no opinion on how the process
// dies — that's a frontend choice, not a kit concern.
type QuitCommand struct {
	def    kitcmd.Definition
	quitFn func()
}

// NewQuit returns a QuitCommand whose Handle invokes quitFn.
// quitFn must be non-nil; a nil callback is a programmer error
// and would silently no-op /quit, leaving the user trapped in
// the TUI. New panics in that case to fail fast at construction.
func NewQuit(quitFn func()) *QuitCommand {
	if quitFn == nil {
		panic("tui/command: NewQuit requires a non-nil quitFn")
	}
	return &QuitCommand{
		def: kitcmd.Definition{
			Name:        "quit",
			Aliases:     []string{"exit", "q"},
			Description: "Exit the application.",
			Source: kitcmd.Source{
				Kind: kitcmd.SourceBuiltin,
				Path: "tui/command",
			},
		},
		quitFn: quitFn,
	}
}

// Definition exposes the command's name, aliases, description, and
// source for /help and registry diagnostics.
func (q *QuitCommand) Definition() kitcmd.Definition { return q.def }

// Handle invokes the captured quitFn and returns nil. /quit
// ignores any args — extra text after the command name is
// discarded silently rather than failing, matching the principle
// that exiting should never require additional ceremony.
func (q *QuitCommand) Handle(_ context.Context, _ kitcmd.Session, _ string) error {
	q.quitFn()
	return nil
}

// Compile-time check that QuitCommand satisfies HandlerCommand.
var _ kitcmd.HandlerCommand = (*QuitCommand)(nil)
