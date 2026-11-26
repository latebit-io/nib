package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/command"
	"github.com/latebit-io/nib/kit/extension"
	"github.com/latebit-io/nib/kit/memory"
	"github.com/latebit-io/nib/kit/nib"
	"github.com/latebit-io/nib/kit/repl"
)

// session is an interactive session's agent and the blocks behind it.
// Switching an extension replaces the agent and keeps the blocks.
type session struct {
	provider llm.Provider
	switches extension.Switches
	assembly *nib.Assembly
	agent    *kit.Agent
}

// newSession assembles the blocks for root and an agent on them.
func newSession(ctx context.Context, root, prompt string, store memory.Store, provider llm.Provider, switches extension.Switches) (*session, error) {
	assembly, err := assemble(ctx, root, prompt, store, switches)
	if err != nil {
		return nil, err
	}
	agent, err := assembly.Agent(provider)
	if err != nil {
		closeAssembly(assembly)
		return nil, err
	}
	return &session{provider: provider, switches: switches, assembly: assembly, agent: agent}, nil
}

// close stops the agent and the blocks it ran on.
func (s *session) close() {
	s.agent.Close()
	closeAssembly(s.assembly)
}

// runInteractive is the line-based session: the default when nib is
// started without a message. Ctrl-C cancels startCtx until started is
// called; from then on interrupt delivers it to the session.
func runInteractive(ctx, startCtx context.Context, started func(), root string, store memory.Store, switches extension.Switches, interrupt <-chan os.Signal) error {
	provider, err := newProvider(root)
	if err != nil {
		return err
	}
	sessionID := newSessionID(time.Now(), "interactive")
	s, err := newSession(startCtx, root, buildSystemPrompt(sessionID), store, provider, switches)
	if err != nil {
		return setupErr("%v", err)
	}
	defer s.close()
	// A cancelled start can still assemble, with every server failed.
	if startCtx.Err() != nil {
		return setupErr("interrupted during startup")
	}
	started()

	r, err := repl.New(repl.Options{
		Agent: s.agent, SystemPrompt: s.assembly.SystemPrompt,
		In: os.Stdin, Out: os.Stdout, Interrupt: interrupt,
	})
	if err != nil {
		return setupErr("%v", err)
	}
	reg, err := (&extensionCommand{session: s, repl: r}).registry()
	if err != nil {
		return setupErr("command: %v", err)
	}
	r.SetCommands(reg)
	fmt.Fprintln(os.Stderr, "nib: /help lists commands; Ctrl-C stops a run, or leaves when nothing is running.")
	err = r.Run(ctx)
	recordSession(store, sessionID, "(interactive session)", statusSuccess)
	return err
}

// extensionCommand is /extension: it lists the extensions, or switches
// one for the rest of the session and gives the session a new agent.
type extensionCommand struct {
	session *session
	repl    *repl.REPL
}

// Definition implements [command.Command].
func (c *extensionCommand) Definition() command.Definition {
	return command.Definition{
		Name:        "extension",
		Aliases:     []string{"ext"},
		Description: "List extensions, or switch one: /extension on|off <name>.",
		Source:      command.Source{Kind: command.SourceBuiltin, Path: "cmd/nib"},
	}
}

// registry returns the commands that match the session's extensions
// now: its own, and those of every extension that is on. It is built
// fresh each time, since a command cannot be taken out of a registry.
func (c *extensionCommand) registry() (*command.Registry, error) {
	reg := command.NewRegistry()
	commands := append(repl.Commands(c.repl), c, command.NewHelp(reg))
	commands = append(commands, c.session.assembly.Toolset.Commands...)
	for _, cmd := range commands {
		if err := reg.Register(cmd); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

// Handle implements [command.HandlerCommand].
func (c *extensionCommand) Handle(ctx context.Context, sess command.Session, args string) error {
	fields := strings.Fields(args)
	if len(fields) == 0 {
		sess.Display(c.list())
		return nil
	}
	if len(fields) != 2 || (fields[0] != "on" && fields[0] != "off") {
		return fmt.Errorf("usage: /extension on|off <name>")
	}
	state, name := fields[0], fields[1]
	change := extension.Switches{Enable: []string{name}}
	if state == "off" {
		change = extension.Switches{Disable: []string{name}}
	}
	s := c.session
	next := s.switches.Then(change)
	seen := len(s.assembly.Warnings)
	changed, err := s.assembly.Switch(ctx, next)
	if err != nil {
		return err
	}
	printWarnings(s.assembly.Warnings[seen:])
	if !changed {
		s.switches = next
		sess.Display(fmt.Sprintf("%s is already %s.", name, state))
		return nil
	}
	// Checked before anything else changes: a command that clashes with
	// one the session has must undo the switch, not half apply it.
	reg, err := c.registry()
	if err != nil {
		if _, undoErr := s.assembly.Switch(ctx, s.switches); undoErr != nil {
			err = errors.Join(err, undoErr)
		}
		return fmt.Errorf("%s stays as it was: %w", name, err)
	}
	// The old agent still holds the tools as they were: replace it.
	agent, err := s.assembly.Agent(s.provider)
	if err == nil {
		err = c.repl.Swap(agent, s.assembly.SystemPrompt)
	}
	if err != nil {
		// No agent matches the blocks any more; the session cannot go on.
		c.repl.Quit()
		if agent != nil {
			agent.Close()
		}
		return err
	}
	s.agent.Close()
	s.agent, s.switches = agent, next
	c.repl.SetCommands(reg)
	sess.Display(fmt.Sprintf("%s is now %s. The conversation continues.", name, state))
	return nil
}

// list renders the extensions and whether each is on.
func (c *extensionCommand) list() string {
	states := c.session.assembly.Extensions()
	if len(states) == 0 {
		return "No extensions are available."
	}
	lines := make([]string, 0, len(states))
	for _, e := range states {
		state := "off"
		if e.On {
			state = "on "
		}
		lines = append(lines, fmt.Sprintf("  %s  %s  %s", state, e.Name, e.Description))
	}
	return strings.Join(lines, "\n")
}

// newProvider resolves the model provider for root, or says which
// credentials are missing.
func newProvider(root string) (llm.Provider, error) {
	resolved, err := nib.ResolveProvider(root)
	if err != nil {
		return nil, setupErr("oauth store: %v", err)
	}
	if !resolved.HasProvider() {
		return nil, setupErr("no LLM credentials — %s", credentialHint(resolved))
	}
	provider := resolved.NewProvider()
	if provider == nil {
		return nil, setupErr("provider construction failed for profile %q", resolved.Profile)
	}
	slog.Debug("provider resolved", "profile", resolved.Profile, "model", resolved.Model)
	return provider, nil
}
