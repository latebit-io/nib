package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/ai/llmconfig"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/command"
	"github.com/latebit-io/nib/kit/extension"
	"github.com/latebit-io/nib/kit/hookspec"
	"github.com/latebit-io/nib/kit/memory"
	"github.com/latebit-io/nib/kit/nib"
	"github.com/latebit-io/nib/kit/permission"
	"github.com/latebit-io/nib/kit/repl"
)

// session is an interactive session's agent and the blocks behind it.
// Switching an extension replaces the agent and keeps the blocks.
type session struct {
	resolved *llmconfig.Resolved // the model profile; /model changes its model
	provider llm.Provider
	switches extension.Switches
	assembly *nib.Assembly
	agent    *kit.Agent
}

// newSession assembles the blocks for root and an agent on them.
func newSession(ctx context.Context, root, prompt string, store memory.Store, resolved *llmconfig.Resolved, provider llm.Provider, chosen choices) (*session, error) {
	s := &session{resolved: resolved, provider: provider, switches: chosen.switches}
	// A subagent's model follows /model: the profile is read when it starts.
	chosen.providerFor = providerFor(func() *llmconfig.Resolved { return s.resolved })
	assembly, err := assemble(ctx, root, prompt, store, chosen)
	if err != nil {
		return nil, err
	}
	assembly.StartSession(ctx, "startup")
	agent, err := assembly.Agent(provider)
	if err != nil {
		closeAssembly(assembly)
		return nil, err
	}
	s.assembly, s.agent = assembly, agent
	return s, nil
}

// attach makes r the session's frontend: asked by the permission gate
// and the MCP servers, given the commands, and rebuilt when a server's
// tools change. It starts in the approve mode when the person chose it.
func (s *session) attach(r *repl.REPL, approve bool) error {
	s.assembly.Permission.SetAsker(r.Ask)
	s.assembly.SetAsker(r.Elicit)
	if approve {
		s.assembly.Permission.SetMode(permission.Approve)
	}
	ext := &extensionCommand{session: s, repl: r}
	reg, err := ext.registry()
	if err != nil {
		return fmt.Errorf("command: %w", err)
	}
	r.SetCommands(reg)
	// One rebuild waits at a time; a burst of changes is followed once.
	var waiting atomic.Bool
	s.assembly.WatchServers(func() {
		if waiting.CompareAndSwap(false, true) {
			r.Post(func() {
				waiting.Store(false)
				ext.serversChanged()
			})
		}
	})
	return nil
}

// close stops the agent and the blocks it ran on.
func (s *session) close() {
	s.agent.Close()
	s.assembly.Hooks.SessionEnd(context.Background(), "prompt_input_exit")
	closeAssembly(s.assembly)
}

