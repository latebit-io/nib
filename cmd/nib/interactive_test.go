package main

import (
	"context"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/ai/llmtest"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/command"
	"github.com/latebit-io/nib/kit/extension"
	"github.com/latebit-io/nib/kit/nib"
	"github.com/latebit-io/nib/kit/repl"
)

// screen is the session's output, safe to read while it is written.
type screen struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *screen) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *screen) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func toolNames(tools []llm.ToolDef) string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Function.Name)
	}
	return strings.Join(names, ",")
}

// TestExtensionCommand_SwitchesTools: /extension off removes an
// extension's tools from the next request and keeps the conversation.
func TestExtensionCommand_SwitchesTools(t *testing.T) {
	// Keep the person's own config, trust store and skills out of it.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("NIB_GLOBAL_SKILLS_DIR", t.TempDir())
	t.Setenv("NIB_MCP", "")

	ctx := context.Background()
	provider := llmtest.NewScripted(llmtest.Text("one"), llmtest.Text("two"))
	s, err := newSession(ctx, t.TempDir(), "base", nil, provider, extension.Switches{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	in, keys := io.Pipe()
	out := &screen{}
	r, err := repl.New(repl.Options{Agent: s.agent, SystemPrompt: s.assembly.SystemPrompt, In: in, Out: out})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := (&extensionCommand{session: s, repl: r}).registry()
	if err != nil {
		t.Fatal(err)
	}
	r.SetCommands(reg)
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	say := func(line, want string) {
		t.Helper()
		if _, err := io.WriteString(keys, line+"\n"); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for !strings.Contains(out.String(), want) {
			if time.Now().After(deadline) {
				t.Fatalf("after %q the output never showed %q:\n%s", line, want, out.String())
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	say("hi", "one\n> ")
	say("/extension", "on   workspace")
	say("/extension on nope", "unknown extension")
	say("/extension sideways", "usage:")
	say("/extension off workspace", "workspace is now off")
	say("/extension off workspace", "workspace is already off")
	say("next", "two\n> ")
	say("/ext", "off  workspace")
	say("/quit", "")
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the session did not end")
	}

	reqs := provider.Requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	if got := toolNames(reqs[0].Tools); !strings.Contains(got, "read_file") || !strings.Contains(got, "bash") {
		t.Errorf("first request tools = %s, want the workspace tools", got)
	}
	if got := toolNames(reqs[1].Tools); got != "" {
		t.Errorf("after switching off, tools = %s, want none", got)
	}
	var said []string
	for _, m := range reqs[1].Messages[1:] {
		said = append(said, m.Role+":"+m.Content)
	}
	if got, want := strings.Join(said, " | "), "user:hi | assistant:one | user:next"; got != want {
		t.Errorf("conversation after the switch = %s\nwant %s", got, want)
	}
	if sys := reqs[1].Messages[0].Content; strings.Contains(sys, "## Workspace") || !strings.HasPrefix(sys, "base") {
		t.Errorf("system prompt after the switch still carries the extension's text:\n%s", sys)
	}
}

func TestExtensionSwitchFlags(t *testing.T) {
	got := config{with: "a, , b", without: "c"}.switches()
	if !slices.Equal(got.Enable, []string{"a", "b"}) || !slices.Equal(got.Disable, []string{"c"}) {
		t.Errorf("switches = %+v, want a and b on, c off", got)
	}
	if got := (config{}).switches(); len(got.Enable)+len(got.Disable) != 0 {
		t.Errorf("no flags: switches = %+v, want none", got)
	}
}

// TestCancelOn: Ctrl-C cancels the startup until the session takes it
// over; afterwards the signal is left for the session to read.
func TestCancelOn(t *testing.T) {
	interrupt := make(chan os.Signal, 1)
	startCtx, started := cancelOn(context.Background(), interrupt)
	interrupt <- os.Interrupt
	select {
	case <-startCtx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Ctrl-C during startup did not cancel it")
	}
	started()

	startCtx, started = cancelOn(context.Background(), interrupt)
	started()
	started() // releasing twice is harmless
	// The goroutine has let go once a signal stays in the channel.
	interrupt <- os.Interrupt
	select {
	case <-startCtx.Done():
		t.Error("after the session took over, Ctrl-C must not cancel the startup context")
	case <-time.After(50 * time.Millisecond):
	}
	if len(interrupt) != 1 {
		t.Error("the signal must be left for the session")
	}
}

// noted is a slash command that shows its own name when run.
type noted struct {
	name    string
	aliases []string
}

func (n noted) Definition() command.Definition {
	return command.Definition{Name: n.name, Aliases: n.aliases, Description: "A test command.",
		Source: command.Source{Kind: command.SourceBuiltin, Path: "test"}}
}

func (n noted) Handle(_ context.Context, sess command.Session, _ string) error {
	sess.Display("ran " + n.name)
	return nil
}

// TestExtensionCommand_CommandsFollowTheSwitch: an extension's commands
// come and go with it, and one whose commands clash with the session's
// is not switched on at all.
func TestExtensionCommand_CommandsFollowTheSwitch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("NIB_GLOBAL_SKILLS_DIR", t.TempDir())

	ext := func(name string, cmd noted) extension.Spec {
		return extension.Spec{Name: name, Description: name + " commands",
			New: func(context.Context, extension.Env) (extension.Extension, error) {
				return extension.Extension{Toolset: kit.Toolset{Commands: []command.Command{cmd}}}, nil
			}}
	}
	registry, err := extension.NewRegistry(
		ext("good", noted{name: "scan"}),
		ext("clash", noted{name: "leave", aliases: []string{"exit"}}),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	assembly, err := nib.Assemble(ctx, nib.Options{Dir: t.TempDir(), Extensions: registry})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := assembly.Agent(llmtest.NewScripted())
	if err != nil {
		t.Fatal(err)
	}
	s := &session{provider: llmtest.NewScripted(), assembly: assembly, agent: agent}
	defer func() { s.close() }() // the agent is replaced during the test

	in, keys := io.Pipe()
	out := &screen{}
	r, err := repl.New(repl.Options{Agent: s.agent, In: in, Out: out})
	if err != nil {
		t.Fatal(err)
	}
	c := &extensionCommand{session: s, repl: r}
	reg, err := c.registry()
	if err != nil {
		t.Fatal(err)
	}
	r.SetCommands(reg)
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	shown := 0
	say := func(line, want string) {
		t.Helper()
		if _, err := io.WriteString(keys, line+"\n"); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		// Only output since the last line counts: the phrases repeat.
		for !strings.Contains(out.String()[shown:], want) {
			if time.Now().After(deadline) {
				t.Fatalf("after %q the output never showed %q:\n%s", line, want, out.String()[shown:])
			}
			time.Sleep(2 * time.Millisecond)
		}
		shown = len(out.String())
	}
	say("/scan", "error:")
	say("/extension on good", "good is now on")
	say("/scan", "ran scan")
	say("/extension on clash", "clash stays as it was")
	say("/extension", "off  clash")
	say("/leave", "error:")
	say("/scan", "ran scan")
	say("/extension off good", "good is now off")
	say("/scan", "error:")
	say("/quit", "")
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the session did not end")
	}
	if got := s.assembly.Extensions(); got[0].On || got[1].On {
		t.Errorf("extensions at the end = %+v, want both off", got)
	}
}
