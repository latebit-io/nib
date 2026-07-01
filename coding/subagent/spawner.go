// Package subagent runs a child coding agent from an
// [agentdef.Definition]: it builds an [agent.Agent] constrained to the
// definition's tool grants and model, runs it headless to completion on
// a one-shot task, and returns the result. Each definition is exposed to
// a parent agent as an `agent_<name>` tool (see [AdaptTool]), mirroring
// how skills become `skill_<name>` tools.
//
// v1 scope and limitations (see /nib/plans/m4-subagent-engine.md):
//   - The agent definition's body becomes the child's persona: it is
//     injected as a high-salience section at the top of the coding system
//     prompt via [agent.NewOptions.SystemPromptPersona], augmenting (not
//     replacing) nib's operational scaffolding. The run goal is just the
//     task.
//   - Tool grants constrain BOTH the extra tools (MCP, skills) via
//     [grantFilter] AND the built-in tools via
//     [agent.NewOptions.BuiltinToolGrants] — the child's builtins are
//     dropped or (for bash) per-command gated per the definition's
//     grants, translated from CC to nib tool names. CC names that nib
//     has no equivalent for map 1:1 only (Edit→edit_file; apply_patch /
//     replace_file have no CC name, so an `Edit` grant does not admit
//     them).
//   - maxTurns IS honored (via [agent.NewOptions.MaxTurns]): a child that
//     reaches its turn cap ends the run. effort IS honored by building the
//     child's provider with the requested reasoning effort (via
//     ProviderFor), subject to provider support — OpenAI-style providers
//     apply it today; the Anthropic provider ignores it until
//     extended-thinking support lands. TaskTokenBudget also caps the child.
//   - The child shares the parent's working tree. isolation:worktree is
//     a later vertical.
package subagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	upagent "github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/agent"
	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/coding/headless"
	"github.com/latebit-io/nib/kit/agentdef"
	"github.com/latebit-io/nib/kit/hookmap"
	"github.com/latebit-io/nib/kit/toolperm"
)

// runFunc builds and runs a child agent to completion, forwarding the
// child's events to onEvent (nil to discard). It is a field on [Spawner]
// so the orchestration can be tested without a real LLM; production wires
// [realRun].
type runFunc func(ctx context.Context, p llm.Provider, ws *headless.DiskWorkspace, opts *agent.NewOptions, tools []agent.Tool, goal string, onEvent func(event.Event)) (headless.Result, error)

// worktreeFactory creates an isolated git worktree of repo and returns
// its root plus a cleanup func that removes it. A field on [Spawner] so
// the git lifecycle can be faked in unit tests; production wires
// [gitWorktree].
type worktreeFactory func(ctx context.Context, repo string) (root string, cleanup func(), err error)

// Spawner constructs and runs child agents. Safe for sequential use; each
// Spawn builds its own single-run [agent.Agent].
type Spawner struct {
	workspace   *headless.DiskWorkspace
	provider    func() llm.Provider
	providerFor func(model, effort string) (llm.Provider, error)
	baseTools   []agent.Tool
	budget      int
	onEvent     func(event.Event)
	run         runFunc
	worktree    worktreeFactory
	// onSubagentStop fires after a child run returns — the parent
	// session's SubagentStop emit point. Nil discards it.
	onSubagentStop func(ctx context.Context)
}

// Options configures a [Spawner].
type Options struct {
	// Workspace is the disk workspace children run against (the project
	// root). Required.
	Workspace *headless.DiskWorkspace
	// Provider returns the CURRENT default LLM provider, used when a
	// definition does not override the model. It is a getter (not a value)
	// so a child resolves the live provider at spawn time — staying in
	// sync with credential/model switches rather than capturing a stale or
	// nil startup provider. Required.
	Provider func() llm.Provider
	// ProviderFor resolves a provider bound to a specific model id and
	// reasoning effort, for a definition's model / effort override. Either
	// argument may be empty to keep the base value (empty model keeps the
	// parent model; empty effort keeps the provider default). Optional — a
	// definition that requests a model or effort errors if this is nil.
	ProviderFor func(model, effort string) (llm.Provider, error)
	// BaseTools is the full set of extra tools a child may receive (MCP,
	// skills). Filtered per definition by its grants.
	BaseTools []agent.Tool
	// TokenBudget is the per-run token cap inherited by children (0 =
	// disabled, like the parent).
	TokenBudget int
	// OnEvent receives child-agent progress events for display (e.g.
	// forwarded into the parent's event stream). Optional; nil discards
	// progress. The Spawner name-prefixes events before calling it.
	OnEvent func(event.Event)
	// OnSubagentStop fires after a spawned child finishes (success or
	// error) — the parent session's SubagentStop hook emit point. The
	// wiring layer passes the parent dispatcher's SubagentStop. Optional;
	// nil discards it. Not fired when Spawn fails before the child runs.
	OnSubagentStop func(ctx context.Context)
}

