// Package subagent lets the model hand a task to a child agent: a tool
// per agent definition (agent_<name>) and per forked skill. The child
// gets only the task, the session's tools and gates, and returns its answer.
package subagent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/agentdef"
	"github.com/latebit-io/nib/kit/hookmap"
	"github.com/latebit-io/nib/kit/skill"
	"github.com/latebit-io/nib/kit/toolperm"
	"github.com/latebit-io/nib/kit/tools/bash"
	"github.com/latebit-io/nib/kit/tools/files"
)

// ToolNamePrefix starts the name of each agent's tool.
const ToolNamePrefix = "agent_"

// defaultMaxTurns bounds a child that sets no limit of its own.
const defaultMaxTurns = 50

// Host is what a child agent is built from: the session's model, tools
// and gates. Each function is called when a child starts.
type Host struct {
	// Provider returns the model to run a child on; model and effort are
	// the definition's ("" for the session's own).
	Provider func(model, effort string) (llm.Provider, error)
	// Tools are the tools a child may use, before its own grants narrow
	// them. They must not include the delegation tools themselves.
	Tools func() []kit.Tool
	// Hooks are the hooks of a child run on provider, built fresh: some
	// keep state per run, and a truncation retry raises that provider's limit.
	Hooks func(provider llm.Provider) kit.Hooks
	// Prompt returns a child's system prompt around its persona.
	Prompt func(persona string) string
	// OnStop is told when a child finished, with its answer. Optional.
	OnStop func(ctx context.Context, agentType, answer string)
	// Skills returns the instructions of the named skills, for an agent
	// that preloads them, and the names it does not know. Optional.
	Skills func(names []string) (text string, missing []string)
	// Resolve returns the project-relative file a call's path reaches, so
	// a grant like Read(src/*) is checked on it. Optional: without it the
	// path is only cleaned.
	Resolve func(path string) (string, error)
}

// task is one kind of child.
type task struct {
	def      llm.ToolDef
	kind     string // the agent's or skill's name, for hooks and logs
	persona  string // the child's system prompt
	brief    string // a fork skill's instructions: the task, before the caller's details
	grants   *toolperm.Matcher
	model    string
	effort   string
	maxTurns int
	host     Host
}

// newTask builds one child kind. A fork's task argument is optional: its
// instructions are already the task.
func newTask(name, description string, allowed, disallowed []string, t task) (*task, error) {
	grants, err := nibGrants(allowed, disallowed, t.host.Resolve)
	if err != nil {
		return nil, err
	}
	t.grants, t.maxTurns = grants, cmp.Or(t.maxTurns, defaultMaxTurns)
	detail := "Everything the agent needs to do it: it sees nothing of this conversation."
	desc := description + " Runs as a separate agent that returns its final answer."
	params := llm.FunctionParams{Type: "object", Required: []string{"task"}}
	if t.brief != "" {
		detail = "Anything the task needs beyond the skill's own instructions, such as which files or what to look for."
		desc = description + " Runs as a separate agent with the skill's instructions and returns its answer."
		params.Required = nil
	}
	params.Properties = map[string]llm.FunctionParam{"task": {Type: "string", Description: detail}}
	t.def = llm.ToolDef{Type: "function", Function: llm.FunctionDef{Name: name, Description: strings.TrimSpace(desc), Parameters: params}}
	return &t, nil
}

// AgentTools returns a tool per definition. A definition nib cannot run
// as asked is left out with the reason: one asking for a worktree would
// otherwise run in the project itself.
func AgentTools(defs []agentdef.Definition, host Host) ([]kit.Tool, []error) {
	var tools []kit.Tool
	var errs []error
	for _, d := range defs {
		if d.Isolation == agentdef.IsolationWorktree {
			errs = append(errs, fmt.Errorf("agent %s asks for a worktree, which nib does not offer yet; left out", d.Name))
			continue
		}
		persona := d.SystemPrompt
		if len(d.Skills) > 0 && host.Skills != nil {
			text, missing := host.Skills(d.Skills)
			if len(missing) > 0 {
				errs = append(errs, fmt.Errorf("agent %s preloads skills nib does not have: %s", d.Name, strings.Join(missing, ", ")))
			}
			if text != "" {
				persona += "\n\n## Skills\n\n" + text
			}
		}
		t, err := newTask(ToolNamePrefix+d.Name, d.Description, d.AllowedTools, d.DisallowedTools, task{
			kind: d.Name, persona: persona, model: d.Model, effort: d.Effort, maxTurns: d.MaxTurns, host: host,
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("agent %s: %w", d.Name, err))
			continue
		}
		tools = append(tools, t)
	}
	return tools, errs
}

