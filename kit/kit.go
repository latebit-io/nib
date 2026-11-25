// Package kit is the curated library surface for building agents on the
// nib foundation. It wraps [github.com/latebit-io/nib/agent] (the bare
// loop) with a small facade that re-exposes the foundation's tool and
// hook contracts under the kit namespace and translates the foundation's
// loop-lifecycle events into the consumer-facing
// [github.com/latebit-io/nib/kit/event] vocabulary.
//
// The consumer pattern: the composition root builds a [Toolset]
// literal from the tools, hooks and commands it wants (see
// cmd/nibster and coding/agent) — [Merge] combines several bundles
// when a root assembles more than one. Events flow through a fan-out
// bus — register a [Subscription] via [Agent.Subscribe] to observe them.
//
//	a, err := kit.New(kit.Config{
//	    Provider: provider,
//	    Toolset:  kit.Toolset{Tools: tools, Hooks: hooks},
//	})
//	if err != nil { ... }
//	sub, err := a.Subscribe(kit.SubscribeOptions{})
//	if err != nil { ... }
//	defer sub.Close()
//	go drain(sub.Events())
//	a.Prompt(ctx, "do the thing")
//
// Applications that need the foundation's lower-level event stream or
// raw lifecycle observability can import
// [github.com/latebit-io/nib/agent] directly — kit is the curated path,
// not the only path.
package kit

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/latebit-io/nib/agent"
	agentevent "github.com/latebit-io/nib/agent/event"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit/event"
	"github.com/latebit-io/nib/kit/eventbus"
)

// Tool is the foundation [agent.Tool] re-exported under kit. Consumers
// import only "kit" to declare tools.
type Tool = agent.Tool

// ToolResult is the foundation [agent.ToolResult] re-exported under kit.
type ToolResult = agent.ToolResult

// Hooks is the foundation [agent.Hooks] re-exported under kit.
type Hooks = agent.Hooks

// BeforeToolCallInput is the foundation [agent.BeforeToolCallInput]
// re-exported under kit.
type BeforeToolCallInput = agent.BeforeToolCallInput

// BeforeToolCallResult is the foundation [agent.BeforeToolCallResult]
// re-exported under kit.
type BeforeToolCallResult = agent.BeforeToolCallResult

// AfterToolCallInput is the foundation [agent.AfterToolCallInput]
// re-exported under kit.
type AfterToolCallInput = agent.AfterToolCallInput

// AfterToolCallResult is the foundation [agent.AfterToolCallResult]
// re-exported under kit.
type AfterToolCallResult = agent.AfterToolCallResult

// TruncationInput is the foundation [agent.TruncationInput]
// re-exported under kit.
type TruncationInput = agent.TruncationInput

// TruncationResult is the foundation [agent.TruncationResult]
// re-exported under kit.
type TruncationResult = agent.TruncationResult

// State is the foundation [agent.State] re-exported under kit.
type State = agent.State

// RunResult is what [Agent.Run] returns.
type RunResult struct {
	// Done is the run's outcome, as published to subscribers.
	Done event.AgentDone
	// Messages is the full conversation transcript at run end.
	Messages []llm.Message
}

// ErrInvalidOptions wraps the foundation's [agent.ErrInvalidOptions] so
// kit consumers can errors.Is against a kit-namespaced sentinel without
// importing the foundation package.
var ErrInvalidOptions = agent.ErrInvalidOptions

// ErrRunInProgress wraps the foundation's [agent.ErrRunInProgress].
var ErrRunInProgress = agent.ErrRunInProgress

// ErrClosed is the foundation's [agent.ErrClosed]: returned by
// [Agent.Prompt], [Agent.PromptWithMessages], [Agent.Run] and
// [Agent.Subscribe] after [Agent.Close]. Other methods stay safe to call.
var ErrClosed = agent.ErrClosed

// Config bundles the construction-time inputs for [New]. Provider is
// required; everything else is optional. Observe events via
// [Agent.Subscribe], which returns a [Subscription] whose
// [Subscription.Events] channel delivers the agent's event stream.
type Config struct {
	// Provider is the LLM provider used for every turn. Required.
	Provider llm.Provider

	// SystemPrompt is the system message prepended to every run's
	// transcript. Empty means "no system message." Application layers
	// compose their prompt and pass the result here, or leave empty
	// and drive runs through [Agent.PromptWithMessages] instead.
	SystemPrompt string

	// Toolset is the bundle of tools, hooks, and slash commands the
	// composition root selected. Build it as a [Toolset] literal (see the
	// package doc); [Merge] combines several bundles in priority order
	// when a root assembles more than one. Tools earlier in the merged
	// [Toolset.Tools] slice win on name collision (first-wins; later
	// duplicates are logged and dropped at [New] time). Hooks chain
	// across the merge per each field's documented semantics.
	// Commands are concatenated; precedence-based dedup happens at
	// [command.Registry] registration time, not here.
	Toolset Toolset

	// MaxTurns caps LLM turns taken without an intervening user message
	// (see [agent.Options.MaxTurns]); hitting it publishes
	// [event.AgentMaxTurns]. Zero means unlimited; negative is rejected.
	MaxTurns int

	// ParallelTools admits tools, by name, to run together when the model
	// calls several in a row; see [agent.Options.ParallelTools]. Nil runs
	// every call on its own.
	ParallelTools func(name string) bool
}