// runInteractive is the line-based session: the default when nib is
// started without a message. Ctrl-C cancels startCtx until started is
// called; from then on interrupt delivers it to the session.
func runInteractive(ctx, startCtx context.Context, started func(), root string, store memory.Store, chosen choices, interrupt <-chan os.Signal) error {
	resolved, provider, err := newProvider(root)
	if err != nil {
		return err
	}
	sessionID := newSessionID(time.Now(), "interactive")
	chosen.session = sessionID
	s, err := newSession(startCtx, root, buildSystemPrompt(sessionID), store, resolved, provider, chosen)
	if err != nil {
		return setupErr("%v", err)
	}
	defer s.close()
	// A cancelled start can still assemble, with every server failed.
	if startCtx.Err() != nil {
		return setupErr("interrupted during startup")
	}
	started()

	opts := repl.Options{
		Agent: s.agent, SystemPrompt: s.assembly.SystemPrompt,
		In: os.Stdin, Out: os.Stdout, Interrupt: interrupt,
	}
	// Only where a plugin listens: each costs work at every prompt or answer.
	if hooks := s.assembly.Hooks; hooks.Has(hookspec.UserPromptSubmit) {
		opts.OnPrompt = hooks.Prompt
	}
	if hooks := s.assembly.Hooks; hooks.Has(hookspec.Stop) {
		opts.OnStop = hooks.Stop
	}
	r, err := repl.New(opts)
	if err != nil {
		return setupErr("%v", err)
	}
	if err := s.attach(r, chosen.approve); err != nil {
		return setupErr("%v", err)
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
	def := builtin("extension", "List extensions, or switch one: /extension on|off <name>.")
	def.Aliases = []string{"ext"}
	return def
}

// registry returns the commands that match the session's extensions
// now: its own, and those of every extension that is on. It is built
// fresh each time, since a command cannot be taken out of a registry.
func (c *extensionCommand) registry() (*command.Registry, error) {
	reg := command.NewRegistry()
	s := c.session
	commands := append(repl.Commands(c.repl), c,
		handler{builtin("approve", "Ask before commands and file changes: /approve on|off."), s.approve},
		handler{builtin("model", "Show the model, or switch to another: /model <name>."), s.model},
		handler{builtin("usage", "Show the tokens this session has used."), s.usage},
		command.NewHelp(reg))
	commands = append(commands, s.assembly.Toolset.Commands...)
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
	if err := c.replaceAgent(reg); err != nil {
		return err
	}
	s.switches = next
	sess.Display(fmt.Sprintf("%s is now %s. The conversation continues.", name, state))
	return nil
}

// replaceAgent gives the session an agent built from the blocks as they
// are now, and the commands in reg; the conversation carries over. The
// old agent still holds the tools as they were.
func (c *extensionCommand) replaceAgent(reg *command.Registry) error {
	s := c.session
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
	c.repl.SetCommands(reg)
	return nil
}

// serversChanged follows an MCP server whose tools or prompts changed.
// It runs in the session's loop while the agent is not working.
func (c *extensionCommand) serversChanged() {
	c.session.assembly.Refresh()
	reg, err := c.registry()
	if err == nil {
		err = c.replaceAgent(reg)
	}
	if err != nil {
		c.repl.Display("error: an MCP server changed its tools and the session could not follow: " + err.Error())
		return
	}
	c.repl.Display("(an MCP server changed its tools or prompts; the conversation continues)")
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

// handler is a slash command that is one function on the session.
type handler struct {
	def command.Definition
	run func(sess command.Session, args string) error
}

// Definition implements [command.Command].
func (h handler) Definition() command.Definition { return h.def }

// Handle implements [command.HandlerCommand].
func (h handler) Handle(_ context.Context, sess command.Session, args string) error {
	return h.run(sess, args)
}

// builtin describes one of nib's own commands.
func builtin(name, description string) command.Definition {
	return command.Definition{
		Name: name, Description: description,
		Source: command.Source{Kind: command.SourceBuiltin, Path: "cmd/nib"},
	}
}

// approve is /approve: it shows the permission mode, or switches it
// for the rest of the session.
func (s *session) approve(sess command.Session, args string) error {
	gate := s.assembly.Permission
	switch strings.TrimSpace(args) {
	case "on":
		gate.SetMode(permission.Approve)
	case "off":
		gate.SetMode(permission.Autonomous)
	case "":
	default:
		return errors.New("usage: /approve on|off")
	}
	if gate.Mode() == permission.Approve {
		sess.Display("Approve mode: nib asks before each call that may change something.")
		return nil
	}
	sess.Display("Autonomous mode: nib acts without asking. The shell still refuses risky commands.")
	return nil
}

// model is /model: it shows the model in use, or switches to another
// model of the same provider from the next request on.
func (s *session) model(sess command.Session, args string) error {
	name := strings.TrimSpace(args)
	if name == "" {
		sess.Display("Model: " + s.resolved.DisplayModel())
		return nil
	}
	// A copy: a model the provider cannot build must leave the session as it is.
	next := *s.resolved
	next.Model = name
	provider := next.NewProvider()
	if provider == nil {
		return fmt.Errorf("no provider could be built for model %q", name)
	}
	s.assembly.SetProvider(provider)
	s.resolved, s.provider = &next, provider
	sess.Display("Model is now " + next.DisplayModel() + ". The conversation continues.")
	return nil
}

// usage is /usage: the tokens the session has used.
func (s *session) usage(sess command.Session, _ string) error {
	u := s.assembly.Usage()
	sess.Display(fmt.Sprintf("%d requests: %d input, %d cached, %d cache-write, %d output tokens.",
		u.Turns, u.TotalPromptTokens, u.TotalCachedTokens, u.TotalCacheWriteTokens, u.TotalCompletionTokens))
	return nil
}

// newProvider resolves the model profile for root and builds its
// provider, or says which credentials are missing.
func newProvider(root string) (*llmconfig.Resolved, llm.Provider, error) {
	resolved, err := nib.ResolveProvider(root)
	if err != nil {
		return nil, nil, setupErr("oauth store: %v", err)
	}
	if err := resolved.Validate(); err != nil {
		return nil, nil, setupErr("%v", err)
	}
	if !resolved.HasProvider() {
		return nil, nil, setupErr("no LLM credentials — %s", credentialHint(resolved))
	}
	provider := resolved.NewProvider()
	if provider == nil {
		return nil, nil, setupErr("provider construction failed for profile %q", resolved.Profile)
	}
	slog.Debug("provider resolved", "profile", resolved.Profile, "model", resolved.Model)
	return resolved, provider, nil
}
