package repl

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/ai/llmtest"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/command"
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
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(h.out.String(), want) {
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
