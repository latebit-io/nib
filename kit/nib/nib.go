// Package nib assembles the base agent: the default set of kit blocks
// (memory, skills, AGENTS.md, MCP, extensions) behind explicit options.
// [kit.New] stays the minimal constructor underneath.
package nib

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/budget"
	"github.com/latebit-io/nib/kit/compact"
	"github.com/latebit-io/nib/kit/contextfile"
	"github.com/latebit-io/nib/kit/elicit"
	"github.com/latebit-io/nib/kit/extension"
	"github.com/latebit-io/nib/kit/mcp"
	"github.com/latebit-io/nib/kit/memory"
	"github.com/latebit-io/nib/kit/meter"
	"github.com/latebit-io/nib/kit/permission"
	"github.com/latebit-io/nib/kit/pluginhooks"
	"github.com/latebit-io/nib/kit/pluginstore"
	"github.com/latebit-io/nib/kit/skill"
	"github.com/latebit-io/nib/kit/subagent"
	memorytools "github.com/latebit-io/nib/kit/tools/memory"
	"github.com/latebit-io/nib/kit/truncation"
)

// Options selects what a session is assembled from. Only Dir is
// required; every other zero value leaves that part out or at its default.
type Options struct {
	// Dir is the session's working directory. Its repository root is
	// the project root that skills, AGENTS.md and .mcp.json load from.
	Dir string
	// SystemPrompt is the host's own prompt. Blocks append to it.
	SystemPrompt string
	// Toolset is what the host wires in itself. Its tools come first.
	Toolset kit.Toolset
	// Store, when set, adds the memory tools.
	Store memory.Store
	// MCP says where server definitions come from. An empty ProjectDir
	// means the project root; its servers still need the person's approval.
	MCP mcp.DiscoverOptions
	// MCPTokens are the person's sign-ins to HTTP servers. Nil means a
	// server that asks for one does not start.
	MCPTokens *mcp.TokenStore
	// Plugins are the installed plugins. Their skills load; a trusted
	// plugin's MCP servers, hooks and agents too. Nil means none.
	Plugins *pluginstore.Store
	// SessionID names the session to hooks.
	SessionID string
	// ProviderFor builds a provider for a subagent that names its own
	// model or effort. Nil means such a subagent cannot start.
	ProviderFor func(model, effort string) (llm.Provider, error)
	// Extensions are the compiled extensions the binary offers.
	Extensions *extension.Registry
	// Switches turns extensions on and off for this session.
	Switches extension.Switches
	// Limits tunes the run machinery. The zero value is the default.
	Limits Limits
}

// Limits switches parts of the run machinery off and sets the budget.
type Limits struct {
	// NoTrimming sends tool output whole in every request: no cutting
	// of results already seen, no reduction of older ones.
	NoTrimming bool
	// TokenBudget ends the session's runs once this many tokens are
	// used. Zero or less means no budget.
	TokenBudget int
}

// Assembly is the base agent without a model: what a session runs
// with and the manifest that describes it. Close it when done.
type Assembly struct {
	// Root is the project root the blocks were loaded for.
	Root string
	// Toolset is every block's tools, hooks and commands, merged.
	Toolset kit.Toolset
	// SystemPrompt is the host's prompt followed by each block's text.
	SystemPrompt string
	// Plugins is the manifest, including what was refused and why.
	Plugins []kit.Plugin
	// Warnings are problems the session survives, such as a file that
	// could not be loaded. The host decides how to show them.
	Warnings []error
	// Permission is the session's gate. A frontend switches its mode and
	// sets who is asked ([permission.Gate.SetAsker]).
	Permission *permission.Gate
	// Hooks runs the trusted plugins' hooks. The tool-call hooks are in
	// Toolset; a frontend calls the rest (Prompt, Stop). Never nil-unsafe.
	Hooks *pluginhooks.Dispatcher

	env      extension.Env
	registry *extension.Registry
	running  map[string]extension.Extension // the extensions that are on, by name

	// What a switch leaves alone.
	meter     *meter.Provider // between the agent and its model
	machinery kit.Toolset     // the run machinery's hooks; they run first
	base      kit.Toolset     // the host's tools and the memory tools
	prompt    string          // the host's prompt
	skills    skill.Result
	plugins   installed
	started   string // what SessionStart hooks added for the model
	limits    Limits
	delegates []kit.Tool // the tools that start a subagent
	agentRows []kit.Plugin
	child     atomic.Pointer[child]
	files     []contextfile.File
	servers   mcp.Config
	session   *mcp.Session
}

