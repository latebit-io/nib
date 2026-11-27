// Package pluginhooks runs installed plugins' hooks as Claude Code does:
// around tool calls (as [kit.Hooks]), on a submitted prompt, when the
// agent or a subagent stops, and when a session starts and ends.
package pluginhooks

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/hookmap"
	"github.com/latebit-io/nib/kit/hookrun"
	"github.com/latebit-io/nib/kit/hookspec"
	"github.com/latebit-io/nib/kit/internal/filecap"
	"github.com/latebit-io/nib/kit/pluginstore"
)

// promptTimeout bounds a UserPromptSubmit hook that sets no timeout, as
// Claude Code does: the person waits on it.
const promptTimeout = 30 * time.Second

// maxHooksFile caps one plugin's hooks file.
const maxHooksFile = 1 << 20

// Source is one plugin's hooks.
type Source struct {
	// Plugin is the plugin's id, for messages.
	Plugin string
	// Config is the parsed hooks file.
	Config hookspec.Config
	// Env is what the plugin's hooks also see: CLAUDE_PLUGIN_ROOT and
	// CLAUDE_PLUGIN_DATA.
	Env map[string]string
}

// FromPlugins loads the hooks of the trusted plugins among active; one
// that is not trusted runs none, since hooks are shell commands, and the
// caller says so. A file that cannot be used is reported; the rest load.
func FromPlugins(store *pluginstore.Store, active []pluginstore.ActivePlugin) ([]Source, []error) {
	var sources []Source
	var errs []error
	for _, p := range active {
		if !p.Trusted {
			continue
		}
		data, err := filecap.Read(p.HooksConfigPath, maxHooksFile, "hooks file")
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("plugin %s: %w", p.ID, err))
			continue
		}
		cfg, err := hookspec.Parse(data)
		if err != nil {
			errs = append(errs, fmt.Errorf("plugin %s: %w", p.ID, err))
			continue
		}
		for _, u := range cfg.Unsupported() {
			errs = append(errs, fmt.Errorf("plugin %s: %s hook left out: %s", p.ID, u.Event, u.Reason))
		}
		sources = append(sources, Source{Plugin: p.ID, Config: cfg, Env: map[string]string{
			"CLAUDE_PLUGIN_ROOT": store.ConvertedDir(p.ID),
			"CLAUDE_PLUGIN_DATA": store.DataDir(p.ID),
		}})
	}
	return sources, errs
}

// Dispatcher runs the hooks of every source. A nil *Dispatcher runs nothing.
type Dispatcher struct {
	dir     string // the project root: hooks run here
	session string
	sources []Source

	mu     sync.Mutex
	passed map[string]string // calls PreToolUse let through, with the context it added
}

// New returns a dispatcher for sources, run in dir (CLAUDE_PROJECT_DIR)
// for the session named sessionID.
func New(dir, sessionID string, sources []Source) *Dispatcher {
	withDir := make([]Source, len(sources))
	for i, s := range sources {
		env := map[string]string{"CLAUDE_PROJECT_DIR": dir}
		maps.Copy(env, s.Env)
		s.Env = env
		withDir[i] = s
	}
	return &Dispatcher{dir: dir, session: sessionID, sources: withDir, passed: map[string]string{}}
}

// Has reports whether any hook listens for ev.
func (d *Dispatcher) Has(ev hookspec.Event) bool {
	if d == nil {
		return false
	}
	for _, s := range d.sources {
		if len(s.Config.Hooks[ev]) > 0 {
			return true
		}
	}
	return false
}

// Hooks returns the tool-call hooks: PreToolUse before each call, and
// PostToolUse after one that ran. None when no hook is about tools.
func (d *Dispatcher) Hooks() kit.Hooks {
	if !d.Has(hookspec.PreToolUse) && !d.Has(hookspec.PostToolUse) {
		return kit.Hooks{}
	}
	return kit.Hooks{BeforeToolCall: d.before, AfterToolCall: d.after}
}

// job is one hook to run, with its plugin's environment.
type job struct {
	hook   hookspec.Hook
	plugin string
	env    map[string]string
}