// Agent is the kit-level handle on a running agent. Drive it with
// [Agent.Prompt] / [Agent.Reply] / [Agent.Cancel] or [Agent.Run]; observe
// via [Agent.Subscribe]. Goroutine-safe; release it with [Agent.Close].
type Agent struct {
	foundation       *agent.Agent
	foundationEvents chan agentevent.Event

	// bus fans every translated [event.Event] out to its registered
	// [Subscription]s. Created by [New], populated by the translator
	// goroutine via [eventbus.Bus.Publish], drained by each subscriber's
	// reader. The bus is the single writer to each subscription's
	// inbox channel; the [Agent] never writes to a consumer-facing
	// channel directly.
	bus *eventbus.Bus

	// done is closed by [Agent.Close] to signal the translator
	// goroutine to drain its remaining events and exit. The
	// translator's select listens on both [Agent.foundationEvents]
	// and done, so a closed done unblocks the translator even when
	// the foundation has stopped emitting.
	done chan struct{}

	// translatorDone is closed by [Agent.translateEvents] (via defer)
	// when the translator goroutine returns. [Agent.Close] blocks on
	// it after closing [Agent.done] so the sequencing
	// "translator stopped publishing, then close the bus" holds
	// without races.
	translatorDone chan struct{}

	// closeOnce guards [Agent.Close] so concurrent or repeated Close
	// calls do not double-close [Agent.done] (which would panic) or
	// double-Cancel the foundation (which is safe but pointless).
	closeOnce sync.Once
}

// New constructs an Agent from cfg. Returns [ErrInvalidOptions] wrapped
// with a specific reason when validation fails.
//
// Validation rules:
//   - cfg.Provider must be non-nil.
//   - Each entry in cfg.Toolset.Tools must satisfy the foundation's
//     tool rules (non-nil, non-empty Definition().Function.Name,
//     names unique).
//
// Consumers obtain event streams via [Agent.Subscribe] after New
// returns; the bus delivers from the next publish onward, so
// subscriptions registered before the first Prompt receive every
// event from the run.
//
// Side effect: New spawns a translator goroutine that drains the
// foundation's lifecycle events and publishes each as the closest
// [event] equivalent on the agent's internal bus. The goroutine lives
// until [Agent.Close] — the foundation never closes its own events
// channel, so without Close the translator and the foundation hold
// each other live until process exit. Callers that create short-lived
// agents (tests, daemons that spin up per-request agents) must Close
// to avoid leaking.
func New(cfg Config) (*Agent, error) {
	if cfg.Provider == nil {
		return nil, fmt.Errorf("%w: Provider is required", ErrInvalidOptions)
	}

	tools, err := deduplicateTools(cfg.Toolset.Tools)
	if err != nil {
		return nil, err
	}

	foundationEvents := make(chan agentevent.Event, 64)

	f, err := agent.New(agent.Options{
		Provider:      cfg.Provider,
		Events:        foundationEvents,
		SystemPrompt:  cfg.SystemPrompt,
		Tools:         tools,
		Hooks:         cfg.Toolset.Hooks,
		MaxTurns:      cfg.MaxTurns,
		ParallelTools: cfg.ParallelTools,
	})
	if err != nil {
		return nil, err
	}

	a := &Agent{
		foundation:       f,
		foundationEvents: foundationEvents,
		bus:              eventbus.New(isStreamingEvent),
		done:             make(chan struct{}),
		translatorDone:   make(chan struct{}),
	}

	go a.translateEvents()
	return a, nil
}

// Subscribe registers a new subscriber for this agent's event stream
// and returns a [Subscription] whose inbox receives events from the
// next [eventbus.Bus.Publish] onward.
//
// Default [SubscribeOptions] (zero value) preserve the legacy
// single-consumer channel semantics: streaming events drop on a full
// inbox; control events block the publisher until the inbox accepts.
// Override the options for subscribers with different needs (a probe
// that can tolerate event loss on every class; a recorder that needs
// strict streaming-event delivery).
//
// Subscribe returns [ErrClosed] if the agent has been closed and
// [eventbus.ErrInvalidPolicy] for unsupported policy values. Late
// subscribers (registered after some events were already published)
// receive only events published after their subscribe call; the bus
// does not journal.
func (a *Agent) Subscribe(opts SubscribeOptions) (*Subscription, error) {
	sub, err := a.bus.Subscribe(toEventBusOptions(opts))
	if errors.Is(err, eventbus.ErrClosed) {
		return nil, ErrClosed
	}
	if err != nil {
		return nil, err
	}
	return &Subscription{inner: sub}, nil
}