// New builds a Spawner from Options.
func New(o Options) *Spawner {
	return &Spawner{
		workspace:      o.Workspace,
		provider:       o.Provider,
		providerFor:    o.ProviderFor,
		baseTools:      o.BaseTools,
		budget:         o.TokenBudget,
		onEvent:        o.OnEvent,
		run:            realRun,
		worktree:       gitWorktree,
		onSubagentStop: o.OnSubagentStop,
	}
}

// progressSink returns the per-spawn child-event handler: it forwards a
// child's tool calls to the parent display, prefixed with the subagent
// name so they read as nested progress. Returns nil when no display sink
// is configured (the run then discards child events). Only tool calls are
// forwarded — token/status spam would drown the parent transcript.
func (s *Spawner) progressSink(name string) func(event.Event) {
	if s.onEvent == nil {
		return nil
	}
	return func(ev event.Event) {
		if tc, ok := ev.(event.AgentToolCall); ok {
			s.onEvent(event.AgentToolCall{Name: "▸ " + name + ": " + tc.Name, Args: tc.Args})
		}
	}
}

// Spawn runs def against task to completion and returns the result.
func (s *Spawner) Spawn(ctx context.Context, def agentdef.Definition, task string) (headless.Result, error) {
	// Translate the definition's CC-named tool grants into a nib-keyed
	// matcher so agent.New can gate the child's BUILT-IN tools (drop the
	// ungranted, per-command gate bash). Malformed grants refuse the spawn
	// rather than run a child with a half-applied boundary.
	builtinGrants, err := nibBuiltinGrants(def)
	if err != nil {
		return headless.Result{}, fmt.Errorf("subagent %q: %w", def.Name, err)
	}

	var prov llm.Provider
	// A model OR effort override needs a provider built for it (both are
	// fixed at provider construction). Empty model keeps the parent model;
	// empty effort keeps the provider default.
	if def.Model != "" || def.Effort != "" {
		if s.providerFor == nil {
			return headless.Result{}, fmt.Errorf("subagent %q requests a model/effort override but no provider resolver is configured", def.Name)
		}
		p, err := s.providerFor(def.Model, def.Effort)
		if err != nil {
			return headless.Result{}, fmt.Errorf("subagent %q: resolve provider (model %q, effort %q): %w", def.Name, def.Model, def.Effort, err)
		}
		prov = p
	} else if s.provider != nil {
		prov = s.provider()
	}
	if prov == nil {
		return headless.Result{}, fmt.Errorf("subagent %q: no LLM provider available", def.Name)
	}

	tools := grantFilter(s.baseTools, def.Permissions())
	opts := &agent.NewOptions{
		Interaction:         agent.Headless,
		Terse:               true,
		TaskTokenBudget:     s.budget,
		BuiltinToolGrants:   builtinGrants,
		SystemPromptPersona: strings.TrimSpace(def.SystemPrompt),
		MaxTurns:            def.MaxTurns,
	}

	// isolation:worktree runs the child in a throwaway git worktree so its
	// edits never touch the parent tree. v1 semantics: the run is fully
	// sandboxed and the worktree (with any edits) is removed on cleanup —
	// the child's Summary and changed-file list are returned, but the file
	// changes themselves are NOT merged back. Auto-merge/keep is a future
	// option. Requires the project to be a git repo.
	ws := s.workspace
	if def.Isolation == agentdef.IsolationWorktree {
		root, cleanup, err := s.worktree(ctx, s.workspace.ProjectRoot())
		if err != nil {
			return headless.Result{}, fmt.Errorf("subagent %q: isolated worktree: %w", def.Name, err)
		}
		defer cleanup()
		ws = headless.NewDiskWorkspace(root)
	}

	res, err := s.run(ctx, prov, ws, opts, tools, task, s.progressSink(def.Name))
	// SubagentStop fires once the child has run and returned, regardless
	// of success — the child stopped either way. Detach from ctx: a
	// canceled child (parent cancellation) must not stop the hook from
	// firing, mirroring the parent Stop emit point.
	if s.onSubagentStop != nil {
		s.onSubagentStop(context.WithoutCancel(ctx))
	}
	return res, err
}