// ExtensionState is one registered extension and whether it is on.
type ExtensionState struct {
	Name        string
	Description string
	On          bool
}

// Assemble loads every block for opts. A block that fails is recorded
// in [Assembly.Warnings] and left out; err is for what makes the
// session meaningless: no directory, or an extension name nobody registered.
func Assemble(ctx context.Context, opts Options) (*Assembly, error) {
	if opts.Dir == "" {
		return nil, errors.New("nib: Options.Dir is required")
	}
	dir, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, fmt.Errorf("nib: %w", err)
	}
	// Checked first: a mistyped name must not cost a server start.
	if _, err := opts.Extensions.Select(opts.Switches); err != nil {
		return nil, fmt.Errorf("nib: %w", err)
	}
	root := ProjectRoot(dir)
	gate := permission.New()
	a := &Assembly{
		Root:       root,
		Permission: gate,
		env:        extension.Env{Dir: dir, Root: root, Permission: gate},
		meter:      meter.New(opts.Limits.TokenBudget),
		registry:   opts.Extensions,
		running:    map[string]extension.Extension{},
		base:       opts.Toolset,
		prompt:     opts.SystemPrompt,
	}
	if opts.Store != nil {
		gate.Register("memory_fetch", permission.Unasked)
		gate.Register("memory_list", permission.Unasked)
		a.base = kit.Merge(a.base, kit.Toolset{Tools: []kit.Tool{
			memorytools.NewFetchTool(opts.Store), memorytools.NewPublishTool(opts.Store),
			memorytools.NewAppendTool(opts.Store), memorytools.NewListTool(opts.Store),
		}})
	}

	a.machinery = machinery(a.meter, gate, opts.Limits)
	a.limits = opts.Limits

	if opts.Plugins != nil {
		a.plugins = installed{active: opts.Plugins.ActivePlugins()}
		a.warn(a.plugins.untrustedNote())
	}
	a.skills, err = skill.DiscoverWithPlugins(root, a.plugins.skillSources(), a.plugins.trusted)
	a.warn(err)
	sources, hookErrs := pluginhooks.FromPlugins(opts.Plugins, a.plugins.active)
	a.Warnings = append(a.Warnings, hookErrs...)
	a.Hooks = pluginhooks.New(root, opts.SessionID, sources)
	a.files, err = contextfile.LoadChain(root, dir)
	a.warn(err)

	if opts.MCP.ProjectDir == "" {
		opts.MCP.ProjectDir = root
	}
	opts.MCP.Plugins = append(opts.MCP.Plugins, a.plugins.mcpSources()...)
	a.servers, err = mcp.Discover(opts.MCP)
	a.warn(err)
	a.warnRefused()
	a.session = mcp.Connect(ctx, a.servers.Servers, mcp.ConnectOptions{Tokens: opts.MCPTokens})
	for _, f := range a.session.Failed {
		a.warn(fmt.Errorf("MCP server %s: %w", f.Server.Name, f.Err))
	}
	// Resources are read, never changed.
	gate.Register(mcp.ListResourcesTool, permission.Unasked)
	gate.Register(mcp.ReadResourceTool, permission.Unasked)

	defs, errs := subagent.Discover(root, a.plugins.agentDirs())
	a.Warnings = append(a.Warnings, errs...)
	a.delegation(defs, opts.ProviderFor)

	if _, err := a.Switch(ctx, opts.Switches); err != nil {
		return nil, errors.Join(fmt.Errorf("nib: %w", err), a.Close())
	}
	return a, nil
}

