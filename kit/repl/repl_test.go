package repl

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/ai/llmtest"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/command"
	"github.com/latebit-io/nib/kit/elicit"
	"github.com/latebit-io/nib/kit/permission"
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

// harness runs a session against a pipe the test types into.
type harness struct {
	t         *testing.T
	repl      *REPL
	out       *screen
	keys      *io.PipeWriter
	interrupt chan os.Signal
	done      chan error
}

func newAgent(t *testing.T, provider llm.Provider, system string) *kit.Agent {
	t.Helper()
	ag, err := kit.New(kit.Config{Provider: provider, SystemPrompt: system})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ag.Close)
	return ag
}

// start runs a session on agent. setup may register commands before
// the loop starts.
func start(t *testing.T, agent *kit.Agent, system string, setup func(*REPL, *command.Registry)) *harness {
	t.Helper()
	in, keys := io.Pipe()
	h := &harness{t: t, out: &screen{}, keys: keys, interrupt: make(chan os.Signal, 1), done: make(chan error, 1)}
	reg := command.NewRegistry()
	r, err := New(Options{Agent: agent, SystemPrompt: system, In: in, Out: h.out, Commands: reg, Interrupt: h.interrupt})
	if err != nil {
		t.Fatal(err)
	}
	h.repl = r
	for _, c := range append(Commands(r), command.NewHelp(reg)) {
		if err := reg.Register(c); err != nil {
			t.Fatal(err)
		}
	}
	if setup != nil {
		setup(r, reg)
	}
	go func() { h.done <- r.Run(context.Background()) }()
	t.Cleanup(func() { _ = keys.Close() }) // ends the loop if the test did not
	return h
}

// typeLine sends one line, as the person would.
func (h *harness) typeLine(line string) {
	h.t.Helper()
	if _, err := io.WriteString(h.keys, line+"\n"); err != nil {
		h.t.Fatalf("type %q: %v", line, err)
	}
}

// see waits until the output contains want.
func (h *harness) see(want string) {
	h.t.Helper()
	h.seeTimes(want, 1)
}

