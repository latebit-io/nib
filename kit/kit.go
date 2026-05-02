// Package kit is the curated library surface for building agents on the
// nib foundation. It wraps [github.com/latebit-io/nib/agent] (the bare
// loop) with a small facade that re-exposes the foundation's tool and
// hook contracts under the kit namespace and translates the foundation's
// loop-lifecycle events into the consumer-facing
// [github.com/latebit-io/nib/kit/event] vocabulary.
//
// The intended consumer pattern:
//
//	events := make(chan event.Event, 64)
//	a, err := kit.New(kit.Config{
//	    Provider: provider,
//	    Events:   events,
//	    Tools:    tools,
//	    Hooks:    hooks,
//	})
//	if err != nil { ... }
//	go drain(events)
//	a.Prompt(ctx, "do the thing")
//
// Applications that need the foundation's lower-level event stream or
// raw lifecycle observability can import
// [github.com/latebit-io/nib/agent] directly — kit is the curated path,
// not the only path.
package kit

import (
	"context"
	"fmt"

	"github.com/latebit-io/nib/agent"
	agentevent "github.com/latebit-io/nib/agent/event"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit/event"
)

// Tool is the foundation [agent.Tool] re-exported under kit. Consumers
// import only "kit" to declare tools.
type Tool = agent.Tool

// ToolResult is the foundation [agent.ToolResult] re-exported under kit.
type ToolResult = agent.ToolResult

// Hooks is the foundation [agent.Hooks] re-exported under kit.
type Hooks = agent.Hooks

// BeforeToolCallContext is the foundation [agent.BeforeToolCallContext]
// re-exported under kit.
type BeforeToolCallContext = agent.BeforeToolCallContext

// BeforeToolCallResult is the foundation [agent.BeforeToolCallResult]
// re-exported under kit.
type BeforeToolCallResult = agent.BeforeToolCallResult

// AfterToolCallContext is the foundation [agent.AfterToolCallContext]
// re-exported under kit.
type AfterToolCallContext = agent.AfterToolCallContext

// AfterToolCallResult is the foundation [agent.AfterToolCallResult]
// re-exported under kit.
type AfterToolCallResult = agent.AfterToolCallResult

// TruncationContext is the foundation [agent.TruncationContext]
// re-exported under kit.
type TruncationContext = agent.TruncationContext

// TruncationResult is the foundation [agent.TruncationResult]
// re-exported under kit.
type TruncationResult = agent.TruncationResult

// State is the foundation [agent.State] re-exported under kit.
type State = agent.State

// ErrInvalidOptions wraps the foundation's [agent.ErrInvalidOptions] so
// kit consumers can errors.Is against a kit-namespaced sentinel without
// importing the foundation package.
var ErrInvalidOptions = agent.ErrInvalidOptions

// ErrRunInProgress wraps the foundation's [agent.ErrRunInProgress].
var ErrRunInProgress = agent.ErrRunInProgress

// Config bundles the construction-time inputs for [New]. Provider and
// Events are required; everything else is optional.
type Config struct {
	// Provider is the LLM provider used for every turn. Required.
	Provider llm.Provider

	// Events is the channel the agent writes consumer-facing events to.
	// Required. The caller must drain this channel; kit drops
	// high-volume streaming events ([event.AgentToken],
	// [event.AgentTurnUsage], [event.AgentInputEstimate]) when the
	// channel is full and blocks for up to 5 seconds on control-flow
	// events before logging an error and discarding.
	Events chan<- event.Event

	// SystemPrompt is the system message prepended to every run's
	// transcript. Empty means "no system message." Application layers
	// compose their prompt and pass the result here, or leave empty
	// and drive runs through [Agent.PromptWithMessages] instead.
	SystemPrompt string

	// Tools are the tool implementations registered against this agent.
	// Each tool's Definition() schema is advertised to the LLM; tool
	// calls route to Execute(). Tools share the agent's lifetime and
	// must be safe to invoke concurrently.
	Tools []Tool

	// Hooks are the application-layer extension points. Nil disables a
	// given hook; the agent skips it without ceremony. Hooks run
	// synchronously on the loop goroutine — long-running work should
	// be dispatched off the hook's call stack.
	Hooks Hooks
}