// Prompt starts a new run with the given user message. Returns
// [ErrRunInProgress] when a run is active and [ErrClosed] after Close.
// Progress flows to subscribers; [Agent.WaitForIdle] blocks until the end.
func (a *Agent) Prompt(ctx context.Context, content string) error {
	return a.foundation.Prompt(ctx, content)
}

// PromptWithMessages starts a new run from a caller-supplied transcript
// instead of the [system?, user] slice [Agent.Prompt] builds. Same
// lifecycle and errors as [Agent.Prompt].
func (a *Agent) PromptWithMessages(ctx context.Context, messages []llm.Message) error {
	return a.foundation.PromptWithMessages(ctx, messages)
}

// Run starts a one-shot run from messages and blocks until it ends; see
// [agent.Agent.Run] for the result and error contract. Subscribers still
// receive the run's events. Returns [ErrClosed] on a closed agent.
func (a *Agent) Run(ctx context.Context, messages []llm.Message) (RunResult, error) {
	end, err := a.foundation.Run(ctx, messages)
	return RunResult{Done: doneFrom(end), Messages: end.Messages}, err
}

// Reply queues a developer follow-up for the active run. Returns true
// when the message was accepted, false when no run is active or the
// reply queue is full. The reply queue is a buffered channel of
// capacity 1 inside the foundation.
func (a *Agent) Reply(ctx context.Context, content string) bool {
	return a.foundation.Reply(ctx, content)
}

// Cancel stops the current run; a no-op when none is active. Subscribers
// receive an [event.AgentDone] with Success=false once the loop unwinds;
// [Agent.WaitForIdle] blocks until then.
func (a *Agent) Cancel() {
	a.foundation.Abort()
}

// State returns a read-only snapshot of the agent's current state.
// Slices and maps in the result are safe to read without further
// synchronization; modifying them does not affect the agent.
func (a *Agent) State() State {
	return a.foundation.State()
}

// ReplaceMessages overwrites the saved transcript with msgs. Returns
// the foundation's [agent.ErrRunInProgress] if a run is active —
// callers must [Agent.Cancel] and [Agent.WaitForIdle] before
// replacing. The slice is cloned; the caller retains ownership of
// the input.
//
// Surfaced for out-of-band compaction and history reset by
// kit consumers. Most consumers should not call this directly —
// prefer driving compaction through the run loop's TransformContext
// hook. The exception is user-triggered between-turn rewrites
// (e.g., a /clear or /compact command) where the consumer wants the
// next resume to start from a different transcript.
func (a *Agent) ReplaceMessages(msgs []llm.Message) error {
	return a.foundation.ReplaceMessages(msgs)
}

// WaitForIdle blocks until the current run (if any) has finished and
// the foundation goroutine has unwound. Returns immediately when no
// run is active.
func (a *Agent) WaitForIdle() {
	a.foundation.WaitForIdle()
}

// Close gracefully shuts down the agent and releases the translator
// goroutine [New] spawned. Idempotent — repeated Close calls and
// concurrent Close calls coalesce through [sync.Once].
//
// Order of operations:
//
//  1. Mark the agent closed so subsequent [Agent.Prompt] and
//     [Agent.PromptWithMessages] return [ErrClosed].
//  2. [agent.Agent.Abort] any in-flight run; wait for the foundation
//     goroutine to unwind via [agent.Agent.WaitForIdle]. The
//     foundation emits its final [agentevent.AgentEnd] during this
//     unwind, which the translator receives and forwards as
//     [event.AgentDone] before exiting.
//  3. Close the done channel; the translator drains any remaining
//     buffered foundation events and returns.
//
// After Close: [Agent.Reply] returns false (no active run),
// [Agent.Cancel] is a no-op, [Agent.WaitForIdle] returns immediately,
// [Agent.State] returns the foundation's final snapshot. Every
// [Subscription] obtained via [Agent.Subscribe] has its inbox closed
// by the bus, so `for ev := range sub.Events()` loops exit naturally
// after draining any buffered tail.
//
// Close blocks until the foundation has fully unwound, the translator
// goroutine has exited, and the bus has closed every subscription
// inbox. Callers that need force-quit semantics should not rely on
// Close — they should abandon the agent and accept the leak.
//
// Synchronous shutdown is the contract subscribers rely on: a reader
// goroutine that ranges over a [Subscription.Events] channel
// terminates cleanly when Close returns, with no risk of a late
// publish racing the channel close.
func (a *Agent) Close() {
	a.closeOnce.Do(func() {
		// The foundation refuses new runs and aborts the current one under
		// one lock, so no run can start behind the shutdown. The aborted
		// run's AgentDone reports failure.
		a.foundation.Close()
		a.foundation.WaitForIdle()
		close(a.done)
		// Wait for translateEvents to drain remaining foundation
		// events and exit. Bus is still open during this drain so
		// the final [event.AgentDone] reaches every subscriber.
		<-a.translatorDone
		// Close the bus. Signals every subscription's done channel,
		// waits for in-flight deliveries to drain, then closes every
		// inbox so reader loops exit. After Bus.Close returns, no
		// further writes happen to any subscription channel.
		a.bus.Close()
	})
}
