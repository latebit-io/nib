// Package agent implements a generic, application-agnostic LLM agent loop.
//
// This is a bare-bones, embeddable foundation for building any agent. It
// owns the multi-turn conversation, dispatches tool calls, and emits
// lifecycle events on a typed channel. It does NOT know about file edits,
// validation, lint, memory, approval flow, prompts, or any other
// application concept — those live in the consuming application.
//
// The agent ships with zero tools. Consumers register their own tools via
// [Tool] and extend behavior via [Hooks] (BeforeToolCall, AfterToolCall,
// TransformContext, SteeringMessages, FollowUpMessages, BeforePark,
// OnTruncated). Anything an application needs to layer on top of the loop
// happens through those two extension points.
package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/latebit-io/nib/agent/event"
	"github.com/latebit-io/nib/ai/llm"
)

// Options bundles the construction-time inputs for [New]. Every field
// other than Provider is optional.
type Options struct {
	// Provider is the LLM provider used for every turn. Required.
	Provider llm.Provider

	// Events receives lifecycle events; nil discards them (enough for a
	// host that only uses [Agent.Run]). A non-nil channel must be drained:
	// control events block up to 5s, streaming events drop when it is full.
	Events chan<- event.Event

	// SystemPrompt is the system message prepended to every run's
	// transcript. Empty means "no system message." Application layers
	// compose their prompt and pass the result here.
	SystemPrompt string

	// Tools are the tool implementations registered against this agent.
	// The agent advertises Definition() of each to the LLM and routes
	// tool calls to Execute(). Tools share the agent's lifetime.
	Tools []Tool

	// MaxTurns caps the number of LLM turns the loop may take without an
	// intervening user message — the guard against tool-call ping-pong in
	// embedded or unattended deployments. The counter increments per turn
	// (steering, follow-up, and truncation-retry turns all count) and
	// resets to zero each time [Agent.Reply] delivers input. When the loop
	// would start turn MaxTurns+1 it instead emits
	// [event.MaxTurnsReached] and ends the run cleanly: every dispatched
	// tool call already has its result appended, so the transcript stays
	// well-formed and the conversation can continue on the next Prompt.
	// Zero (the default) means unlimited; negative is rejected by [New].
	MaxTurns int

	// OneShot ends the run once a turn has no tool calls and no steering
	// or follow-up is queued, instead of parking. [Hooks.BeforePark] and
	// [event.AgentParked] are skipped and [Agent.Reply] returns false.
	OneShot bool

	// Hooks are the application-layer extension points. Nil disables a
	// given hook; the agent skips it without ceremony.
	Hooks Hooks
}

// Agent runs the multi-turn LLM loop on one goroutine. Drive it with
// [Agent.Prompt] / [Agent.Reply] and the events channel, or call
// [Agent.Run]. Methods are goroutine-safe; hooks must not block forever.
type Agent struct {
	provider     llm.Provider
	events       chan<- event.Event
	systemPrompt string
	tools        map[string]Tool
	toolDefs     []llm.ToolDef
	hooks        Hooks
	maxTurns     int
	oneShot      bool

	// mu guards the run-state fields below. It is NOT held while
	// invoking the provider, hooks, or tool Execute calls — those
	// would deadlock the public surface (Reply, State, Abort) the loop
	// must remain responsive to.
	mu               sync.Mutex
	running          bool
	streaming        bool
	cancel           context.CancelFunc
	doneCh           chan struct{}
	inputCh          chan string
	messages         []llm.Message
	pendingToolCalls map[string]bool
	lastErr          error
}

// runResult is what a finished run hands back to [Agent.Run].
type runResult struct {
	end event.AgentEnd
	err error
}

// ErrInvalidOptions is returned by [New] when Options is missing a required
// field or contains an inconsistency that would produce an unusable agent
// (nil tool, empty tool name, duplicate tool name).
var ErrInvalidOptions = errors.New("agent: invalid options")

// ErrRunInProgress is returned by [Agent.Prompt] when called while a run
// is already active. Callers must Abort the existing run (and optionally
// WaitForIdle) before starting a new one.
var ErrRunInProgress = errors.New("agent: a run is already in progress")