// grantFilter selects the extra tools a child may use under its grants.
// With no allow list the child inherits every extra tool (minus any
// outright deny); with an allow list it keeps only granted tools. An
// argument-scoped grant like Bash(git *) still admits the Bash tool here;
// per-command enforcement of those args happens on the built-in bash tool
// via [nibBuiltinGrants] → [agent.NewOptions.BuiltinToolGrants].
func grantFilter(tools []agent.Tool, perm *toolperm.Matcher) []agent.Tool {
	hasAllow := perm.HasAllowList()
	var out []agent.Tool
	for _, t := range tools {
		name := t.Definition().Function.Name
		if perm.DeniesTool(name) {
			continue
		}
		if hasAllow && !perm.GrantsTool(name) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// nibBuiltinGrants translates a definition's CC-named tool grants into a
// nib-keyed matcher for [agent.NewOptions.BuiltinToolGrants], gating the
// child's built-in tools by the CC-authored `tools:` / `disallowedTools:`.
// Returns (nil, nil) when the definition imposes no restriction (the child
// keeps every builtin). Malformed grants are an error — refuse the spawn
// rather than run with a half-applied boundary.
//
// CC→nib name mapping is via [hookmap] (Read→read_file, Bash→bash, …).
// Tokens with no CC mapping — nib-native names, MCP/skill names — pass
// through unchanged so they still match their targets.
func nibBuiltinGrants(def agentdef.Definition) (*toolperm.Matcher, error) {
	if len(def.AllowedTools) == 0 && len(def.DisallowedTools) == 0 {
		return nil, nil
	}
	allow, aerr := nibRules(def.AllowedTools)
	deny, derr := nibRules(def.DisallowedTools)
	if err := errors.Join(aerr, derr); err != nil {
		return nil, fmt.Errorf("invalid tool grants: %w", err)
	}
	return toolperm.New(allow, deny), nil
}

// nibRules parses CC grant tokens, rewriting each rule's tool name to its
// nib built-in equivalent where one exists.
func nibRules(tokens []string) ([]toolperm.Rule, error) {
	rules, err := toolperm.ParseField(tokens)
	if err != nil {
		return nil, err
	}
	for i := range rules {
		if nib, ok := hookmap.NibName(rules[i].Tool); ok {
			rules[i].Tool = nib
		}
	}
	return rules, nil
}

// realRun builds the child agent, forwards its events into the headless
// runner (and to onEvent for parent-side progress display), and blocks
// until the run completes.
func realRun(ctx context.Context, p llm.Provider, ws *headless.DiskWorkspace, opts *agent.NewOptions, tools []agent.Tool, goal string, onEvent func(event.Event)) (headless.Result, error) {
	ag := agent.New(p, ws, opts, tools...)
	defer ag.Close()

	sub, err := ag.Subscribe(agent.SubscribeOptions{BufferSize: 128})
	if err != nil {
		return headless.Result{}, fmt.Errorf("subagent: subscribe: %w", err)
	}
	events := make(chan event.Event, 128)
	go func() {
		defer close(events)
		for ev := range sub.Events() {
			if onEvent != nil {
				onEvent(ev)
			}
			events <- ev
		}
	}()

	res := headless.NewRunner(ag, ws, events, io.Discard, false).Run(ctx, goal, nil)
	if res == nil {
		return headless.Result{}, fmt.Errorf("subagent: runner returned no result")
	}
	return *res, nil
}

// ensure spawnTool satisfies the agent tool port at compile time.
var _ upagent.Tool = spawnTool{}
