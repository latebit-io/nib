package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/ai/llmconfig"
	"github.com/latebit-io/nib/ai/llmtest"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/command"
	"github.com/latebit-io/nib/kit/extension"
	"github.com/latebit-io/nib/kit/nib"
	"github.com/latebit-io/nib/kit/repl"
)

// isolate keeps the person's own config, trust store and skills out
// of a test.
func isolate(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv(brand.EnvKeyGlobalSkillsDir, t.TempDir())
	t.Setenv(brand.EnvKeyMCP, "")
}

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

// term is a session a test types into.
type term struct {
	t     *testing.T
	keys  *io.PipeWriter
	out   *screen
	shown int // output up to here was matched by an earlier say
	done  chan error
}

// open runs s the way runInteractive does, on a pipe.
func open(t *testing.T, s *session, approve bool) *term {
	t.Helper()
	in, keys := io.Pipe()
	tm := &term{t: t, keys: keys, out: &screen{}, done: make(chan error, 1)}
	r, err := repl.New(repl.Options{Agent: s.agent, SystemPrompt: s.assembly.SystemPrompt, In: in, Out: tm.out})
	if err != nil {
		t.Fatal(err)
	}
	s.attach(r, approve)
	reg, err := (&extensionCommand{session: s, repl: r}).registry()
	if err != nil {
		t.Fatal(err)
	}
	r.SetCommands(reg)
	go func() { tm.done <- r.Run(context.Background()) }()
	t.Cleanup(func() { _ = keys.Close() }) // ends the loop if the test did not
	return tm
}

// say types a line and waits until the output after it shows want.
func (tm *term) say(line, want string) {
	tm.t.Helper()
	if _, err := io.WriteString(tm.keys, line+"\n"); err != nil {
		tm.t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(tm.out.String()[tm.shown:], want) {
		if time.Now().After(deadline) {
			tm.t.Fatalf("after %q the output never showed %q:\n%s", line, want, tm.out.String()[tm.shown:])
		}
		time.Sleep(2 * time.Millisecond)
	}
	tm.shown = len(tm.out.String())
}

// quit ends the session and waits for the loop to return.
func (tm *term) quit() {
	tm.t.Helper()
	tm.say("/quit", "")
	select {
	case err := <-tm.done:
		if err != nil {
			tm.t.Errorf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		tm.t.Fatal("the session did not end")
	}
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
	isolate(t)
	provider := llmtest.NewScripted(llmtest.Text("one"), llmtest.Text("two"))
	s, err := newSession(context.Background(), t.TempDir(), "base", nil, &llmconfig.Resolved{}, provider, choices{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.close() }() // the agent is replaced during the test
	tm := open(t, s, false)

	tm.say("hi", "one\n> ")
	tm.say("/extension", "on   workspace")
	tm.say("/extension on nope", "unknown extension")
	tm.say("/extension sideways", "usage:")
	tm.say("/extension off workspace", "workspace is now off")
	tm.say("/extension off workspace", "workspace is already off")
	tm.say("next", "two\n> ")
	tm.say("/ext", "off  workspace")
	tm.say("/model", "Model:")
	tm.say("/usage", "2 requests")
	tm.quit()

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
	def := builtin(n.name, "A test command.")
	def.Aliases = n.aliases
	return def
}

func (n noted) Handle(_ context.Context, sess command.Session, _ string) error {
	sess.Display("ran " + n.name)
	return nil
}

// TestExtensionCommand_CommandsFollowTheSwitch: an extension's commands
// come and go with it, and one whose commands clash with the session's
// is not switched on at all.
func TestExtensionCommand_CommandsFollowTheSwitch(t *testing.T) {
	isolate(t)
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
	assembly, err := nib.Assemble(context.Background(), nib.Options{Dir: t.TempDir(), Extensions: registry})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := assembly.Agent(llmtest.NewScripted())
	if err != nil {
		t.Fatal(err)
	}
	s := &session{resolved: &llmconfig.Resolved{}, provider: llmtest.NewScripted(), assembly: assembly, agent: agent}
	defer func() { s.close() }() // the agent is replaced during the test
	tm := open(t, s, false)

	tm.say("/scan", "error:")
	tm.say("/extension on good", "good is now on")
	tm.say("/scan", "ran scan")
	tm.say("/extension on clash", "clash stays as it was")
	tm.say("/extension", "off  clash")
	tm.say("/leave", "error:")
	tm.say("/scan", "ran scan")
	tm.say("/extension off good", "good is now off")
	tm.say("/scan", "error:")
	tm.quit()
	if got := s.assembly.Extensions(); got[0].On || got[1].On {
		t.Errorf("extensions at the end = %+v, want both off", got)
	}
}

// TestSession_ApproveModeAsksBeforeAWrite: started in approve mode, a
// file is written only after the person says yes, and /approve off
// returns the session to acting on its own.
func TestSession_ApproveModeAsksBeforeAWrite(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	write := func(id, name string) []llm.StreamEvent {
		return llmtest.ToolCalls(llmtest.Call(id, "write_file", `{"path":"`+name+`","content":"hello\n"}`))
	}
	provider := llmtest.NewScripted(
		write("c1", "no.txt"), llmtest.Text("refused"),
		write("c2", "yes.txt"), llmtest.Text("written"),
		write("c3", "free.txt"), llmtest.Text("on my own"),
	)
	s, err := newSession(context.Background(), root, "base", nil, &llmconfig.Resolved{}, provider, choices{approve: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	tm := open(t, s, true)

	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(root, name))
		return err == nil
	}
	tm.say("first", "? write_file wants to run: no.txt")
	tm.say("n", "refused\n> ")
	tm.say("second", "? write_file wants to run: yes.txt")
	tm.say("y", "written\n> ")
	tm.say("/approve off", "Autonomous mode")
	tm.say("third", "on my own\n> ")
	tm.quit()
	if exists("no.txt") || !exists("yes.txt") || !exists("free.txt") {
		t.Errorf("files: no.txt %v, yes.txt %v, free.txt %v; want only the last two", exists("no.txt"), exists("yes.txt"), exists("free.txt"))
	}
}

func TestChoices(t *testing.T) {
	isolate(t)
	t.Setenv(brand.EnvKeyTaskTokenBudget, "5000")
	t.Setenv(brand.EnvKeyToolOutputCapDisabled, "")

	chosen, err := config{}.choices()
	if err != nil || chosen.limits.TokenBudget != 5000 || chosen.limits.NoTrimming || chosen.approve {
		t.Errorf("choices = %+v, %v; want the budget from the environment", chosen, err)
	}
	if chosen, _ := (config{budget: 900, approve: true}).choices(); chosen.limits.TokenBudget != 900 || !chosen.approve {
		t.Errorf("choices = %+v, want the flag to decide over the environment", chosen)
	}
	if _, err := (config{approve: true, message: "do it"}).choices(); err == nil {
		t.Error("--approve with -m must be refused: a single run cannot ask")
	}
	t.Setenv(brand.EnvKeyTaskTokenBudget, "2m")
	if _, err := (config{}).choices(); err == nil {
		t.Error("a budget that is not a number must be an error, not no budget")
	}
	if chosen, err := (config{budget: 1000}).choices(); err != nil || chosen.limits.TokenBudget != 1000 {
		t.Errorf("choices = %+v, %v; with --budget a bad environment value must not matter", chosen, err)
	}
}