// seeTimes waits until the output contains want n times, for text
// that repeats, such as a question asked again.
func (h *harness) seeTimes(want string, n int) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(h.out.String(), want) < n {
		if time.Now().After(deadline) {
			h.t.Fatalf("output never showed %q:\n%s", want, h.out.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ended waits for Run to return.
func (h *harness) ended() {
	h.t.Helper()
	select {
	case err := <-h.done:
		if err != nil {
			h.t.Errorf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		h.t.Fatalf("the session did not end:\n%s", h.out.String())
	}
}

// contents returns the role and text of each message, for comparison.
func contents(msgs []llm.Message) string {
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		parts = append(parts, m.Role+":"+m.Content)
	}
	return strings.Join(parts, " | ")
}

// TestRun_ConversationContinues: each line is answered, and the second
// request carries the first exchange.
func TestRun_ConversationContinues(t *testing.T) {
	provider := llmtest.NewScripted(llmtest.Text("hel", "lo"), llmtest.Text("again"))
	h := start(t, newAgent(t, provider, "sys"), "sys", nil)

	h.typeLine("hi")
	h.see("hello\n> ")
	h.typeLine("more")
	h.see("again\n> ")
	if err := h.keys.Close(); err != nil {
		t.Fatal(err)
	}
	h.ended()

	reqs := provider.Requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	if got, want := contents(reqs[1].Messages), "system:sys | user:hi | assistant:hello | user:more"; got != want {
		t.Errorf("second request = %s\nwant %s", got, want)
	}
}

// TestRun_InterruptStopsTheRun: Ctrl-C during a run stops the run and
// keeps the session; Ctrl-C with nothing running ends it.
func TestRun_InterruptStopsTheRun(t *testing.T) {
	h := start(t, newAgent(t, llmtest.Blocking("thinking"), ""), "", nil)

	h.typeLine("go")
	h.see("thinking")
	h.interrupt <- os.Interrupt
	h.see("(stopped)\n> ")
	h.interrupt <- os.Interrupt
	h.ended()
}

// TestRun_EndOfInputLetsTheRunFinish: piped input ends before the
// answer arrives, and the answer is still printed.
func TestRun_EndOfInputLetsTheRunFinish(t *testing.T) {
	provider := llmtest.NewScripted(llmtest.Text("the answer"))
	h := start(t, newAgent(t, provider, ""), "", nil)
	h.typeLine("question")
	if err := h.keys.Close(); err != nil {
		t.Fatal(err)
	}
	h.ended()
	if !strings.Contains(h.out.String(), "the answer") {
		t.Errorf("the answer was not printed:\n%s", h.out.String())
	}
}

func TestRun_Commands(t *testing.T) {
	provider := llmtest.NewScripted(llmtest.Text("one"), llmtest.Text("two"))
	h := start(t, newAgent(t, provider, "sys"), "sys", nil)

	h.typeLine("/help")
	h.see("/clear")
	h.typeLine("/nope")
	h.see("error:")
	h.typeLine("first")
	h.see("one\n> ")
	h.typeLine("/clear")
	h.see("(stopped)\n> ")
	h.typeLine("second")
	h.see("two\n> ")
	h.typeLine("/quit")
	h.ended()

	reqs := provider.Requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2: a command must not reach the model", len(reqs))
	}
	if got, want := contents(reqs[1].Messages), "system:sys | user:second"; got != want {
		t.Errorf("after /clear the request = %s\nwant %s", got, want)
	}
}

// swap is a test command that replaces the session's agent.
type swap struct {
	repl *REPL
	next *kit.Agent
}

func (swap) Definition() command.Definition {
	return command.Definition{Name: "swap", Description: "Swap the agent.", Source: command.Source{Kind: command.SourceBuiltin}}
}

func (s swap) Handle(context.Context, command.Session, string) error {
	return s.repl.Swap(s.next, "new sys")
}

// TestSwap_KeepsTheConversation: after the agent is replaced, the next
// line goes to the new agent with the earlier exchange and its prompt.
func TestSwap_KeepsTheConversation(t *testing.T) {
	first := llmtest.NewScripted(llmtest.Text("one"))
	second := llmtest.NewScripted(llmtest.Text("two"))
	old := newAgent(t, first, "old sys")
	next := newAgent(t, second, "new sys")
	h := start(t, old, "old sys", func(r *REPL, reg *command.Registry) {
		if err := reg.Register(swap{repl: r, next: next}); err != nil {
			t.Fatal(err)
		}
	})

	h.typeLine("hi")
	h.see("one\n> ")
	h.typeLine("/swap")
	h.see("(stopped)\n> ")
	// The replaced agent must close without a reader on its events.
	closed := make(chan struct{})
	go func() { old.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the replaced agent did not close")
	}
	h.typeLine("next")
	h.see("two\n> ")
	h.typeLine("/quit")
	h.ended()

	reqs := second.Requests()
	if len(reqs) != 1 {
		t.Fatalf("requests to the new agent = %d, want 1", len(reqs))
	}
	if got, want := contents(reqs[0].Messages), "system:new sys | user:hi | assistant:one | user:next"; got != want {
		t.Errorf("request = %s\nwant %s", got, want)
	}
}

func TestNew_RequiresAgentAndStreams(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("New without an agent must fail")
	}
}

// gated is a provider whose first turn waits to be released, so a test
// can type while the agent is busy.
type gated struct {
	release chan struct{}
	*llmtest.Scripted
}

func (g *gated) Stream(ctx context.Context, msgs []llm.Message, tools []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	if g.Calls() == 0 {
		select {
		case <-g.release:
		case <-ctx.Done():
		}
	}
	return g.Scripted.Stream(ctx, msgs, tools)
}

// TestRun_LineTheAgentDidNotTakeIsKept: a line typed while the agent's
// own queue is full is held and sent when the run next waits, not
// dropped behind a message that says it was queued.
func TestRun_LineTheAgentDidNotTakeIsKept(t *testing.T) {
	provider := &gated{release: make(chan struct{}),
		Scripted: llmtest.NewScripted(llmtest.Text("one"), llmtest.Text("two"), llmtest.Text("three"))}
	h := start(t, newAgent(t, provider, "sys"), "sys", nil)

	h.typeLine("a")
	h.typeLine("b")
	h.see("(queued")
	h.typeLine("c")
	h.typeLine("d")
	h.see("one message is already waiting")
	close(provider.release)
	h.see("three\n> ")
	h.typeLine("/quit")
	h.ended()

	reqs := provider.Requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d, want 3:\n%s", len(reqs), h.out.String())
	}
	last := reqs[2].Messages
	if got := last[len(last)-1]; got.Role != "user" || got.Content != "c" {
		t.Errorf("the held line reached the model as %+v, want user:c\n%s", got, contents(last))
	}
	if strings.Contains(contents(last), "user:d") {
		t.Errorf("the refused line must not be sent: %s", contents(last))
	}
}