// ForkTools returns a tool per skill that runs forked: its instructions
// become a child's task, with the tools the skill allows.
func ForkTools(skills []skill.Skill, host Host) ([]kit.Tool, []error) {
	var tools []kit.Tool
	var errs []error
	for _, s := range skills {
		t, err := newTask(skill.ToolNamePrefix+s.Name, s.Description, s.AllowedTools, s.DisallowedTools, task{
			kind: s.Name, brief: s.Body, host: host,
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("skill %s: %w", s.Name, err))
			continue
		}
		tools = append(tools, t)
	}
	return tools, errs
}

// nibGrants parses a definition's tool grants, naming tools as nib does
// as well as Claude Code: "Read" grants read_file.
func nibGrants(allowed, disallowed []string, resolve func(string) (string, error)) (*toolperm.Matcher, error) {
	parse := func(raw []string) ([]toolperm.Rule, error) {
		rules, err := toolperm.ParseField(raw)
		if err != nil {
			return nil, err
		}
		var out []toolperm.Rule
		for _, r := range rules {
			// A file pattern is resolved as a call's path is, so ./x.key and
			// x.key match alike. One outside the project stays as written:
			// a call there is refused before any grant is read.
			if r.Arg != "" && !r.IsShell() {
				target, err := patternTarget(r.Arg, resolve)
				switch {
				case errors.Is(err, errUnclean):
					return nil, err
				case err == nil:
					r.Arg = target
				}
			}
			out = append(out, r)
			for _, nib := range hookmap.NibNames(r.Tool) {
				alias := r
				alias.Tool = nib
				out = append(out, alias)
			}
		}
		return out, nil
	}
	allow, err := parse(allowed)
	if err != nil {
		return nil, fmt.Errorf("allowed tools: %w", err)
	}
	deny, err := parse(disallowed)
	if err != nil {
		return nil, fmt.Errorf("disallowed tools: %w", err)
	}
	if len(allow) == 0 && len(deny) == 0 {
		return nil, nil
	}
	return toolperm.New(allow, deny), nil
}

// Definition implements [agent.Tool].
func (t *task) Definition() llm.ToolDef { return t.def }

// Execute implements [agent.Tool]: it runs one child to the end.
func (t *task) Execute(ctx context.Context, call llm.ToolCall) agent.ToolResult {
	args, err := llm.DecodeArguments[struct {
		Task string `json:"task"`
	}](t.def.Function.Parameters, call.Function.Arguments)
	if err != nil {
		return failed("invalid arguments: %v", err)
	}
	goal := strings.TrimSpace(strings.Join([]string{t.brief, args.Task}, "\n\n"))
	provider, err := t.host.Provider(t.model, t.effort)
	if err != nil {
		return failed("no model for %s: %v", t.kind, err)
	}
	child, err := kit.New(kit.Config{
		Provider: provider,
		Toolset:  kit.Toolset{Tools: grant(t.host.Tools(), t.grants, t.host.Resolve), Hooks: t.host.Hooks(provider)},
		MaxTurns: t.maxTurns,
	})
	if err != nil {
		return failed("start %s: %v", t.kind, err)
	}
	defer child.Close()
	res, err := child.Run(ctx, []llm.Message{
		{Role: "system", Content: t.host.Prompt(t.persona)},
		{Role: "user", Content: goal},
	})
	answer := strings.TrimSpace(res.Done.FinalText)
	if t.host.OnStop != nil {
		t.host.OnStop(context.WithoutCancel(ctx), t.kind, answer)
	}
	if err == nil {
		err = res.Done.Err
	}
	if err != nil {
		if answer != "" {
			return agent.ToolResult{Content: answer + "\n\n(the agent stopped early: " + err.Error() + ")", IsError: true}
		}
		return failed("%s stopped: %v", t.kind, err)
	}
	if answer == "" {
		return failed("%s finished without an answer", t.kind)
	}
	return agent.ToolResult{Content: answer}
}

// failed is an error result.
func failed(format string, args ...any) agent.ToolResult {
	return agent.ToolResult{Content: "Error: " + fmt.Sprintf(format, args...), IsError: true}
}

// grant returns the tools a child may use: with an allow list only those
// it names, never one it denies outright, and a tool whose grant is
// scoped by argument (Bash(git *)) checked call by call.
func grant(tools []kit.Tool, m *toolperm.Matcher, resolve func(string) (string, error)) []kit.Tool {
	if m == nil {
		return tools
	}
	var out []kit.Tool
	for _, t := range tools {
		name := t.Definition().Function.Name
		if !m.Admits(name) {
			continue
		}
		if m.HasArgRules(name) {
			t = &scoped{Tool: t, grants: m, resolve: resolve}
		}
		out = append(out, t)
	}
	return out
}