// jobs are the runnable hooks of ev whose matcher fits tool ("" for
// events that have none).
func (d *Dispatcher) jobs(ev hookspec.Event, tool string) []job {
	if d == nil {
		return nil
	}
	var out []job
	for _, s := range d.sources {
		for _, g := range s.Config.Hooks[ev] {
			if tool != "" && !hookmap.Matches(g, tool) {
				continue
			}
			for _, h := range g.Hooks {
				if h.Runnable() {
					out = append(out, job{hook: h, plugin: s.Plugin, env: s.Env})
				}
			}
		}
	}
	return out
}

// run runs every hook of ev that fits tool, all at once as Claude Code
// does. fill completes the input, which is built only when a hook runs.
// Failures are logged and do not block.
func (d *Dispatcher) run(ctx context.Context, ev hookspec.Event, tool string, timeout time.Duration, fill func(*hookrun.Input)) []hookrun.Result {
	jobs := d.jobs(ev, tool)
	if len(jobs) == 0 {
		return nil
	}
	in := hookrun.Input{SessionID: d.session, Cwd: d.dir, HookEventName: string(ev)}
	if fill != nil {
		fill(&in)
	}
	results := make([]hookrun.Result, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		runner := hookrun.Runner{Timeout: timeout, Dir: d.dir, Env: j.env}
		wg.Go(func() { results[i] = runner.Run(ctx, j.hook, in) })
	}
	wg.Wait()
	for i, r := range results {
		if r.Err != nil {
			slog.Warn("pluginhooks: hook failed", "plugin", jobs[i].plugin, "event", ev, "err", r.Err)
		}
	}
	return results
}

// strongest combines the decisions of hooks that ran together: a deny
// wins over an ask, an ask over an allow, as in Claude Code.
func strongest(results []hookrun.Result) (decision hookrun.Decision, reason string) {
	rank := map[hookrun.Decision]int{hookrun.Proceed: 0, hookrun.Allow: 1, hookrun.Ask: 2, hookrun.Deny: 3}
	decision = hookrun.Proceed
	for _, r := range results {
		if rank[r.Decision] > rank[decision] {
			decision, reason = r.Decision, r.Reason
		}
	}
	return decision, reason
}

// stopped returns the reason of the first hook that asked to end the
// work ("continue": false).
func stopped(results []hookrun.Result) (reason string, ok bool) {
	for _, r := range results {
		if r.Stop {
			return cmp.Or(r.StopReason, r.Reason, "a hook ended the work"), true
		}
	}
	return "", false
}

// contexts joins what the hooks added for the model.
func contexts(results []hookrun.Result) string {
	var parts []string
	for _, r := range results {
		if c := strings.TrimSpace(r.Context); c != "" {
			parts = append(parts, c)
		}
	}
	return strings.Join(parts, "\n")
}

func (d *Dispatcher) before(ctx context.Context, c kit.BeforeToolCallInput) (kit.BeforeToolCallResult, error) {
	results := d.run(ctx, hookspec.PreToolUse, c.Name, 0, func(in *hookrun.Input) {
		in.ToolName, in.ToolInput, in.ToolUseID = ccName(c.Name), ccInput(c.Name, c.Args), c.CallID
	})
	if reason, ok := stopped(results); ok {
		return kit.BeforeToolCallResult{Block: true, Reason: "A hook stopped the work here: " + reason}, nil
	}
	switch decision, reason := strongest(results); decision {
	case hookrun.Deny:
		return kit.BeforeToolCallResult{Block: true, Reason: "A hook blocked this call: " + cmp.Or(reason, "no reason given")}, nil
	case hookrun.Ask:
		// nib has no way to put a hook's question to the person mid-call; refusing is the safe side.
		return kit.BeforeToolCallResult{Block: true, Reason: "A hook wants the person to approve this call, which this session cannot ask: " + cmp.Or(reason, "no reason given")}, nil
	}
	d.mu.Lock()
	d.passed[c.CallID] = contexts(results)
	d.mu.Unlock()
	return kit.BeforeToolCallResult{}, nil
}

