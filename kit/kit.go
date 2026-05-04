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
//	    Tools:    coreTools,
//	    Hooks:    coreHooks,
//	    Toolsets:  []kit.Toolset{memoryBundle, mcpBundle},
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
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

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

// ErrClosed is returned by [Agent.Prompt] and [Agent.PromptWithMessages]
// after [Agent.Close] has been called. Reply, Cancel, State, and
// WaitForIdle remain safe after Close — they delegate to the foundation
// which is already idle by then.
var ErrClosed = errors.New("kit: agent is closed")

// Config bundles the construction-time inputs for [New]. Provider and
// Events are required; everything else is optional.
type Config struct {
	// Provider is the LLM provider used for every turn. Required.
	Provider llm.Provider

	// Events is the channel the agent writes consumer-facing events to.
	// Required. The caller MUST drain this channel.
	//
	// Send semantics:
	//
	//   - High-volume streaming events ([event.AgentToken],
	//     [event.AgentTurnUsage], [event.AgentInputEstimate]) drop
	//     when the channel is full. They are individually
	//     replaceable — a missing token is visually closed by the
	//     next one; a missing usage tick is recovered from the next.
	//
	//   - Control events ([event.AgentDone], [event.AgentError],
	//     [event.AgentToolCall], [event.AgentWaiting],
	//     [event.AgentStatus], [event.AgentCompacted]) block until
	//     the consumer drains. Guaranteed delivery — these carry
	//     lifecycle and failure signals an event-driven consumer
	//     cannot reconstruct from later events.
	//
	// A consumer that stops draining will wedge the kit translator
	// goroutine on its next blocking send. Because [Agent.Close]
	// waits synchronously for the translator to drain remaining
	// foundation events and exit (the translatorDone barrier),
	// Close itself will hang indefinitely against a wedged consumer
	// — Close cannot rescue a stuck translator without violating
	// the "no further writes to Events after Close returns"
	// guarantee callers rely on for safe channel cleanup. Size the
	// channel generously (64+ recommended) and keep the consumer
	// draining for the lifetime of the agent; close the channel
	// only AFTER Close returns.
	Events chan<- event.Event

	// SystemPrompt is the system message prepended to every run's
	// transcript. Empty means "no system message." Application layers
	// compose their prompt and pass the result here, or leave empty
	// and drive runs through [Agent.PromptWithMessages] instead.
	SystemPrompt string

	// Tools are the tool implementations registered directly. These
	// appear BEFORE tools from [Toolsets] in the flattened list, giving
	// them builtin precedence when names collide (first wins).
	// Each tool's Definition() schema is advertised to the LLM; tool
	// calls route to Execute(). Tools share the agent's lifetime and
	// must be safe to invoke concurrently.
	Tools []Tool

	// Hooks are hooks registered directly. These fire BEFORE any hooks
	// contributed by [Toolsets]. Nil disables a given hook; the agent
	// skips it without ceremony. Hooks run synchronously on the loop
	// goroutine — long-running work should be dispatched off the
	// hook's call stack.
	Hooks Hooks

	// Toolsets are composable tool+hook bundles merged in order after
	// the direct [Tools] and [Hooks] fields. Use for library-provided
	// bundles that ship their own hooks alongside their tools.
	// Duplicate tool names across toolsets and direct tools are
	// resolved at [New] time: first registration wins; later
	// duplicates are logged and dropped.
	Toolsets []Toolset
}

// runOutcome carries the failure bit for a single run from
// [Agent.Prompt] (which allocates it) through the translator (which
// binds it on AgentStart, may set it on Error/Cancel, and consumes it
// on AgentEnd to emit [event.AgentDone]). Per-run rather than shared
// so concurrent Cancels and Prompts cannot poison each other's runs.
type runOutcome struct {
	// unsuccess flips to 1 when the run fails. Atomic because
	// [Agent.Cancel] and the translator goroutine both write it
	// (Cancel marks the bound outcome; translator marks on Error).
	unsuccess uint32
}

