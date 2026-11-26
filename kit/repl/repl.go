// Package repl drives an agent from a line-based terminal: a prompt,
// streamed output, a way to stop a run, replies while it works, and
// slash commands. It is a session without a TUI.
package repl

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/command"
	"github.com/latebit-io/nib/kit/event"
)

// maxArgPreview is how much of a tool call's arguments one line shows.
const maxArgPreview = 100

// eventBuffer is the subscription inbox. Tokens arrive in bursts while
// the loop is busy printing.
const eventBuffer = 256

// Options configures a [REPL]. Agent, In and Out are required.
type Options struct {
	// Agent is the agent the session talks to.
	Agent *kit.Agent
	// SystemPrompt is the agent's system prompt. It is needed again
	// when a conversation continues in a new run.
	SystemPrompt string
	// In is where lines are read from. Its end ends the session.
	In io.Reader
	// Out is where everything is printed.
	Out io.Writer
	// Commands handles lines that start with a slash. Nil means none.
	Commands *command.Registry
	// Interrupt delivers Ctrl-C: it stops the active run, or ends the
	// session when no run is active. Nil means no interrupts.
	Interrupt <-chan os.Signal
}

// prompt is printed when the session wants a line.
const prompt = "> "

// phase is where the session is in the conversation.
type phase int

const (
	idle    phase = iota // no run; the next line starts one
	running              // the agent is working
	waiting              // the run is parked until the person replies
)

// REPL is one interactive session. It is driven by [REPL.Run] on one
// goroutine; commands call its methods from inside that loop.
type REPL struct {
	in        io.Reader
	out       io.Writer
	commands  *command.Registry
	interrupt <-chan os.Signal

	agent   *kit.Agent
	system  string // the agent's system prompt, resent with each run
	sub     *kit.Subscription
	history []llm.Message // the conversation so far, without the system message
	pending string        // a line typed during a run that the agent did not take
	phase   phase
	midLine bool // output so far does not end with a newline
	quit    bool
}

// New returns a session on opts.Agent, subscribed to its events.
func New(opts Options) (*REPL, error) {
	if opts.Agent == nil || opts.In == nil || opts.Out == nil {
		return nil, errors.New("repl: Agent, In and Out are required")
	}
	r := &REPL{
		in: opts.In, out: opts.Out, commands: opts.Commands, interrupt: opts.Interrupt,
		system: opts.SystemPrompt,
	}
	if err := r.attach(opts.Agent); err != nil {
		return nil, err
	}
	return r, nil
}

// attach subscribes to agent and makes it the session's agent.
func (r *REPL) attach(agent *kit.Agent) error {
	sub, err := agent.Subscribe(kit.SubscribeOptions{BufferSize: eventBuffer})
	if err != nil {
		return fmt.Errorf("repl: %w", err)
	}
	r.agent, r.sub = agent, sub
	return nil
}

// Run reads lines and prints the agent's output until the input ends,
// the person quits, or ctx is cancelled. A run still active at the end
// of input is allowed to finish. The caller closes the agent afterwards.
func (r *REPL) Run(ctx context.Context) error {
	defer r.detach()
	// Cancelled on return, so the reader does not wait on a line nobody
	// takes. It can still sit in Read until In is closed.
	readCtx, stopReading := context.WithCancel(ctx)
	defer stopReading()
	lines := make(chan string)
	go readLines(readCtx, r.in, lines)
	r.prompt()
	// After the end of input, only a run in progress keeps the loop alive.
	for !r.quit && (lines != nil || r.phase == running) {
		select {
		case <-ctx.Done():
			return nil
		case <-r.interrupt:
			if r.phase != running {
				r.println("")
				return nil
			}
			r.agent.Cancel()
		case line, ok := <-lines:
			if !ok {
				lines = nil
				continue
			}
			r.line(ctx, line)
		case ev := <-r.sub.Events():
			r.event(ctx, ev)
		}
	}
	return nil
}

// readLines sends each line of in, then closes out.
func readLines(ctx context.Context, in io.Reader, out chan<- string) {
	defer close(out)
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		select {
		case out <- scanner.Text():
		case <-ctx.Done():
			return
		}
	}
	// A read error ends the session the same way the end of input does.
}

// line handles one line the person typed.
func (r *REPL) line(ctx context.Context, line string) {
	// During a run the output stream is the agent's; otherwise ask again.
	defer r.promptUnlessRunning()
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	if r.commands != nil {
		matched, err := r.commands.Dispatch(ctx, r, line, command.WithBusyCheck(r.Busy))
		if errors.Is(err, command.ErrTurnInFlight) {
			r.println("(the agent is working; press Ctrl-C to stop it first)")
		} else if err != nil {
			r.println("error: " + err.Error())
		}
		if matched {
			return
		}
	}
	if err := r.SubmitPrompt(ctx, line); err != nil {
		r.println("error: " + err.Error())
	}
}

// Display prints text without involving the model. It implements
// [command.Session].
func (r *REPL) Display(text string) { r.println(text) }