// machinery returns the hooks every run goes through: output
// trimming, truncation recovery and the permission gate. The budget
// needs no hook: the meter refuses the request itself.
func machinery(limit llm.OutputCapEscalator, gate *permission.Gate, limits Limits) kit.Toolset {
	sets := []kit.Toolset{{Hooks: truncation.Hooks(limit)}, {Hooks: gate.Hooks()}}
	if !limits.NoTrimming {
		// A skill's instructions arrive as a tool result and must stay whole.
		skillTool := func(tool string) bool { return strings.HasPrefix(tool, skill.ToolNamePrefix) }
		sets = append(sets, kit.Toolset{Hooks: compact.Hooks(skillTool)})
	}
	return kit.Merge(sets...)
}

// Switch turns extensions on and off to match switches, then rebuilds
// Toolset, SystemPrompt and Plugins. Skills, files and MCP servers are
// kept. An agent built before a change must be rebuilt with [Assembly.Agent].
func (a *Assembly) Switch(ctx context.Context, switches extension.Switches) (changed bool, err error) {
	specs, err := a.registry.Select(switches)
	if err != nil {
		return false, err
	}
	want := map[string]bool{}
	for _, spec := range specs {
		want[spec.Name] = true
		if _, on := a.running[spec.Name]; on {
			continue
		}
		ext, err := spec.New(ctx, a.env)
		if err != nil {
			a.warn(fmt.Errorf("extension %s: %w", spec.Name, err))
			continue
		}
		a.running[spec.Name] = ext
		changed = true
	}
	for name, ext := range a.running {
		if want[name] {
			continue
		}
		if ext.Close != nil {
			a.warn(ext.Close())
		}
		delete(a.running, name)
		changed = true
	}
	a.compose()
	return changed, nil
}

// compose rebuilds what the host reads from the blocks as they are now.
func (a *Assembly) compose() {
	// Plugin hooks see a call once nib's own gate has let it through.
	sets := []kit.Toolset{a.machinery, {Hooks: a.Hooks.Hooks()}, a.base}
	prompt := []string{a.prompt}
	rows := describe(a.base, "")
	for _, state := range a.Extensions() {
		desc := state.Description
		if !state.On {
			desc = "(off) " + desc
		}
		rows = append(rows, kit.Plugin{Kind: kit.KindExtension, Name: state.Name, Description: desc})
		if ext, on := a.running[state.Name]; on {
			sets = append(sets, ext.Toolset)
			prompt = append(prompt, ext.Prompt)
			rows = append(rows, describe(ext.Toolset, "extension:"+state.Name)...)
		}
	}
	sets = append(sets, kit.Toolset{Tools: a.skills.Tools},
		kit.Toolset{Tools: a.session.Tools(), Commands: a.session.Commands()})
	prompt = append(prompt, contextfile.Prompt(a.files), a.session.Prompt())

	join := func(parts []string) string {
		return strings.Join(slices.DeleteFunc(slices.Clone(parts), func(s string) bool { return s == "" }), "\n\n")
	}
	// A subagent gets the session's tools, hooks and project text: not the
	// run machinery, which keeps state per run, nor the delegation tools.
	shared := kit.Merge(sets[1:]...)
	a.child.Store(&child{tools: shared.Tools, hooks: shared.Hooks, prompt: join(prompt[1:])})
	if a.started != "" {
		prompt = append(prompt, "## Session start\n\n"+a.started)
	}
	sets = append(sets, kit.Toolset{Tools: a.delegates})
	a.Toolset = kit.Merge(sets...)
	a.SystemPrompt = join(prompt)
	a.Plugins = slices.Concat(rows, a.plugins.manifest(), a.agentRows, contextfile.Plugins(a.Root, a.files),
		skill.Plugins(a.skills), mcp.Plugins(a.servers, a.session))
}