// Agent is the kit-level handle on a running agent. Construct with [New];
// drive with [Agent.Prompt] / [Agent.Reply] / [Agent.Abort]; observe via
// the events channel from [Config].
//
// Concurrency: every method on Agent is safe to call from any goroutine.
// The wrapped foundation serializes its loop on a single goroutine.
type Agent struct {
	foundation       *agent.Agent
	foundationEvents chan agentevent.Event
	consumerEvents   chan<- event.Event

	// runUnsuccessful is set by [Agent.Abort] and by the translator on
	// [agentevent.Error] so that the next [event.AgentDone] derived
	// from [agentevent.AgentEnd] reports Success=false. The foundation
	// emits AgentEnd whether the run completed cleanly or unwound on
	// error/cancel — kit reconstructs the success bit from the
	// out-of-band signals the foundation does not include in AgentEnd
	// itself.
	//
	// The translator goroutine and Abort are the only writers; the
	// translator is also the only reader. Both writers run on
	// independent goroutines and the read happens on the translator
	// goroutine, so a plain bool guarded by [Agent.foundation]'s lock
	// is unsafe — Abort cannot reach the foundation's lock without
	// serializing through the foundation API. A dedicated field works
	// without a lock here only because every writer either precedes
	// the corresponding AgentEnd (ordered by the foundation lifecycle)
	// or is the translator itself. To keep that invariant explicit and
	// guard against future cross-goroutine writes, all access happens
	// through atomic operations on a uint32 flag.
	runUnsuccessful uint32
}

// New constructs an Agent from cfg. Returns [ErrInvalidOptions] wrapped
// with a specific reason when validation fails.
//
// Validation rules:
//   - cfg.Provider must be non-nil.
//   - cfg.Events must be non-nil.
//   - Each entry in cfg.Tools must satisfy the foundation's tool rules
//     (non-nil, non-empty Definition().Function.Name, names unique).
//
// Side effect: New spawns a translator goroutine that drains the
// foundation's lifecycle events and re-emits each as the closest
// [event] equivalent through cfg.Events. The goroutine lives the
// agent's lifetime — kit has no Close path today and the foundation's
// events channel is never closed by the foundation itself.
func New(cfg Config) (*Agent, error) {
	if cfg.Provider == nil {
		return nil, fmt.Errorf("%w: Provider is required", ErrInvalidOptions)
	}
	if cfg.Events == nil {
		return nil, fmt.Errorf("%w: Events is required", ErrInvalidOptions)
	}

	foundationEvents := make(chan agentevent.Event, 64)

	f, err := agent.New(agent.Options{
		Provider:     cfg.Provider,
		Events:       foundationEvents,
		SystemPrompt: cfg.SystemPrompt,
		Tools:        cfg.Tools,
		Hooks:        cfg.Hooks,
	})
	if err != nil {
		return nil, err
	}

	a := &Agent{
		foundation:       f,
		foundationEvents: foundationEvents,
		consumerEvents:   cfg.Events,
	}

	go a.translateEvents()
	return a, nil
}

// Prompt starts a new run with the given user message. Returns
// [ErrRunInProgress] when a run is already active. Lifecycle progress
// flows through the events channel; [Agent.WaitForIdle] blocks for run
// completion.
//
// Prompt resets the unsuccess flag at the start of each run so a prior
// run's [Agent.Abort] or error does not poison the next run's
// [event.AgentDone].
func (a *Agent) Prompt(ctx context.Context, content string) error {
	a.resetUnsuccessful()
	return a.foundation.Prompt(ctx, content)
}

// PromptWithMessages starts a new run with a caller-supplied initial
// transcript instead of the [system?, user] slice [Agent.Prompt] builds
// from [Config.SystemPrompt]. Useful for applications that compose
// dynamic per-run transcripts (memory summary, context files, file
// content) alongside the user's goal.
//
// Same lifecycle and unsuccess-reset semantics as [Agent.Prompt].
func (a *Agent) PromptWithMessages(ctx context.Context, messages []llm.Message) error {
	a.resetUnsuccessful()
	return a.foundation.PromptWithMessages(ctx, messages)
}

// Reply queues a developer follow-up for the active run. Returns true
// when the message was accepted, false when no run is active or the
// reply queue is full. The reply queue is a buffered channel of
// capacity 1 inside the foundation.
func (a *Agent) Reply(ctx context.Context, content string) bool {
	return a.foundation.Reply(ctx, content)
}

// Abort cancels the current run. Safe to call when no run is active.
// The events channel receives an [event.AgentDone] with Success=false
// once the loop unwinds; use [Agent.WaitForIdle] to block until that
// happens.
func (a *Agent) Abort() {
	a.markUnsuccessful()
	a.foundation.Abort()
}

// State returns a read-only snapshot of the agent's current state.
// Slices and maps in the result are safe to read without further
// synchronization; modifying them does not affect the agent.
func (a *Agent) State() State {
	return a.foundation.State()
}

// WaitForIdle blocks until the current run (if any) has finished and
// the foundation goroutine has unwound. Returns immediately when no
// run is active.
func (a *Agent) WaitForIdle() {
	a.foundation.WaitForIdle()
}
