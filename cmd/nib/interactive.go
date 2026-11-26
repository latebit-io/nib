package main

import (
	"context"
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
// started without a message. interrupt delivers Ctrl-C.
func runInteractive(ctx context.Context, root string, store memory.Store, switches extension.Switches, interrupt <-chan os.Signal) error {
	provider, err := newProvider(root)
	if err != nil {
		return err
	}
	sessionID := newSessionID(time.Now(), "interactive")
	s, err := newSession(ctx, root, buildSystemPrompt(sessionID), store, provider, switches)
	if err != nil {
		return setupErr("%v", err)
	}
	defer s.close()

	reg := command.NewRegistry()
	r, err := repl.New(repl.Options{
		Agent: s.agent, SystemPrompt: s.assembly.SystemPrompt,
		In: os.Stdin, Out: os.Stdout, Commands: reg, Interrupt: interrupt,
	})
	if err != nil {
		return setupErr("%v", err)
	}
	commands := append(repl.Commands(r), &extensionCommand{session: s, repl: r}, command.NewHelp(reg))
	// Commands the extensions on at start bring with them.
	commands = append(commands, s.assembly.Toolset.Commands...)
	for _, c := range commands {
		if err := reg.Register(c); err != nil {
			return setupErr("command: %v", err)
		}
	}
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
	s.switches = next
	printWarnings(s.assembly.Warnings[seen:])
	if !changed {
		sess.Display(fmt.Sprintf("%s is already %s.", name, state))
		return nil
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
	s.agent = agent
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
