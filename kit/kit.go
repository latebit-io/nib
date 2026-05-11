// Package kit is the curated library surface for building agents on the
// nib foundation. It wraps [github.com/latebit-io/nib/agent] (the bare
// loop) with a small facade that re-exposes the foundation's tool and
// hook contracts under the kit namespace and translates the foundation's
// loop-lifecycle events into the consumer-facing
// [github.com/latebit-io/nib/kit/event] vocabulary.
//
// The intended consumer pattern: each plug-in package exposes a
// Plugins() [Toolset] function; the composition root merges them.
// Events flow through a fan-out bus — register a [Subscription] via
// [Agent.Subscribe] to observe them.
//
//	a, err := kit.New(kit.Config{
//	    Provider: provider,
//	    Toolset:  kit.Merge(builtin.Plugins(), coding.Plugins(opts)),
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
	"sync/atomic"

	"github.com/latebit-io/nib/agent"
	agentevent "github.com/latebit-io/nib/agent/event"
	"github.com/latebit-io/nib/ai/llm"
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

	// Toolset is the merged plug-in bundle: tools, hooks, and slash
	// commands. Compose with [Merge]: each plug-in package exposes a
	// Plugins() [Toolset] function; the composition root merges them
	// in priority order. Tools registered earlier in the merged
	// [Toolset.Tools] slice win on name collision (first-wins; later
	// duplicates are logged and dropped at [New] time). Hooks chain
	// across the merge per each field's documented semantics.
	// Commands are concatenated; precedence-based dedup happens at
	// [command.Registry] registration time, not here.
	Toolset Toolset
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
// drive with [Agent.Prompt] / [Agent.Reply] / [Agent.Cancel]; observe by
// registering a [Subscription] via [Agent.Subscribe]. Release with
// [Agent.Close] when the agent is no longer needed — without it the
// translator goroutine and the foundation hold each other live until
// process exit.
//
// Concurrency: every method on Agent is safe to call from any goroutine.
// The wrapped foundation serializes its loop on a single goroutine.
type Agent struct {
	foundation       *agent.Agent
	foundationEvents chan agentevent.Event

	// bus fans every translated [event.Event] out to its registered
	// [Subscription]s. Created by [New], populated by the translator
	// goroutine via [bus.publish], drained by each subscriber's
	// reader. The bus is the single writer to each subscription's
	// inbox channel; the [Agent] never writes to a consumer-facing
	// channel directly.
	bus *bus

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
	// it after closing [Agent.done] so the sequencing
	// "translator stopped publishing → safe to bus.close" holds
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

	tools := deduplicateTools(cfg.Toolset.Tools)

	foundationEvents := make(chan agentevent.Event, 64)

	f, err := agent.New(agent.Options{
		Provider:     cfg.Provider,
		Events:       foundationEvents,
		SystemPrompt: cfg.SystemPrompt,
		Tools:        tools,
		Hooks:        cfg.Toolset.Hooks,
	})
	if err != nil {
		return nil, err
	}

	a := &Agent{
		foundation:       f,
		foundationEvents: foundationEvents,
		bus:              newBus(),
		done:             make(chan struct{}),
		translatorDone:   make(chan struct{}),
	}

	go a.translateEvents()
	return a, nil
}

// Subscribe registers a new subscriber for this agent's event stream
// and returns a [Subscription] whose inbox receives events from the
// next [bus.publish] onward.
//
// Default [SubscribeOptions] (zero value) preserve the legacy
// single-consumer channel semantics: streaming events drop on a full
// inbox; control events block the publisher until the inbox accepts.
// Override the options for subscribers with different needs (a probe
// that can tolerate event loss on every class; a recorder that needs
// strict streaming-event delivery).
//
// Subscribe returns [ErrClosed] if the agent has been closed. Late
// subscribers (registered after some events were already published)
// receive only events published after their subscribe call; the bus
// does not journal.
func (a *Agent) Subscribe(opts SubscribeOptions) (*Subscription, error) {
	if atomic.LoadUint32(&a.closed) == 1 {
		return nil, ErrClosed
	}
	return a.bus.subscribe(opts)
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
// [Agent.State] returns the foundation's final snapshot. The legacy
// [Config.Events] channel is NOT closed by Close — that channel
// belongs to the caller and may be reused for other producers. Every
// [Subscription] obtained via [Agent.Subscribe] has its inbox closed
// by the bus, so reader loops exit naturally.
//
// Close blocks until the foundation has fully unwound, the translator
// goroutine has exited, the bus has closed every subscription inbox,
// and any legacy [Config.Events] forwarder has drained. Callers that
// need force-quit semantics should not rely on Close — they should
// abandon the agent and accept the leak.
//
// Synchronous shutdown is the contract callers rely on when they own
// the consumer channel: closing it right after Close returns must
// not race a still-draining writer.
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
		// Wait for translateEvents to drain remaining foundation
		// events and exit. Bus is still open during this drain so
		// the final [event.AgentDone] reaches every subscriber.
		<-a.translatorDone
		// Close the bus. Signals every subscription's done channel,
		// waits for in-flight deliveries to drain, then closes every
		// inbox so reader loops exit. After bus.close returns, no
		// further writes happen to any subscription channel.
		a.bus.close()
	})
}