// Agent is the kit-level handle on a running agent. Construct with [New];
// drive with [Agent.Prompt] / [Agent.Reply] / [Agent.Cancel]; observe via
// the events channel from [Config]. Release with [Agent.Close] when the
// agent is no longer needed — without it the translator goroutine and
// the foundation hold each other live until process exit.
//
// Concurrency: every method on Agent is safe to call from any goroutine.
// The wrapped foundation serializes its loop on a single goroutine.
type Agent struct {
	foundation       *agent.Agent
	foundationEvents chan agentevent.Event
	consumerEvents   chan<- event.Event

	// outcomeMu guards [Agent.pendingOutcome] and [Agent.currentOutcome].
	// The two pointers carry per-run failure state across goroutines:
	// [Agent.Prompt] (user goroutine) creates an outcome and parks it
	// in pending; the translator binds it to the run on AgentStart by
	// moving pending → current; the translator reads it on AgentEnd to
	// emit [event.AgentDone]; [Agent.Cancel] marks whichever outcome is
	// in flight (current preferred, pending fallback) so a Cancel that
	// races AgentStart still poisons the right run.
	//
	// Per-run state replaces the previous shared atomic flag. The
	// shared flag was racy because (a) WaitForIdle returns on
	// foundation exit but the translator may still have the previous
	// run's AgentEnd buffered, and (b) Cancel and the translator's
	// AgentStart-reset are unordered, so a fast Prompt-then-Cancel
	// could have the AgentStart-reset clear a Cancel-set flag for the
	// run the user intended to cancel.
	outcomeMu      sync.Mutex
	pendingOutcome *runOutcome
	currentOutcome *runOutcome

	// closed is set by [Agent.Close] before it begins shutdown. Read
	// by [Agent.Prompt] and [Agent.PromptWithMessages] to refuse new
	// runs after Close. Atomic so the check is safe from any
	// goroutine without taking a lock on every Prompt.
	closed uint32

	// done is closed by [Agent.Close] to signal the translator
	// goroutine to drain its remaining events and exit. The
	// translator's select listens on both [Agent.foundationEvents]
	// and done, so a closed done unblocks the translator even when
	// the foundation has stopped emitting.
	done chan struct{}

	// translatorDone is closed by [Agent.translateEvents] (via defer)
	// when the translator goroutine returns. [Agent.Close] blocks on
	// it after closing [Agent.done] so callers receive a synchronous
	// "translator has stopped writing to consumerEvents" signal.
	// Without this guarantee, a caller that closes the consumer
	// channel right after Close returns can race a translator still
	// draining buffered foundation events and panic on send-on-closed.
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
//   - cfg.Events must be non-nil.
//   - Each entry in cfg.Tools must satisfy the foundation's tool rules
//     (non-nil, non-empty Definition().Function.Name, names unique).
//
// Side effect: New spawns a translator goroutine that drains the
// foundation's lifecycle events and re-emits each as the closest
// [event] equivalent through cfg.Events. The goroutine lives until
// [Agent.Close] is called — the foundation never closes its own
// events channel, so without Close the translator and the foundation
// hold each other live until process exit. Callers that create
// short-lived agents (tests, daemons that spin up per-request agents)
// must Close to avoid leaking.
func New(cfg Config) (*Agent, error) {
	if cfg.Provider == nil {
		return nil, fmt.Errorf("%w: Provider is required", ErrInvalidOptions)
	}
	if cfg.Events == nil {
		return nil, fmt.Errorf("%w: Events is required", ErrInvalidOptions)
	}

	merged := Merge(
		append([]Toolset{{Tools: cfg.Tools, Hooks: cfg.Hooks}}, cfg.Toolsets...)...,
	)
	tools := deduplicateTools(merged.Tools)

	foundationEvents := make(chan agentevent.Event, 64)

	f, err := agent.New(agent.Options{
		Provider:     cfg.Provider,
		Events:       foundationEvents,
		SystemPrompt: cfg.SystemPrompt,
		Tools:        tools,
		Hooks:        merged.Hooks,
	})
	if err != nil {
		return nil, err
	}

	a := &Agent{
		foundation:       f,
		foundationEvents: foundationEvents,
		consumerEvents:   cfg.Events,
		done:             make(chan struct{}),
		translatorDone:   make(chan struct{}),
	}

	go a.translateEvents()
	return a, nil
}

// Prompt starts a new run with the given user message. Returns
// [ErrRunInProgress] when a run is already active or [ErrClosed] when
// the agent has been closed. Lifecycle progress flows through the
// events channel; [Agent.WaitForIdle] blocks for run completion.
//
// Prompt does NOT touch a shared success flag. It parks a fresh
// [runOutcome] in [Agent.pendingOutcome] for the translator to bind
// to the run on the next [agentevent.AgentStart]. Per-run state
// instead of a shared flag eliminates the race where (a) WaitForIdle
// returns on foundation exit but the translator still has the
// previous run's AgentEnd buffered, or (b) a fast Prompt-then-Cancel
// has the translator's AgentStart-processing clear a Cancel-set
// flag.
func (a *Agent) Prompt(ctx context.Context, content string) error {
	if atomic.LoadUint32(&a.closed) == 1 {
		return ErrClosed
	}
	a.parkPendingOutcome()
	if err := a.foundation.Prompt(ctx, content); err != nil {
		a.discardPendingOutcome()
		return err
	}
	return nil
}

// PromptWithMessages starts a new run with a caller-supplied initial
// transcript instead of the [system?, user] slice [Agent.Prompt] builds
// from [Config.SystemPrompt]. Useful for applications that compose
// dynamic per-run transcripts (memory summary, context files, file
// content) alongside the user's goal.
//
// Same lifecycle, close-check, and per-run-outcome semantics as
// [Agent.Prompt].
func (a *Agent) PromptWithMessages(ctx context.Context, messages []llm.Message) error {
	if atomic.LoadUint32(&a.closed) == 1 {
		return ErrClosed
	}
	a.parkPendingOutcome()
	if err := a.foundation.PromptWithMessages(ctx, messages); err != nil {
		a.discardPendingOutcome()
		return err
	}
	return nil
}

// parkPendingOutcome installs a fresh outcome in [Agent.pendingOutcome]
// for the translator to bind on the next AgentStart. Replaces any
// previous pendingOutcome that did not get bound — happens when the
// foundation rejects a Prompt with [ErrRunInProgress] before the next
// successful Prompt is attempted (the unbound outcome is simply
// discarded).
func (a *Agent) parkPendingOutcome() {
	a.outcomeMu.Lock()
	a.pendingOutcome = &runOutcome{}
	a.outcomeMu.Unlock()
}

// discardPendingOutcome clears [Agent.pendingOutcome]. Called when the
// foundation rejects a Prompt — without it a stray pendingOutcome
// would bind to a future run's AgentStart and silently survive across
// the rejection.
func (a *Agent) discardPendingOutcome() {
	a.outcomeMu.Lock()
	a.pendingOutcome = nil
	a.outcomeMu.Unlock()
}

// Reply queues a developer follow-up for the active run. Returns true
// when the message was accepted, false when no run is active or the
// reply queue is full. The reply queue is a buffered channel of
// capacity 1 inside the foundation.
func (a *Agent) Reply(ctx context.Context, content string) bool {
	return a.foundation.Reply(ctx, content)
}

// Cancel stops the current run. Safe to call when no run is active —
// it is a no-op in that case. The events channel receives an
// [event.AgentDone] with Success=false once the loop unwinds; use
// [Agent.WaitForIdle] to block until that happens.
//
// Marks the in-flight run's outcome as unsuccess. Prefers
// [Agent.currentOutcome] (the translator has bound the run already)
// over [Agent.pendingOutcome] (the translator has not yet processed
// AgentStart for this run). If neither exists, Cancel does not mark
// anything — there is no run to fail.
func (a *Agent) Cancel() {
	a.markCurrentOrPendingUnsuccess()
	a.foundation.Abort()
}

// markCurrentOrPendingUnsuccess marks the in-flight run's outcome
// (current first, pending fallback) as unsuccess. Pendant to
// [runOutcome.unsuccess]'s atomic store: the lock guards which
// outcome we pick, the atomic guards the write itself so the
// translator can read it without holding outcomeMu on every event.
func (a *Agent) markCurrentOrPendingUnsuccess() {
	a.outcomeMu.Lock()
	target := a.currentOutcome
	if target == nil {
		target = a.pendingOutcome
	}
	a.outcomeMu.Unlock()
	if target != nil {
		atomic.StoreUint32(&target.unsuccess, 1)
	}
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
// [Agent.State] returns the foundation's final snapshot. The
// consumer's events channel is NOT closed by Close — that channel
// belongs to the caller and may be reused for other producers.
//
// Close blocks until the foundation has fully unwound AND the
// translator goroutine has exited (no further writes to
// [Config.Events]). Callers that need force-quit semantics should
// not rely on Close — they should abandon the agent and accept the
// leak.
//
// Synchronous translator exit is the contract callers rely on when
// they own the consumer channel: closing it right after Close
// returns must not race a still-draining translator.
func (a *Agent) Close() {
	a.closeOnce.Do(func() {
		atomic.StoreUint32(&a.closed, 1)
		// Route through [Agent.Cancel] (not foundation.Abort directly)
		// so the kit-level unsuccess flag is set alongside the
		// foundation cancel — otherwise an in-flight run interrupted
		// by Close would surface as AgentDone(Success=true).
		a.Cancel()
		a.foundation.WaitForIdle()
		close(a.done)
		// Block until translateEvents has fully drained any buffered
		// foundation events and exited. If the consumer is no longer
		// draining [Config.Events], the translator wedges and Close
		// will hang — same hard contract as steady-state operation
		// (consumer must drain). Documented on Config.Events.
		<-a.translatorDone
	})
}
