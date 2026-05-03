// Package headless provides a frontend-free runner that drives a kit
// agent to completion by draining its event channel. It owns the
// generic event vocabulary (token, status, lifecycle, error) and the
// REPL loop in TTY mode; specializations layer domain-specific event
// handling via an [EventHandler].
//
// Two entry points fit different agent shapes:
//
//   - [Runner.Run] starts the agent on a prompt string and drains
//     events. Use this when the agent's run-start API is the
//     kit-generic [Agent.Prompt].
//   - [Runner.Drive] enters the drain loop without starting the
//     agent. Use this when the caller's run-start API is richer than
//     [Agent.Prompt] and translating to a single prompt string would
//     lose fidelity (the coding agent's per-run setup is the
//     canonical example).
package headless

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/latebit-io/nib/kit/event"
)

// Agent is the narrow surface a [Runner] needs to drain events and
// run the REPL loop. Required for both [Runner.Run] and
// [Runner.Drive]. Agents whose run-start API takes a single prompt
// string additionally satisfy [Prompter]; richer run-start paths
// (the coding agent's [Run] is the canonical case) use [Runner.Drive]
// and start the agent independently.
type Agent interface {
	// Reply queues a follow-up input on a running agent. Returns true
	// when the reply was accepted; false when the agent is no longer
	// accepting input and the run should terminate.
	Reply(ctx context.Context, input string) bool
	// Cancel asks the agent to stop. The runner calls Cancel on EOF
	// in single-shot mode, on stdin EOF in REPL mode, and on context
	// cancellation.
	Cancel()
}

// Prompter is an [Agent] whose run-start API is the kit-generic
// (ctx, prompt) shape. Required for [Runner.Run]. Not required for
// [Runner.Drive].
type Prompter interface {
	Agent
	// Prompt starts a new run with the given user prompt. Returns an
	// error if the agent rejects the prompt (concurrent run already
	// active, agent closed, etc.).
	Prompt(ctx context.Context, prompt string) error
}

// EventHandler is the extension point for events the runner does not
// own. Specializations register a handler via [WithHandler] to react
// to domain events (edit proposals, file creation, custom statuses).
//
// The handler runs synchronously in the runner's drain goroutine, so
// handler-owned state needs no synchronization with the runner. A
// non-nil error appends to [Result.Errors]; the run is not aborted.
//
// AgentStatus events are forwarded to the handler only when the
// runner does not recognize the [event.StatusKind] — the runner
// handles its own generic statuses (Thinking, Planning) on stderr;
// domain-specific kinds (Linting, Reviewing) reach the handler.
type EventHandler func(ctx context.Context, ev event.Event) error

// Result captures the outcome of a single [Runner.Run] or
// [Runner.Drive] call. Specializations embed this type and add
// domain-specific fields (files changed, edits applied, etc.).
type Result struct {
	// Success is true when the run reached AgentDone with Success=true,
	// or when the user ended a TTY session cleanly via stdin EOF.
	Success bool `json:"success"`
	// Summary is the agent's accumulated streamed text from the final
	// turn. In REPL mode prior turns are streamed to stderr and not
	// retained here.
	Summary string `json:"summary"`
	// Errors collects error messages encountered during the run,
	// including those returned by the [EventHandler] and ctx.Err() on
	// cancellation. Order matches occurrence.
	Errors []string `json:"errors,omitempty"`
}

// Runner drains an agent event channel and orchestrates a single
// run-to-completion lifecycle. In TTY mode it loops on AgentWaiting,
// reading follow-up input from stdin. Concurrent invocations of [Run]
// or [Drive] on the same Runner are not supported — the lazy stdin
// reader and the drain loop both assume single-goroutine ownership.
type Runner struct {
	agent   Agent
	events  <-chan event.Event
	handler EventHandler
	stdin   io.Reader
	stderr  io.Writer
	isTTY   bool

	// lines is the shared stdin reader channel, started lazily on the
	// first REPL prompt and kept alive for the runner's lifetime so a
	// cancelled run does not leak a scanner goroutine.
	lines chan stdinLine
}

// Option configures a [Runner] at construction.
type Option func(*Runner)

