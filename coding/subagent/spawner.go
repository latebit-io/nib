// Package subagent runs a child coding agent from an
// [agentdef.Definition]: it builds an [agent.Agent] constrained to the
// definition's tool grants and model, runs it headless to completion on
// a one-shot task, and returns the result. Each definition is exposed to
// a parent agent as an `agent_<name>` tool (see [AdaptTool]), mirroring
// how skills become `skill_<name>` tools.
//
// v1 scope and limitations (see /nib/plans/m4-subagent-engine.md):
//   - The agent definition's body is layered into the run GOAL, because
//     [agent.New] has no system-prompt override yet. A proper
//     NewOptions.SystemPrompt is the right fix (a core change).
//   - Tool grants filter the EXTRA tools passed to the child (MCP,
//     skills). Built-in tools (read/write/edit/bash/…) are registered
//     inside [agent.New] and cannot be filtered or per-command gated from
//     here; that needs a core agent option.
//   - maxTurns and effort are not yet honored ([agent.NewOptions] has no
//     such fields); TaskTokenBudget is the only run cap.
//   - The child shares the parent's working tree. isolation:worktree is
//     a later vertical.
package subagent

import (
	"context"
	"fmt"
	"io"
	"strings"

	upagent "github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/agent"
	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/coding/headless"
	"github.com/latebit-io/nib/kit/agentdef"
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
	providerFor func(model string) (llm.Provider, error)
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
	// ProviderFor resolves a provider bound to a specific model id, for a
	// definition's model override. Optional — a definition that requests a
	// model errors if this is nil.
	ProviderFor func(model string) (llm.Provider, error)
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
	// A definition's tool grants can be enforced on the EXTRA tools nib
	// passes the child, but not on the built-in tools (read/write/edit/
	// bash/…) that agent.New registers internally. So an advertised
	// restriction like `tools: Read` would not actually prevent
	// write_file/bash. Refuse to run a falsely-sandboxed child rather than
	// pretend the boundary holds; restrictions become honorable once
	// built-in tool gating (and CC→nib tool-name mapping) land.
	if len(def.AllowedTools) > 0 || len(def.DisallowedTools) > 0 {
		return headless.Result{}, fmt.Errorf("subagent %q declares tool restrictions (tools/disallowedTools) that nib cannot yet enforce on its built-in tools; remove them or wait for built-in tool gating", def.Name)
	}

	var prov llm.Provider
	if def.Model != "" {
		if s.providerFor == nil {
			return headless.Result{}, fmt.Errorf("subagent %q requests model %q but no model resolver is configured", def.Name, def.Model)
		}
		p, err := s.providerFor(def.Model)
		if err != nil {
			return headless.Result{}, fmt.Errorf("subagent %q: resolve model %q: %w", def.Name, def.Model, err)
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
		Interaction:     agent.Headless,
		Terse:           true,
		TaskTokenBudget: s.budget,
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

	res, err := s.run(ctx, prov, ws, opts, tools, composeGoal(def, task), s.progressSink(def.Name))
	// SubagentStop fires once the child has run and returned, regardless
	// of success — the child stopped either way.
	if s.onSubagentStop != nil {
		s.onSubagentStop(ctx)
	}
	return res, err
}

// grantFilter selects the extra tools a child may use under its grants.
// With no allow list the child inherits every extra tool (minus any
// outright deny); with an allow list it keeps only granted tools. An
// argument-scoped grant like Bash(git *) still admits the Bash tool —
// per-command enforcement of those args is a builtin-tool concern this
// layer cannot reach (see package doc).
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

// composeGoal layers the definition's system prompt into the run goal.
// This is the v1 stand-in for a real system-prompt override (see package
// doc): the child still runs under nib's coding system prompt, with the
// agent persona prepended to the task.
func composeGoal(def agentdef.Definition, task string) string {
	persona := strings.TrimSpace(def.SystemPrompt)
	if persona == "" {
		return task
	}
	return persona + "\n\n---\n\nTask:\n" + task
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