// New constructs an Agent from Options. Returns ErrInvalidOptions wrapped
// with a specific reason when validation fails.
//
// Validation rules:
//   - Provider must be non-nil.
//   - Each entry in Tools must be non-nil.
//   - Each tool's Definition().Function.Name must be non-empty.
//   - Tool names must be unique. A duplicate is a configuration error;
//     silently overwriting would desync the tools map and toolDefs slice
//     (the LLM would see the duplicate while only one route exists).
//   - MaxTurns must not be negative (zero means unlimited).
func New(opts Options) (*Agent, error) {
	if opts.Provider == nil {
		return nil, fmt.Errorf("%w: Provider is required", ErrInvalidOptions)
	}
	if opts.MaxTurns < 0 {
		return nil, fmt.Errorf("%w: MaxTurns is negative (%d)", ErrInvalidOptions, opts.MaxTurns)
	}

	tools := make(map[string]Tool, len(opts.Tools))
	defs := make([]llm.ToolDef, 0, len(opts.Tools))
	for i, t := range opts.Tools {
		if t == nil {
			return nil, fmt.Errorf("%w: Tools[%d] is nil", ErrInvalidOptions, i)
		}
		def := t.Definition()
		name := def.Function.Name
		if name == "" {
			return nil, fmt.Errorf("%w: Tools[%d] has empty Definition().Function.Name", ErrInvalidOptions, i)
		}
		if _, dup := tools[name]; dup {
			return nil, fmt.Errorf("%w: duplicate tool name %q", ErrInvalidOptions, name)
		}
		tools[name] = t
		defs = append(defs, def)
	}

	return &Agent{
		provider:     opts.Provider,
		events:       opts.Events,
		systemPrompt: opts.SystemPrompt,
		tools:        tools,
		toolDefs:     defs,
		hooks:        opts.Hooks,
		maxTurns:     opts.MaxTurns,
		oneShot:      opts.OneShot,
	}, nil
}

// Prompt starts a new run with the given user message. Returns
// [ErrRunInProgress] when a run is already active; callers must
// [Agent.Abort] the previous run (and optionally [Agent.WaitForIdle])
// before starting a new one. Lifecycle progress flows through the events
// channel; [Agent.WaitForIdle] blocks for run completion.
//
// The provided context governs the run's lifetime. Cancelling it is
// equivalent to [Agent.Abort]: the loop unwinds and emits
// [event.AgentEnd]. Hooks and tool Execute calls receive a child context
// derived from this ctx so they unblock alongside the run.
func (a *Agent) Prompt(ctx context.Context, content string) error {
	msgs := make([]llm.Message, 0, 2)
	if a.systemPrompt != "" {
		msgs = append(msgs, llm.Message{Role: "system", Content: a.systemPrompt})
	}
	msgs = append(msgs, llm.Message{Role: "user", Content: content})
	return a.startRun(ctx, msgs, a.oneShot, nil)
}

// PromptWithMessages starts a new run with a caller-supplied initial
// transcript instead of the [system?, user] slice [Agent.Prompt] builds
// from [Options.SystemPrompt] + content.
//
// Useful for applications that compose dynamic per-run transcripts —
// e.g. a coding agent that injects file content, context files, and a
// memory summary alongside the user's goal, with system text that
// depends on runtime mode and configuration. Such applications should
// leave [Options.SystemPrompt] empty and drive every run through this
// method.
//
// The agent takes a defensive copy of messages; the caller may mutate
// its slice after the call returns. An empty slice is rejected with
// [ErrInvalidOptions] — a run with no initial messages would have
// nothing to send to the provider on the first turn.
//
// Same lifecycle semantics as [Agent.Prompt]: [ErrRunInProgress] when a
// run is active, ctx cancellation unwinds the loop, [event.AgentEnd]
// signals completion.
func (a *Agent) PromptWithMessages(ctx context.Context, messages []llm.Message) error {
	if len(messages) == 0 {
		return fmt.Errorf("%w: messages is empty", ErrInvalidOptions)
	}
	return a.startRun(ctx, slices.Clone(messages), a.oneShot, nil)
}

