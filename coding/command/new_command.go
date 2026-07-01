package command

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	kitcmd "github.com/latebit-io/nib/kit/command"
)

// scaffoldTemplate is the body written to a freshly created
// markdown command file. The frontmatter is intentionally minimal:
// a TODO description, no aliases. The body is a one-liner that
// hints at the substitution syntax so the user can fill it in
// without consulting external docs. Keep this short — long
// scaffolds invite cargo-cult editing where users leave boilerplate
// they didn't actually need.
const scaffoldTemplate = `---
description: TODO — replace with a one-line description
aliases: []
---
TODO — write the prompt body. Substitution syntax: $1, $2, ... for positional args, $@ for all args joined, $ARGUMENTS for the raw input string.
`

// CommandLookup is the minimum surface NewCommandCommand needs from
// the registry: "is this name already taken?" Pulled out as an
// interface so tests can use a fake registry without constructing
// the full kit/command.Registry. *kit/command.Registry satisfies
// it via its Lookup method.
type CommandLookup interface {
	// Lookup resolves a name (or alias) to its registered command,
	// returning ok=false when nothing matches.
	Lookup(name string) (kitcmd.Command, bool)
}

// NewCommandCommand is the /new-command HandlerCommand. It scaffolds
// a new markdown PromptCommand under <projectRoot>/.project/commands/
// so users can author commands without leaving the TUI.
//
// What it does NOT do: register the new command into the live
// registry. The scaffold body is a TODO placeholder; auto-registering
// would expose a half-written prompt to the LLM on the next /name
// invocation. The user edits the file and restarts to pick it up.
// A future hot-reload path would lift this restriction.
//
// Lives in coding/command (not kit/command) because it touches a
// project-specific filesystem layout (.project/commands/) that the
// kit framework deliberately knows nothing about — kit owns the
// loader API, consumers own where their commands live.
type NewCommandCommand struct {
	def        kitcmd.Definition
	commandDir string
	lookup     CommandLookup
}

// NewNewCommand returns a NewCommandCommand. commandDir is the
// directory where new files are written — typically
// <projectRoot>/.project/commands/. lookup is consulted to refuse
// names that would shadow an existing registered command; a nil
// lookup disables that check (acceptable in tests, rejected at the
// real composition site).
//
// Both arguments are required for the production wiring. nil
// commandDir would silently route writes to the binary's CWD; nil
// lookup would let the user create commands that override built-ins
// without warning. The constructor panics on either to fail fast at
// startup rather than at first dispatch.
func NewNewCommand(commandDir string, lookup CommandLookup) *NewCommandCommand {
	if commandDir == "" {
		panic("coding/command: NewNewCommand requires a non-empty commandDir")
	}
	if lookup == nil {
		panic("coding/command: NewNewCommand requires a non-nil CommandLookup")
	}
	return &NewCommandCommand{
		def: kitcmd.Definition{
			Name:        "new-command",
			Aliases:     []string{"newcmd"},
			Description: "Scaffold a new markdown slash command in this project.",
			Source: kitcmd.Source{
				Kind: kitcmd.SourceBuiltin,
				Path: "coding/command",
			},
		},
		commandDir: commandDir,
		lookup:     lookup,
	}
}

// Definition exposes the command's name, aliases, description, and
// source for /help and registry diagnostics.
func (c *NewCommandCommand) Definition() kitcmd.Definition { return c.def }

// Handle creates the scaffold file and surfaces the next-step
// instruction via Display. Validation order:
//
//  1. Name argument required.
//  2. Name must satisfy [kit/command.ValidName].
//  3. Name must not already be registered (would shadow).
//  4. Target file must not already exist (refuse to overwrite —
//     destroying user-edited content silently is the worst outcome
//     this command could produce).
//
// Each validation failure returns a [CommandError]-shaped error so
// the dispatcher renders it cleanly. Successful creation writes the
// scaffold and tells the user to edit and restart.
func (c *NewCommandCommand) Handle(_ context.Context, sess kitcmd.Session, args string) error {
	name := strings.ToLower(strings.TrimSpace(args))
	// Reject extra positional args. The first whitespace-delimited
	// token is the name; anything after is a likely user error
	// (e.g. typing a description inline).
	if i := strings.IndexAny(name, " \t"); i >= 0 {
		return fmt.Errorf("/new-command takes one argument (the name); got %q. Add description and aliases by editing the scaffold file", args)
	}
	if name == "" {
		return errors.New("/new-command requires a name; e.g. /new-command review")
	}
	if !kitcmd.ValidName(name) {
		return fmt.Errorf("invalid command name %q (must match [a-zA-Z0-9_-]+)", name)
	}
	if existing, ok := c.lookup.Lookup(name); ok {
		def := existing.Definition()
		return fmt.Errorf("command %q is already registered (%s, %s)", name, sourceKindLabel(def.Source.Kind), def.Source.Path)
	}

	// Create the directory if missing — first-time use of
	// /new-command in a project that opted out of auto-seed (the
	// user mkdir'd .project/commands earlier and emptied it) still
	// works. mkdir on an existing directory is a no-op.
	if err := os.MkdirAll(c.commandDir, 0750); err != nil {
		return fmt.Errorf("create commands dir %s: %w", c.commandDir, err)
	}

	target := filepath.Join(c.commandDir, name+".md")
	// Refuse to overwrite. O_EXCL means "fail if file exists" —
	// atomic check-and-create that doesn't race a concurrent
	// editor write.
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("file already exists at %s — edit it directly or rename and try again", target)
		}
		return fmt.Errorf("create %s: %w", target, err)
	}
	if _, writeErr := f.Write([]byte(scaffoldTemplate)); writeErr != nil {
		_ = f.Close() // best-effort close; the write error below is the real failure
		return fmt.Errorf("write %s: %w", target, writeErr)
	}
	if closeErr := f.Close(); closeErr != nil {
		return fmt.Errorf("close %s: %w", target, closeErr)
	}

	sess.Display(fmt.Sprintf(
		"Created %s. Edit it to define the prompt, then restart nib-code to register the /%s command.",
		target, name,
	))
	return nil
}

// sourceKindLabel renders a [kit/command.SourceKind] as a short
// human-readable label for diagnostic messages. Kept local to this
// file; if a second caller emerges, promote to kit/command.
func sourceKindLabel(k kitcmd.SourceKind) string {
	switch k {
	case kitcmd.SourceProject:
		return "project"
	case kitcmd.SourceGlobal:
		return "global"
	case kitcmd.SourceMCP:
		return "mcp"
	case kitcmd.SourceBuiltin:
		return "builtin"
	default:
		return "unknown"
	}
}

// Compile-time check that NewCommandCommand satisfies HandlerCommand.
var _ kitcmd.HandlerCommand = (*NewCommandCommand)(nil)