func (d *Dispatcher) after(ctx context.Context, c kit.AfterToolCallInput) (kit.AfterToolCallResult, error) {
	d.mu.Lock()
	notes, ran := d.passed[c.CallID]
	delete(d.passed, c.CallID)
	d.mu.Unlock()
	// A call that was blocked did not run: Claude Code fires no PostToolUse for it.
	if !ran {
		return kit.AfterToolCallResult{}, nil
	}
	results := d.run(ctx, hookspec.PostToolUse, c.Name, 0, func(in *hookrun.Input) {
		in.ToolName, in.ToolInput, in.ToolUseID = ccName(c.Name), ccInput(c.Name, c.Args), c.CallID
		in.ToolResponse = c.Result.Content
	})
	var added []string
	if notes != "" {
		added = append(added, notes)
	}
	// The call already ran: a block can only tell the model why.
	if decision, reason := strongest(results); decision == hookrun.Deny {
		added = append(added, "A hook objected to this call: "+cmp.Or(reason, "no reason given"))
	}
	if text := contexts(results); text != "" {
		added = append(added, text)
	}
	if len(added) == 0 {
		return kit.AfterToolCallResult{}, nil
	}
	content := c.Result.Content + "\n\n" + strings.Join(added, "\n")
	return kit.AfterToolCallResult{Content: &content}, nil
}

// BlockedError is a prompt a hook did not let through.
type BlockedError struct{ Reason string }

// Error implements error.
func (e *BlockedError) Error() string {
	return "a hook blocked the prompt: " + cmp.Or(e.Reason, "no reason given")
}

// Prompt runs the UserPromptSubmit hooks on what the person typed and
// returns the prompt to send, with any context the hooks added. A hook
// that blocks, or ends the work, returns a [*BlockedError].
func (d *Dispatcher) Prompt(ctx context.Context, prompt string) (string, error) {
	results := d.run(ctx, hookspec.UserPromptSubmit, "", promptTimeout, func(in *hookrun.Input) { in.Prompt = prompt })
	if reason, ok := stopped(results); ok {
		return "", &BlockedError{Reason: reason}
	}
	if decision, reason := strongest(results); decision == hookrun.Deny {
		return "", &BlockedError{Reason: reason}
	}
	if text := contexts(results); text != "" {
		return prompt + "\n\n" + text, nil
	}
	return prompt, nil
}

// SessionStart runs the SessionStart hooks; source is why the session
// starts, such as "startup". It returns the context they add.
func (d *Dispatcher) SessionStart(ctx context.Context, source string) string {
	return contexts(d.run(ctx, hookspec.SessionStart, "", 0, func(in *hookrun.Input) { in.Source = source }))
}

// SessionEnd runs the SessionEnd hooks for their side effects.
func (d *Dispatcher) SessionEnd(ctx context.Context, reason string) {
	d.run(ctx, hookspec.SessionEnd, "", 0, func(in *hookrun.Input) { in.Reason = reason })
}

// Stop runs the Stop hooks when the agent has finished answering. A block
// returns its reason, to send next; ending the work ("continue": false)
// wins over that. active says the agent is going on because of one.
func (d *Dispatcher) Stop(ctx context.Context, last string, active bool) string {
	results := d.run(ctx, hookspec.Stop, "", 0, func(in *hookrun.Input) {
		in.LastAssistantMessage, in.StopHookActive = last, &active
	})
	if _, halt := stopped(results); halt {
		return ""
	}
	if decision, reason := strongest(results); decision == hookrun.Deny {
		return cmp.Or(reason, "Keep going.")
	}
	return ""
}

// SubagentStop runs the SubagentStop hooks after a subagent of type
// agentType finished, for their side effects.
func (d *Dispatcher) SubagentStop(ctx context.Context, agentType, last string) {
	d.run(ctx, hookspec.SubagentStop, "", 0, func(in *hookrun.Input) {
		in.AgentType, in.LastAssistantMessage = agentType, last
	})
}

// ccName is the name a hook knows a tool by: Claude Code's, where nib's
// tool has one.
func ccName(tool string) string {
	if cc, ok := hookmap.CCName(tool); ok {
		return cc
	}
	return tool
}

// ccInput passes a call's arguments as Claude Code names them: nib's
// Read, Write and Edit tools say path where Claude Code says file_path.
func ccInput(tool, args string) json.RawMessage {
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(args), &obj) != nil {
		return quoted(args)
	}
	switch ccName(tool) {
	case "Read", "Write", "Edit":
		if path, ok := obj["path"]; ok {
			if _, has := obj["file_path"]; !has {
				obj["file_path"] = path
			}
		}
	default:
		return json.RawMessage(args)
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return quoted(args)
	}
	return data
}

// quoted encodes text as a JSON string.
func quoted(text string) json.RawMessage {
	data, err := json.Marshal(text)
	if err != nil {
		return json.RawMessage(`""`) // a Go string always encodes
	}
	return data
}