// WithHandler registers an [EventHandler] for events the runner does
// not own. The runner still dispatches generic events (token,
// status, lifecycle) through its own logic.
func WithHandler(h EventHandler) Option { return func(r *Runner) { r.handler = h } }

// WithStdin overrides the runner's stdin source. Defaults to os.Stdin.
// Useful in tests to pre-script REPL input.
func WithStdin(in io.Reader) Option { return func(r *Runner) { r.stdin = in } }

// WithStderr overrides the runner's stderr destination for status
// writes. Defaults to [io.Discard]; pass os.Stderr for streaming
// status output to a human in TTY mode.
func WithStderr(out io.Writer) Option { return func(r *Runner) { r.stderr = out } }

// WithTTY toggles REPL mode. In TTY mode the runner reads follow-up
// input from stdin on every AgentWaiting; in single-shot mode
// AgentWaiting triggers Cancel and exit.
func WithTTY(tty bool) Option { return func(r *Runner) { r.isTTY = tty } }

// New constructs a [Runner]. The agent and events channel are
// required; pass options to customize stdin, stderr, TTY mode, and
// the [EventHandler].
func New(agent Agent, events <-chan event.Event, opts ...Option) *Runner {
	r := &Runner{
		agent:  agent,
		events: events,
		stdin:  os.Stdin,
		stderr: io.Discard,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Run starts the agent on prompt and drains events until the run
// completes. Returns an error result with no drain attempt if the
// agent does not implement [Prompter] or rejects the prompt.
func (r *Runner) Run(ctx context.Context, prompt string) *Result {
	p, ok := r.agent.(Prompter)
	if !ok {
		return &Result{Errors: []string{"agent does not implement headless.Prompter; use Drive for richer run-start paths"}}
	}
	if err := p.Prompt(ctx, prompt); err != nil {
		return &Result{Errors: []string{err.Error()}}
	}
	return r.Drive(ctx)
}

// Drive drains events without starting the agent. Use this when the
// caller has already started the agent through a richer surface than
// [Agent.Prompt] (the coding agent's [Run] is the canonical case).
func (r *Runner) Drive(ctx context.Context) *Result {
	result := &Result{}
	r.processEvents(ctx, result)
	return result
}

// Stderr returns the runner's status output destination. Specializations
// route their domain-specific status writes through the same writer so
// generic and domain output share a single sink.
func (r *Runner) Stderr() io.Writer { return r.stderr }

// IsTTY reports whether the runner is in REPL mode. Specializations
// gate their own status writes on this so non-TTY runs stay quiet.
func (r *Runner) IsTTY() bool { return r.isTTY }

// stdinLine carries one line read from stdin, or an EOF/error signal.
type stdinLine struct {
	text string
	ok   bool // false on EOF or error
}

// startStdinReader launches the single goroutine that reads lines
// from stdin. Called lazily on the first REPL prompt; the goroutine
// exits naturally when stdin reaches EOF or errors.
func (r *Runner) startStdinReader() {
	r.lines = make(chan stdinLine, 1)
	go func() {
		scanner := bufio.NewScanner(r.stdin)
		for scanner.Scan() {
			text := strings.TrimSpace(scanner.Text())
			r.lines <- stdinLine{text, text != ""}
		}
		if err := scanner.Err(); err != nil {
			slog.Error("stdin read failed", "err", err)
		}
		r.lines <- stdinLine{"", false}
	}()
}

// maxSummaryBytes caps the in-memory summary buffer. Tokens beyond
// this limit still stream to stderr but are not retained on the
// [Result]. 10 MB is generous enough that an honest run never trips
// it; the cap exists to bound a runaway transcript.
const maxSummaryBytes = 10 * 1024 * 1024

// processEvents drains the event channel, dispatching each event,
// returning when AgentDone arrives or ctx is cancelled.
func (r *Runner) processEvents(ctx context.Context, result *Result) {
	var summary strings.Builder
	summaryTruncated := false

	for {
		select {
		case <-ctx.Done():
			r.agent.Cancel()
			result.Success = false
			result.Summary = summary.String()
			result.Errors = append(result.Errors, ctx.Err().Error())
			return

		case ev, ok := <-r.events:
			if !ok {
				return
			}
			if done := r.handleEvent(ctx, ev, result, &summary, &summaryTruncated); done {
				return
			}
		}
	}
}

// handleEvent dispatches a single event. Returns true when the run is
// complete and the drain loop should exit.
func (r *Runner) handleEvent(ctx context.Context, ev event.Event, result *Result, summary *strings.Builder, truncated *bool) bool {
	switch e := ev.(type) {
	case event.AgentToken:
		appendSummary(summary, e.Text, truncated)
		r.status("%s", e.Text)
		return false

	case event.AgentStatus:
		if !r.handleStatus(e) {
			r.dispatchHandler(ctx, ev, result)
		}
		return false

	case event.AgentToolCall:
		r.status("[%s]\n", e.Name)
		slog.Debug("tool call", "name", e.Name)
		return false

	case event.AgentError:
		result.Errors = append(result.Errors, e.Err)
		r.status("error: %s\n", e.Err)
		return false

	case event.AgentWaiting:
		return r.handleWaiting(ctx, result, summary, truncated)

	case event.AgentDone:
		result.Success = e.Success
		result.Summary = summary.String()
		return true
	}

	r.dispatchHandler(ctx, ev, result)
	return false
}

// dispatchHandler forwards an event to the [EventHandler] if one is
// registered, recording any returned error on the result.
func (r *Runner) dispatchHandler(ctx context.Context, ev event.Event, result *Result) {
	if r.handler == nil {
		slog.Debug("unhandled event in headless runner", "type", fmt.Sprintf("%T", ev))
		return
	}
	if err := r.handler(ctx, ev); err != nil {
		result.Errors = append(result.Errors, err.Error())
	}
}

// appendSummary adds text to the summary builder, truncating once the
// byte cap is exceeded.
func appendSummary(summary *strings.Builder, text string, truncated *bool) {
	if *truncated {
		return
	}
	if summary.Len()+len(text) > maxSummaryBytes {
		summary.WriteString("\n…[summary truncated]")
		*truncated = true
		return
	}
	summary.WriteString(text)
}

// handleStatus writes a human-readable status line to stderr in TTY
// mode for kit-generic status kinds. Returns false when the kind is
// not generic, so the caller can forward to the [EventHandler] for
// domain-specific kinds (StatusLinting, StatusReviewing, etc.).
func (r *Runner) handleStatus(e event.AgentStatus) bool {
	switch e.Status {
	case event.StatusThinking:
		r.status("[thinking...]\n")
		return true
	case event.StatusPlanning:
		r.status("[planning...]\n")
		return true
	}
	return false
}

// handleWaiting processes AgentWaiting. In single-shot mode the
// runner cancels and exits; in REPL mode it reads stdin and continues
// the conversation. Returns true when the run is complete.
func (r *Runner) handleWaiting(ctx context.Context, result *Result, summary *strings.Builder, truncated *bool) bool {
	result.Summary = summary.String()
	if !r.isTTY {
		r.agent.Cancel()
		result.Success = true
		return true
	}
	input, ok := r.readInput(ctx)
	if !ok {
		r.agent.Cancel()
		result.Success = true
		return true
	}
	summary.Reset()
	*truncated = false
	if !r.agent.Reply(ctx, input) {
		slog.Warn("agent not accepting input, ending conversation")
		result.Success = true
		return true
	}
	return false
}

// status writes a formatted message to stderr when in TTY mode.
// Stderr writes are best-effort; errors are intentionally ignored
// since status output is non-critical.
func (r *Runner) status(format string, args ...any) {
	if !r.isTTY {
		return
	}
	_, _ = fmt.Fprintf(r.stderr, format, args...)
}

// readInput reads a single line from the shared stdin reader,
// cancellable via ctx. Returns the trimmed input and true on success,
// or ("", false) on EOF, error, or context cancellation.
func (r *Runner) readInput(ctx context.Context) (string, bool) {
	if r.lines == nil {
		r.startStdinReader()
	}
	r.status("\n> ")
	select {
	case line := <-r.lines:
		return line.text, line.ok
	case <-ctx.Done():
		return "", false
	}
}