// reload is a test command that replaces the session's commands.
type reload struct {
	repl *REPL
	next *command.Registry
}

func (reload) Definition() command.Definition {
	return command.Definition{Name: "reload", Description: "Replace the commands.", Source: command.Source{Kind: command.SourceBuiltin}}
}

func (c reload) Handle(context.Context, command.Session, string) error {
	c.repl.SetCommands(c.next)
	return nil
}

// TestSetCommands: a host that replaces the command set gets the new
// commands and loses the old ones, as when an extension is switched.
func TestSetCommands(t *testing.T) {
	next := command.NewRegistry()
	if err := next.Register(command.NewHelp(next)); err != nil {
		t.Fatal(err)
	}
	h := start(t, newAgent(t, llmtest.NewScripted(), ""), "", func(r *REPL, reg *command.Registry) {
		if err := reg.Register(reload{repl: r, next: next}); err != nil {
			t.Fatal(err)
		}
	})
	h.typeLine("/reload")
	h.typeLine("/quit")
	h.see("error:")
	h.typeLine("/help")
	h.see("/help")
	if strings.Contains(h.out.String(), "/clear") {
		t.Errorf("the old commands must be gone:\n%s", h.out.String())
	}
	if err := h.keys.Close(); err != nil {
		t.Fatal(err)
	}
	h.ended()
}

type noArgs struct{}

// TestRun_ShowsToolCalls: while a tool runs the session says which,
// and says so when it failed.
func TestRun_ShowsToolCalls(t *testing.T) {
	provider := llmtest.NewScripted(
		llmtest.ToolCalls(llmtest.Call("c1", "boom", `{}`)),
		llmtest.Text("gave up"),
	)
	boom := agent.NewTool("boom", "Always fails.", func(context.Context, noArgs) (string, error) {
		return "", errors.New("no luck")
	})
	ag, err := kit.New(kit.Config{Provider: provider, Toolset: kit.Toolset{Tools: []kit.Tool{boom}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ag.Close)
	h := start(t, ag, "", nil)
	h.typeLine("try it")
	h.see("gave up\n> ")
	for _, want := range []string{"· boom {}", "! boom failed"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, h.out.String())
		}
	}
	if err := h.keys.Close(); err != nil {
		t.Fatal(err)
	}
	h.ended()
}