// SubmitPrompt sends text to the agent as the person's message: a new
// run, a reply to a parked one, or a follow-up queued behind the work
// in progress. It implements [command.Session].
func (r *REPL) SubmitPrompt(ctx context.Context, text string) error {
	switch r.phase {
	case idle:
		return r.start(ctx, text)
	case waiting:
		if !r.agent.Reply(ctx, text) {
			return errors.New("the agent did not take the reply")
		}
		r.phase = running
	case running:
		// A refusal means the agent's queue is full or the run just
		// ended. Either way the line is kept and sent when it can be.
		if !r.agent.Reply(ctx, text) {
			if r.pending != "" {
				return errors.New("one message is already waiting; press Ctrl-C to stop the run")
			}
			r.pending = text
		}
		r.println("(queued; the agent reads it after the current step)")
	}
	return nil
}

// flush sends a held line now that the run has parked or ended. A
// parked run that still does not take it keeps it for the next chance.
func (r *REPL) flush(ctx context.Context) {
	text := r.pending
	if text == "" {
		return
	}
	r.pending = ""
	switch r.phase {
	case waiting:
		if !r.agent.Reply(ctx, text) {
			r.pending = text
			return
		}
		r.phase = running
	case idle:
		if err := r.start(ctx, text); err != nil {
			r.println("error: your queued message was not sent: " + err.Error())
		}
	}
}

// start begins a run with the conversation so far and text. A new run
// starts with no transcript, so the earlier exchange is sent again.
func (r *REPL) start(ctx context.Context, text string) error {
	msgs := make([]llm.Message, 0, len(r.history)+2)
	if r.system != "" {
		msgs = append(msgs, llm.Message{Role: "system", Content: r.system})
	}
	msgs = append(msgs, r.history...)
	msgs = append(msgs, llm.Message{Role: "user", Content: text})
	if err := r.agent.PromptWithMessages(ctx, msgs); err != nil {
		return err
	}
	r.phase = running
	return nil
}

// event prints one event from the agent and tracks the run's phase.
func (r *REPL) event(ctx context.Context, ev event.Event) {
	switch e := ev.(type) {
	case event.AgentToken:
		r.print(e.Text)
	case event.AgentToolCall:
		args := e.Args
		if len(args) > maxArgPreview {
			args = strings.ToValidUTF8(args[:maxArgPreview], "") + "…"
		}
		r.println("· " + e.Name + " " + args)
	case event.AgentError:
		r.println("error: " + e.Err)
	case event.AgentWaiting:
		r.phase = waiting
		r.flush(ctx)
		r.promptUnlessRunning()
	case event.AgentDone:
		r.finish(e)
		// A stopped run takes its queued line with it: the person hit Ctrl-C.
		if errors.Is(e.Err, kit.ErrAborted) {
			r.pending = ""
		}
		r.flush(ctx)
		r.promptUnlessRunning()
	}
}

// finish records the end of a run and keeps its transcript, so the
// next line continues the conversation.
func (r *REPL) finish(done event.AgentDone) {
	r.settle()
	r.phase = idle
	if msgs := r.agent.State().Messages; len(msgs) > 0 {
		if msgs[0].Role == "system" {
			msgs = msgs[1:]
		}
		r.history = msgs
	}
	switch {
	case errors.Is(done.Err, kit.ErrAborted):
		r.println("(stopped)")
	case !done.Success && done.Err != nil:
		r.println("error: " + done.Err.Error())
	}
}

// settle waits until the agent accepts a new run. Events are drained
// meanwhile: an agent can block on a subscriber that stopped reading.
func (r *REPL) settle() {
	idleCh := make(chan struct{})
	go func() {
		r.agent.WaitForIdle()
		close(idleCh)
	}()
	for {
		select {
		case <-idleCh:
			return
		case <-r.sub.Events():
		}
	}
}

// stopRun cancels an active or parked run and waits for it to end.
func (r *REPL) stopRun() {
	if r.phase == idle {
		return
	}
	r.agent.Cancel()
	for r.phase != idle {
		if done, ok := (<-r.sub.Events()).(event.AgentDone); ok {
			r.finish(done)
		}
	}
}

// detach ends any run and releases the subscription.
func (r *REPL) detach() {
	r.stopRun()
	r.sub.Close()
}

// Swap makes next the session's agent and keeps the conversation. A
// command uses it after rebuilding the agent with different tools. The
// caller closes the previous agent once Swap returns.
func (r *REPL) Swap(next *kit.Agent, systemPrompt string) error {
	r.detach()
	r.system = systemPrompt
	return r.attach(next)
}

// Clear forgets the conversation, stopping any run first.
func (r *REPL) Clear() {
	r.stopRun()
	r.history = nil
}

// Quit ends the session after the current line.
func (r *REPL) Quit() { r.quit = true }

// Busy reports whether the agent is working on a run.
func (r *REPL) Busy() bool { return r.phase == running }

// print writes text as it streams.
func (r *REPL) print(text string) {
	if text == "" {
		return
	}
	r.write(text)
	r.midLine = !strings.HasSuffix(text, "\n")
}

// println writes text on a line of its own.
func (r *REPL) println(text string) {
	if r.midLine {
		r.write("\n")
	}
	r.write(text + "\n")
	r.midLine = false
}

// prompt asks for the next line.
func (r *REPL) prompt() {
	if r.midLine {
		r.write("\n")
	}
	r.write(prompt)
	r.midLine = false
}

// promptUnlessRunning asks for a line when one is wanted.
func (r *REPL) promptUnlessRunning() {
	if r.phase != running {
		r.prompt()
	}
}

// write sends text to Out. A terminal that went away cannot be told
// about it, so the error is dropped; the session ends at end of input.
func (r *REPL) write(text string) {
	_, _ = io.WriteString(r.out, text)
}
