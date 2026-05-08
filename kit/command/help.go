package command

import (
	"context"
	"fmt"
	"strings"
)

// helpCommand is kit's only built-in [HandlerCommand]. It lists
// every command in the registry it was constructed against,
// sorted by canonical name, including each command's source so
// the user can tell built-in apart from project-local.
//
// Convention: register helpCommand last during composition, so
// /help sees every other command. Registering /help first works
// — it holds a registry pointer, not a snapshot — but reading
// the binary's main.go is easier when /help is unambiguously
// last.
type helpCommand struct {
	reg *Registry
	def Definition
}

// NewHelp returns the /help built-in bound to the given registry.
// /help is the only command kit ships unconditionally; everything
// else (including /clear and /quit) lives in the consumer that
// owns its semantics.
func NewHelp(reg *Registry) HandlerCommand {
	return &helpCommand{
		reg: reg,
		def: Definition{
			Name:        "help",
			Description: "List available commands.",
			Source: Source{
				Kind: SourceBuiltin,
				Path: "kit/command",
			},
		},
	}
}

// Definition implements [Command].
func (h *helpCommand) Definition() Definition { return h.def }

// Handle renders one line per registered command via
// [Session.Display]. Output format: two-space indent, slash-name
// padded to a fixed width, description, and a trailing source tag
// for non-builtin commands. /help itself is included so users
// know it is registered.
func (h *helpCommand) Handle(_ context.Context, sess Session, _ string) error {
	cmds := h.reg.List()
	var b strings.Builder
	b.WriteString("Available commands:\n")
	if len(cmds) == 0 {
		b.WriteString("  (none registered)\n")
		sess.Display(b.String())
		return nil
	}

	const namePadWidth = 12
	for _, c := range cmds {
		d := c.Definition()
		nameField := "/" + d.Name
		pad := ""
		if l := len(nameField); l < namePadWidth {
			pad = strings.Repeat(" ", namePadWidth-l)
		}
		fmt.Fprintf(&b, "  %s%s  %s", nameField, pad, d.Description)
		if label := sourceLabel(d.Source.Kind); label != "" {
			fmt.Fprintf(&b, "  [%s]", label)
		}
		b.WriteByte('\n')
	}
	sess.Display(b.String())
	return nil
}

// sourceLabel returns the short tag /help renders next to
// non-builtin commands. Builtins render with no tag — they are
// the implicit baseline.
func sourceLabel(k SourceKind) string {
	switch k {
	case SourceBuiltin:
		return ""
	case SourceMCP:
		return "mcp"
	case SourceGlobal:
		return "global"
	case SourceProject:
		return "project"
	default:
		return "?"
	}
}