// TestRun_AnswersApprovalRequests: in the approve mode a tool call
// waits for the person; "n" refuses it and the model is told, "y" lets
// it run, and an unclear answer asks again.
func TestRun_AnswersApprovalRequests(t *testing.T) {
	ran := 0
	touch := agent.NewTool("touch", "Changes something.", func(context.Context, noArgs) (string, error) {
		ran++
		return "touched", nil
	})
	provider := llmtest.NewScripted(
		llmtest.ToolCalls(llmtest.Call("c1", "touch", `{}`)),
		llmtest.ToolCalls(llmtest.Call("c2", "touch", `{}`)),
		llmtest.Text("all done"),
	)
	gate := permission.New()
	gate.Register("touch", func(string) permission.Check { return permission.Check{Detail: "the thing", Reason: "risky"} })
	gate.SetMode(permission.Approve)
	ag, err := kit.New(kit.Config{Provider: provider, Toolset: kit.Toolset{Tools: []kit.Tool{touch}, Hooks: gate.Hooks()}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ag.Close)
	h := start(t, ag, "", func(r *REPL, _ *command.Registry) { gate.SetAsker(r.Ask) })

	h.typeLine("do it")
	h.see("? touch wants to run: the thing  (risky)")
	h.see("allow? [y]es, [a]lways, [n]o: ")
	h.typeLine("maybe")
	h.typeLine("n")
	// The first call's question, asked again after "maybe", then the
	// second call's: only then is there something for "y" to answer.
	h.seeTimes("? touch wants to run", 3)
	h.typeLine("y")
	h.see("all done\n> ")
	if ran != 1 || !strings.Contains(h.out.String(), "! touch failed") {
		t.Errorf("the tool ran %d times; want once, after a refusal shown as a failed call", ran)
	}
	if got := strings.Count(h.out.String(), "? touch wants to run"); got != 3 {
		t.Errorf("the question was shown %d times, want 3 (two calls, one repeat)", got)
	}
	if err := h.keys.Close(); err != nil {
		t.Fatal(err)
	}
	h.ended()
}

// TestRun_ApprovalWithNoInputLeftIsRefused: piped input ends before
// the question comes; the call is refused and the run still finishes.
func TestRun_ApprovalWithNoInputLeftIsRefused(t *testing.T) {
	ran := 0
	touch := agent.NewTool("touch", "Changes something.", func(context.Context, noArgs) (string, error) {
		ran++
		return "touched", nil
	})
	provider := llmtest.NewScripted(llmtest.ToolCalls(llmtest.Call("c1", "touch", `{}`)), llmtest.Text("could not"))
	gate := permission.New()
	gate.SetMode(permission.Approve)
	ag, err := kit.New(kit.Config{Provider: provider, Toolset: kit.Toolset{Tools: []kit.Tool{touch}, Hooks: gate.Hooks()}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ag.Close)
	h := start(t, ag, "", func(r *REPL, _ *command.Registry) { gate.SetAsker(r.Ask) })
	h.typeLine("do it")
	if err := h.keys.Close(); err != nil {
		t.Fatal(err)
	}
	h.ended()
	if ran != 0 || !strings.Contains(h.out.String(), "could not") {
		t.Errorf("tool ran %d times; output:\n%s\nwant the call refused and the run finished", ran, h.out.String())
	}
}

// TestRun_AnswersServerQuestions: a server's form is asked field by
// field, a bad answer is asked again, and the person reviews before
// anything is sent. A page to open needs a yes, then opens outside.
func TestRun_AnswersServerQuestions(t *testing.T) {
	var asker elicit.Asker
	var got []elicit.Result
	form := elicit.Request{From: "github", Mode: elicit.Form, Message: "Which repository?", Fields: []elicit.Field{
		{Name: "repo", Title: "Repository", Type: "string", Required: true},
		{Name: "count", Type: "integer"},
	}}
	page := elicit.Request{From: "github", Mode: elicit.Page, Message: "Connect your account", URL: "https://github.example/connect"}
	ask := agent.NewTool("ask", "Asks.", func(ctx context.Context, _ noArgs) (string, error) {
		for _, e := range []elicit.Request{form, page} {
			res, err := asker(ctx, e)
			if err != nil {
				return "", err
			}
			got = append(got, res)
		}
		return "asked", nil
	})
	provider := llmtest.NewScripted(llmtest.ToolCalls(llmtest.Call("c1", "ask", `{}`)), llmtest.Text("all done"))
	var opened []string
	h := start(t, newAgentWith(t, provider, ask), "", func(r *REPL, _ *command.Registry) {
		asker = r.Elicit
		r.openURL = func(u string) error { opened = append(opened, u); return nil }
	})

	h.typeLine("go")
	h.see("? github asks: Which repository?")
	h.see("Repository: ")
	h.typeLine("")
	h.see("an answer is required")
	h.typeLine("latebit/nib")
	h.see("count [integer; optional]: ")
	h.typeLine("many")
	h.see("a whole number is needed")
	h.typeLine("3")
	h.see("send? [y]es, [e]dit, [d]ecline, [c]ancel: ")
	h.typeLine("y")
	h.see("? github asks you to open a page: Connect your account")
	h.see("site: github.example")
	h.typeLine("y")
	h.see("all done\n> ")
	if len(got) != 2 || got[0].Action != elicit.Accept || got[0].Content["repo"] != "latebit/nib" || got[0].Content["count"] != int64(3) {
		t.Errorf("answers = %+v", got)
	}
	if got[1].Action != elicit.Accept || len(opened) != 1 || opened[0] != page.URL {
		t.Errorf("page answer %+v, opened %v", got[1], opened)
	}
	if err := h.keys.Close(); err != nil {
		t.Fatal(err)
	}
	h.ended()
}

// TestPost_WaitsForTheRun: work posted during a run runs after it.
func TestPost_WaitsForTheRun(t *testing.T) {
	release := make(chan struct{})
	slow := agent.NewTool("slow", "Waits.", func(ctx context.Context, _ noArgs) (string, error) {
		<-release
		return "ok", nil
	})
	provider := llmtest.NewScripted(llmtest.ToolCalls(llmtest.Call("c1", "slow", `{}`)), llmtest.Text("finished"))
	var r *REPL
	h := start(t, newAgentWith(t, provider, slow), "", func(repl *REPL, _ *command.Registry) { r = repl })
	h.typeLine("go")
	h.see("· slow")
	ran := make(chan string, 1)
	go r.Post(func() {
		phase := "during the run"
		if !r.Busy() {
			phase = "after the run"
		}
		r.Display("posted ran " + phase)
		ran <- phase
	})
	close(release)
	h.see("posted ran after the run")
	if phase := <-ran; phase != "after the run" {
		t.Errorf("posted work ran %s", phase)
	}
	if err := h.keys.Close(); err != nil {
		t.Fatal(err)
	}
	h.ended()
}

// newAgentWith is an agent with tools.
func newAgentWith(t *testing.T, provider llm.Provider, tools ...kit.Tool) *kit.Agent {
	t.Helper()
	ag, err := kit.New(kit.Config{Provider: provider, Toolset: kit.Toolset{Tools: tools}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ag.Close)
	return ag
}

// TestRun_ServerTextCannotForgeLines: control characters in a server's
// text are printed as spaces, so it cannot draw a line of its own.
func TestRun_ServerTextCannotForgeLines(t *testing.T) {
	var asker elicit.Asker
	forged := elicit.Request{From: "evil", Mode: elicit.Page, Message: "hi\n? bank asks you to open a page: login\x1b[2K\u202e", URL: "https://evil.example"}
	ask := agent.NewTool("ask", "Asks.", func(ctx context.Context, _ noArgs) (string, error) {
		_, err := asker(ctx, forged)
		return "asked", err
	})
	provider := llmtest.NewScripted(llmtest.ToolCalls(llmtest.Call("c1", "ask", `{}`)), llmtest.Text("done"))
	h := start(t, newAgentWith(t, provider, ask), "", func(r *REPL, _ *command.Registry) {
		asker = r.Elicit
		r.openURL = func(string) error { return nil }
	})
	h.typeLine("go")
	h.see("open it in your browser?")
	h.typeLine("n")
	h.see("done\n> ")
	out := h.out.String()
	if strings.ContainsAny(out, "\x1b\u202e") || strings.Contains(out, "\n? bank") {
		t.Errorf("the server's text drew its own line or escape:\n%q", out)
	}
	if err := h.keys.Close(); err != nil {
		t.Fatal(err)
	}
	h.ended()
}

// TestRun_PromptAndStopHooks: a prompt hook changes or refuses what is
// sent; a stop hook sends the agent back to work, at most a few times.
func TestRun_PromptAndStopHooks(t *testing.T) {
	provider := llmtest.NewScripted(llmtest.Text("first"), llmtest.Text("second"), llmtest.Text("third"))
	stops := 0
	h := start(t, newAgentWith(t, provider), "", func(r *REPL, _ *command.Registry) {
		r.onPrompt = func(_ context.Context, text string) (string, error) {
			if text == "forbidden" {
				return "", errors.New("a hook blocked the prompt: no")
			}
			return text + " (with context)", nil
		}
		r.onStop = func(_ context.Context, answer string, active bool) string {
			stops++
			if answer == "first" && !active {
				return "check your work"
			}
			return ""
		}
	})
	h.typeLine("forbidden")
	h.see("error: a hook blocked the prompt: no")
	h.typeLine("hi")
	h.see("(a hook asks the agent to go on: check your work)")
	h.see("second\n> ")
	reqs := provider.Requests()
	if len(reqs) != 2 || reqs[0].Messages[0].Content != "hi (with context)" || reqs[1].Messages[len(reqs[1].Messages)-1].Content != "check your work" {
		t.Fatalf("requests = %+v", reqs)
	}
	if stops != 2 {
		t.Errorf("stop hook ran %d times, want twice (after each answer)", stops)
	}
	if err := h.keys.Close(); err != nil {
		t.Fatal(err)
	}
	h.ended()
}
