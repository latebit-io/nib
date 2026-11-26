package repl

import (
	"context"

	"github.com/latebit-io/nib/kit/command"
)

// builtin is a command whose handler is a function on the session.
type builtin struct {
	def command.Definition
	run func()
}

func (b builtin) Definition() command.Definition { return b.def }

func (b builtin) Handle(context.Context, command.Session, string) error {
	b.run()
	return nil
}

// Commands returns the session's own commands, /clear and /quit, for
// the host to register beside [command.NewHelp] and its own.
func Commands(r *REPL) []command.Command {
	source := command.Source{Kind: command.SourceBuiltin, Path: "kit/repl"}
	return []command.Command{
		builtin{
			def: command.Definition{Name: "clear", Description: "Forget the conversation and start over.", Source: source},
			run: r.Clear,
		},
		builtin{
			def: command.Definition{Name: "quit", Aliases: []string{"exit"}, Description: "End the session.", Source: source},
			run: r.Quit,
		},
	}
}