// Run starts a one-shot run from messages and blocks until it ends. The
// [event.AgentEnd] is filled in even when err is non-nil: err is the
// failure cause, the ctx error on abort, or why the run never started.
func (a *Agent) Run(ctx context.Context, messages []llm.Message) (event.AgentEnd, error) {
	if len(messages) == 0 {
		return event.AgentEnd{}, fmt.Errorf("%w: messages is empty", ErrInvalidOptions)
	}
	result := make(chan runResult, 1)
	if err := a.startRun(ctx, slices.Clone(messages), true, result); err != nil {
		return event.AgentEnd{}, err
	}
	res := <-result
	return res.end, res.err
}

// startRun launches a run for [Agent.Prompt], [Agent.PromptWithMessages]
// and [Agent.Run]. It keeps msgs as passed (callers clone); a non-nil
// result receives the run's outcome when it ends.
func (a *Agent) startRun(ctx context.Context, msgs []llm.Message, oneShot bool, result chan<- runResult) error {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return ErrRunInProgress
	}

	runCtx, cancel := context.WithCancel(ctx)
	doneCh := make(chan struct{})
	// A one-shot run never reads a reply; a nil channel makes Reply
	// refuse instead of queueing input the loop would drop.
	var inputCh chan string
	if !oneShot {
		inputCh = make(chan string, 1)
	}

	a.running = true
	a.streaming = false
	a.cancel = cancel
	a.doneCh = doneCh
	a.inputCh = inputCh
	a.messages = msgs
	a.pendingToolCalls = nil
	a.lastErr = nil
	a.mu.Unlock()

	go a.runLoop(runCtx, oneShot, result)
	return nil
}

// Reply queues a follow-up for the active run and reports whether it was
// accepted. It is refused when no run is active, the run is one-shot, or
// an earlier reply is still queued (the queue holds one).
func (a *Agent) Reply(_ context.Context, content string) bool {
	a.mu.Lock()
	ch := a.inputCh
	running := a.running
	a.mu.Unlock()
	if !running || ch == nil {
		return false
	}
	select {
	case ch <- content:
		return true
	default:
		return false
	}
}

// Abort cancels the current run. Safe to call when no run is active —
// it is a no-op in that case. The events channel receives an
// [event.AgentEnd] once the loop unwinds; use [Agent.WaitForIdle] to
// block until that happens.
func (a *Agent) Abort() {
	a.mu.Lock()
	cancel := a.cancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// ReplaceMessages overwrites the saved transcript with msgs. Returns
// [ErrRunInProgress] if a run is active — callers must [Agent.Abort]
// and [Agent.WaitForIdle] before replacing. msgs is cloned; the caller
// retains ownership of the input slice.
//
// Use cases: out-of-band compaction (replace with a smaller slice
// before the next run resumes) and history reset (pass nil to start
// the next run from a clean slate). The next [Agent.PromptWithMessages]
// call will overwrite again — this method only matters when the
// consumer drives a resume-from-saved-state path.
func (a *Agent) ReplaceMessages(msgs []llm.Message) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		return ErrRunInProgress
	}
	if len(msgs) == 0 {
		a.messages = nil
		return nil
	}
	a.messages = slices.Clone(msgs)
	return nil
}

// State returns a read-only snapshot of the agent's current state.
// Slices and maps in the result are safe to read without further
// synchronization; modifying them does NOT affect the agent.
func (a *Agent) State() State {
	a.mu.Lock()
	defer a.mu.Unlock()

	var lastError string
	if a.lastErr != nil {
		lastError = a.lastErr.Error()
	}
	return State{
		Messages:         slices.Clone(a.messages),
		Streaming:        a.streaming,
		PendingToolCalls: maps.Clone(a.pendingToolCalls),
		LastError:        lastError,
	}
}

// WaitForIdle blocks until the current run (if any) has finished and
// the loop goroutine has unwound. Returns immediately when no run is
// active.
func (a *Agent) WaitForIdle() {
	a.mu.Lock()
	ch := a.doneCh
	running := a.running
	a.mu.Unlock()
	if !running || ch == nil {
		return
	}
	<-ch
}