// describe returns the manifest rows for a toolset's tools and slash
// commands. source, when set, says which block brought them.
func describe(set kit.Toolset, source string) []kit.Plugin {
	rows := make([]kit.Plugin, 0, len(set.Tools)+len(set.Commands))
	for _, t := range set.Tools {
		rows = append(rows, kit.DescribeTool(t))
	}
	for _, c := range set.Commands {
		rows = append(rows, kit.DescribeCommand(c))
	}
	if source != "" {
		for i := range rows {
			rows[i].Source = source
		}
	}
	return rows
}

// Extensions lists every registered extension, in registration order,
// with whether it is on. A frontend reads this, not the manifest text.
func (a *Assembly) Extensions() []ExtensionState {
	specs := a.registry.Specs()
	states := make([]ExtensionState, 0, len(specs))
	for _, spec := range specs {
		_, on := a.running[spec.Name]
		states = append(states, ExtensionState{Name: spec.Name, Description: spec.Description, On: on})
	}
	return states
}

// ProjectRoot returns the nearest ancestor of dir, or dir itself, that
// holds a .git entry. With none it returns dir: without a repository
// boundary, no parent directory is read.
func ProjectRoot(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir // only when the working directory is gone
	}
	for cur := abs; ; {
		// Any Lstat failure means this directory marks no repository.
		if _, err := os.Lstat(filepath.Join(cur, ".git")); err == nil {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs
		}
		cur = parent
	}
}

// warnRefused records the servers that will not start. Those waiting
// for approval share one warning: a person may never give it.
func (a *Assembly) warnRefused() {
	untrusted := 0
	for _, r := range a.servers.Refused {
		if errors.Is(r.Err, mcp.ErrUntrusted) {
			untrusted++
			continue
		}
		a.warn(fmt.Errorf("MCP server %s not started: %w", r.Server.Name, r.Err))
	}
	if untrusted > 0 {
		a.warn(fmt.Errorf("%d server(s) in %s not started: %w", untrusted, mcp.ProjectConfigName, mcp.ErrUntrusted))
	}
}

// warn records err unless it is nil.
func (a *Assembly) warn(err error) {
	if err != nil {
		a.Warnings = append(a.Warnings, err)
	}
}

// Agent returns the session's agent on provider. The Assembly still
// owns the blocks: close the agent first, then the Assembly.
func (a *Assembly) Agent(provider llm.Provider) (*kit.Agent, error) {
	a.meter.Set(provider)
	return kit.New(kit.Config{Provider: a.meter, SystemPrompt: a.SystemPrompt, Toolset: a.Toolset})
}

// SetProvider switches the model for the session's agent, from its
// next request on. The conversation and the token count carry over.
func (a *Assembly) SetProvider(provider llm.Provider) { a.meter.Set(provider) }

// Usage returns the tokens the session has used.
func (a *Assembly) Usage() budget.Session { return a.meter.Usage() }

// SetAsker sets who answers the MCP servers' questions to the person.
// Without one, every question is answered as dismissed.
func (a *Assembly) SetAsker(ask elicit.Asker) { a.session.SetAsker(ask) }

// WatchServers sets what is called, on a goroutine of its own, when an
// MCP server's tools or prompts changed. The host then calls
// [Assembly.Refresh] and rebuilds its agent when no run is active.
func (a *Assembly) WatchServers(onChange func()) { a.session.Watch(onChange) }

// Refresh rebuilds Toolset, SystemPrompt and Plugins from the blocks
// as they are now. An agent built before must be rebuilt.
func (a *Assembly) Refresh() { a.compose() }

// StartSession runs the SessionStart hooks; source says why, such as
// "startup". What they add joins the system prompt of agents built after.
func (a *Assembly) StartSession(ctx context.Context, source string) {
	if text := a.Hooks.SessionStart(ctx, source); text != a.started {
		a.started = text
		a.compose()
	}
}

// Close releases every block. A second call does nothing.
func (a *Assembly) Close() error {
	var errs []error
	for name, ext := range a.running {
		if ext.Close != nil {
			errs = append(errs, ext.Close())
		}
		delete(a.running, name)
	}
	errs = append(errs, a.session.Close())
	a.session = nil
	return errors.Join(errs...)
}
