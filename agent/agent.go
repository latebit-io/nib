// Package agent implements a generic, application-agnostic LLM agent loop.
//
// The agent owns the multi-turn conversation, dispatches tool calls, and
// emits lifecycle events on a typed channel. It does NOT know about file
// edits, validation, lint, memory, approval flow, prompts, or any other
// application concept — those live in the consuming application (in this
// repo, the `coding` module). Application logic plugs in via [Hooks] and
// custom [Tool] implementations.
//
// The design mirrors badlogic/pi-mono's `packages/agent`: tools are
// plugins, hooks own all app-specific extension, and the agent stays a
// few hundred lines of focused control flow.
//
// This file currently contains the public surface only. The loop body
// is filled in during phase 3 of the layering refactor; see
// /nib/plans/pi-mono-layering.md.
package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/latebit-io/nib/agent/event"
	"github.com/latebit-io/nib/ai/llm"
)

// Options bundles the construction-time inputs for [New]. Every field
// other than Provider and Events is optional.
type Options struct {
	// Provider is the LLM provider used for every turn. Required.
	Provider llm.Provider

	// Events is the channel the agent writes lifecycle events to.
	// Required. The caller must drain this channel; the agent blocks
	// briefly on control-flow events (limit specified by future
	// implementation) and drops streaming events when the channel is
	// full.
	Events chan<- event.Event

	// SystemPrompt is the system message prepended to every LLM call.
	// Empty means "no system prompt." Application layers compose their
	// prompt and pass the result here.
	SystemPrompt string

	// Tools are the tool implementations registered against this agent.
	// The agent advertises Definition() of each to the LLM and routes
	// tool calls to Execute(). Tools share the agent's lifetime.
	Tools []Tool

	// Hooks are the application-layer extension points. Nil disables a
	// given hook; the agent skips it without ceremony.
	Hooks Hooks
}

// Agent runs the multi-turn LLM loop. Construct with [New]; drive with
// [Agent.Prompt] / [Agent.Reply]; observe via the events channel from
// [Options].
//
// Concurrency: Agent serializes its own loop on a single goroutine.
// Public methods (Prompt, Reply, Abort, State) are safe to call from
// any goroutine. Hooks run on the loop goroutine and must not block
// indefinitely.
type Agent struct {
	provider     llm.Provider
	events       chan<- event.Event
	systemPrompt string
	tools        map[string]Tool
	toolDefs     []llm.ToolDef
	hooks        Hooks

	// Run-state fields (mutex, cancellation, transcript, etc.) are
	// added during phase 3 alongside the loop implementation.
}

// ErrInvalidOptions is returned by [New] when Options is missing a required
// field or contains an inconsistency that would produce an unusable agent
// (nil tool, empty tool name, duplicate tool name).
var ErrInvalidOptions = errors.New("agent: invalid options")

// New constructs an Agent from Options. Returns ErrInvalidOptions wrapped
// with a specific reason when validation fails.
//
// Validation rules:
//   - Provider must be non-nil.
//   - Events must be non-nil.
//   - Each entry in Tools must be non-nil.
//   - Each tool's Definition().Function.Name must be non-empty.
//   - Tool names must be unique. A duplicate is a configuration error;
//     silently overwriting would desync the tools map and toolDefs slice
//     (the LLM would see the duplicate while only one route exists).
func New(opts Options) (*Agent, error) {
	if opts.Provider == nil {
		return nil, fmt.Errorf("%w: Provider is required", ErrInvalidOptions)
	}
	if opts.Events == nil {
		return nil, fmt.Errorf("%w: Events is required", ErrInvalidOptions)
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
	}, nil
}

// Prompt starts a new run with the given user message. Blocks the caller
// only long enough to launch the loop goroutine; lifecycle progress flows
// through the events channel and a final completion blocks on
// [Agent.WaitForIdle] when callers need it.
//
// Returns an error if a run is already active. The current implementation
// is a stub — phase 3 fills in the loop.
func (a *Agent) Prompt(ctx context.Context, content string) error {
	_ = ctx
	_ = content
	panic("agent.Agent.Prompt: not yet implemented (phase 3)")
}

// Reply delivers a developer follow-up. Behavior depends on the agent's
// current state: when waiting between turns the message becomes the next
// user turn; when running it queues for injection after the current turn
// finishes. The boolean return indicates whether the reply was accepted.
//
// Phase 3 fills in the implementation.
func (a *Agent) Reply(ctx context.Context, content string) bool {
	_ = ctx
	_ = content
	panic("agent.Agent.Reply: not yet implemented (phase 3)")
}

// Abort cancels the current run. Safe to call when no run is active —
// it is a no-op in that case. The events channel receives an [event.AgentEnd]
// once the loop unwinds.
func (a *Agent) Abort() {
	panic("agent.Agent.Abort: not yet implemented (phase 3)")
}

// State returns a read-only snapshot of the agent's current state. Slices
// and maps in the result are safe to read without further synchronization;
// modifying them does NOT affect the agent.
func (a *Agent) State() State {
	panic("agent.Agent.State: not yet implemented (phase 3)")
}

// WaitForIdle blocks until the current run (if any) has finished and all
// loop goroutines have unwound. Returns immediately when no run is active.
//
// Phase 3 fills in the implementation.
func (a *Agent) WaitForIdle() {
	panic("agent.Agent.WaitForIdle: not yet implemented (phase 3)")
}