// scoped is a tool whose grant depends on the call's argument.
type scoped struct {
	kit.Tool
	grants  *toolperm.Matcher
	resolve func(string) (string, error)
}

// Execute implements [agent.Tool].
func (s *scoped) Execute(ctx context.Context, call llm.ToolCall) agent.ToolResult {
	name := call.Function.Name
	arg := argument(name, call.Function.Arguments)
	// A glob is matched against one simple command; a compound one could hide another.
	if name == "bash" && toolperm.HasShellControl(arg) {
		return failed("this agent may run only single commands that its grants name")
	}
	if name != "bash" {
		// An omitted path is the working directory, where glob and list_dir
		// default; a tool that needs a path still refuses the call itself.
		target, err := fileTarget(cmp.Or(arg, "."), s.resolve)
		if err != nil {
			return failed("%v", err)
		}
		arg = target
	}
	if !s.grants.PermitsArg(name, arg) {
		return failed("this agent is not granted %s %q", name, arg)
	}
	return s.Tool.Execute(ctx, call)
}

// errUnclean refuses a file pattern that cleaning would change. Call
// paths are cleaned, and a pattern rewritten to match them can miss.
var errUnclean = errors.New("write it as a clean path, such as src/*.go or ./src/*.go")

// patternTarget resolves a file pattern as fileTarget resolves a path, up
// to the component with its first wildcard. The rest names no entry, not
// even one called "*", so it stays as written.
func patternTarget(pattern string, resolve func(string) (string, error)) (string, error) {
	p := filepath.ToSlash(pattern)
	clean := path.Clean(p)
	if p != clean && p != "./"+clean {
		return "", fmt.Errorf("pattern %s: %w", pattern, errUnclean)
	}
	parts := strings.Split(clean, "/")
	i := slices.IndexFunc(parts, toolperm.HasWildcard)
	if i < 0 {
		return fileTarget(clean, resolve)
	}
	prefix := strings.Join(parts[:i], "/")
	switch {
	case i == 0:
		prefix = "."
	case prefix == "":
		prefix = "/" // the pattern was /*...
	}
	dir, err := fileTarget(prefix, resolve)
	if err != nil {
		return "", err
	}
	return path.Join(dir, strings.Join(parts[i:], "/")), nil
}

// fileTarget is the file a path reaches, as grants name files: src/../x
// is x, and a link is the file it points to. Without resolve the path is
// only cleaned.
func fileTarget(p string, resolve func(string) (string, error)) (string, error) {
	if resolve != nil {
		return resolve(p)
	}
	clean := path.Clean(filepath.ToSlash(p))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%s is outside the project", p)
	}
	return clean, nil
}

// argument is what an argument-scoped grant is matched against: the
// command of a shell call, else the path of a file call.
func argument(tool, args string) string {
	if tool == "bash" {
		return strings.TrimSpace(bash.CommandOf(args))
	}
	return files.PathOf(args)
}

// PluginDir is a trusted plugin's agents directory.
type PluginDir struct {
	ID, Dir string
}

// Discover loads agent definitions: the project's (.project/agents), the
// person's, then the plugins'. A name already taken keeps the first; a
// directory that cannot be read is reported and the rest still load.
func Discover(root string, plugins []PluginDir) ([]agentdef.Definition, []error) {
	var all []agentdef.Definition
	var errs []error
	load := func(dir string, source agentdef.Source, plugin string) {
		defs, err := agentdef.LoadDir(dir, source) // a missing directory is no error
		if err != nil {
			errs = append(errs, fmt.Errorf("agents in %s: %w", dir, err))
		}
		for _, d := range defs {
			d.PluginID = plugin
			if slices.ContainsFunc(all, func(o agentdef.Definition) bool { return o.Name == d.Name }) {
				slog.Debug("subagent: shadowed", "name", d.Name, "path", d.Path)
				continue
			}
			all = append(all, d)
		}
	}
	load(filepath.Join(root, ".project", "agents"), agentdef.SourceProject, "")
	if dir, ok := globalDir(); ok {
		load(dir, agentdef.SourceGlobal, "")
	}
	for _, p := range plugins {
		load(p.Dir, agentdef.SourcePlugin, p.ID)
	}
	return all, errs
}

// globalDir is the person's agents directory.
func globalDir() (string, bool) {
	if dir := os.Getenv(brand.EnvKeyGlobalAgentsDir); dir != "" {
		return dir, true
	}
	cfg, err := os.UserConfigDir()
	if err != nil {
		slog.Warn("subagent: no user config dir, so no personal agents", "err", err)
		return "", false
	}
	return filepath.Join(cfg, brand.ConfigDirName, "agents"), true
}
