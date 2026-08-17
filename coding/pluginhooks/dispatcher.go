package pluginhooks

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/latebit-io/nib/kit/hookmap"
	"github.com/latebit-io/nib/kit/hookrun"
	"github.com/latebit-io/nib/kit/hookspec"
)

// Decision is the dispatcher's verdict for an event that can be vetoed
// (PreToolUse, UserPromptSubmit). The zero value proceeds.
type Decision struct {
	// Deny reports that a hook objected to the action.
	Deny bool
	// Reason is the objecting hook's feedback, surfaced to the LLM (as a
	// block reason) or the user (as a rejected prompt).
	Reason string
}

// Dispatcher runs the lifecycle hooks of the trusted plugins it was
// built with. It is safe for concurrent use: it holds immutable config
// and a value-type runner, and keeps no mutable state.
type Dispatcher struct {
	configs []hookspec.Config
	runner  hookrun.Runner
	cwd     string
}

// New builds a Dispatcher over the given trusted-plugin hook configs. The
// caller (the wiring layer) is responsible for the trust gate: pass only
// the configs of enabled+trusted plugins. cwd is the project working
// directory delivered to each hook in its JSON input. Passing no configs
// yields a dispatcher whose every method is a proceeding no-op.
func New(configs []hookspec.Config, runner hookrun.Runner, cwd string) *Dispatcher {
	return &Dispatcher{configs: configs, runner: runner, cwd: cwd}
}

// PreToolUse runs the PreToolUse hooks whose matcher matches the tool and
// returns the first Deny (which the caller maps to a tool BLOCK). argsJSON
// is the raw tool-call argument JSON, passed through verbatim.
func (d *Dispatcher) PreToolUse(ctx context.Context, toolName, argsJSON string) Decision {
	if d == nil {
		return Decision{}
	}
	in := hookrun.Input{
		Event:    string(hookspec.PreToolUse),
		Tool:     toolName,
		ToolArgs: rawJSON(argsJSON),
		Cwd:      d.cwd,
	}
	return d.dispatch(ctx, hookspec.PreToolUse, in, toolName, true, true)
}

// PostToolUse runs the PostToolUse hooks whose matcher matches the tool.
// A Deny maps to an error result: overrideContent is the hook's reason
// and isError is true. With no Deny both returns are nil (leave the
// result untouched). v1 does not perform free-form content substitution
// beyond the deny-to-error mapping.
func (d *Dispatcher) PostToolUse(ctx context.Context, toolName, argsJSON, resultText string) (overrideContent *string, isError *bool) {
	if d == nil {
		return nil, nil
	}
	in := hookrun.Input{
		Event:      string(hookspec.PostToolUse),
		Tool:       toolName,
		ToolArgs:   rawJSON(argsJSON),
		ToolResult: resultText,
		Cwd:        d.cwd,
	}
	dec := d.dispatch(ctx, hookspec.PostToolUse, in, toolName, true, true)
	if !dec.Deny {
		return nil, nil
	}
	reason, isErr := dec.Reason, true
	return &reason, &isErr
}

// UserPromptSubmit runs the UserPromptSubmit hooks and returns the first
// Deny (which the caller maps to a blocked prompt). These hooks carry no
// tool, so every group fires regardless of its matcher.
func (d *Dispatcher) UserPromptSubmit(ctx context.Context, prompt string) Decision {
	if d == nil {
		return Decision{}
	}
	in := hookrun.Input{
		Event:  string(hookspec.UserPromptSubmit),
		Prompt: prompt,
		Cwd:    d.cwd,
	}
	return d.dispatch(ctx, hookspec.UserPromptSubmit, in, "", false, true)
}

// SessionStart runs the SessionStart hooks for their side effects. A deny
// has no run-loop meaning at session start in v1, so the decision is
// discarded (hook infrastructure failures are still logged).
func (d *Dispatcher) SessionStart(ctx context.Context) {
	d.fireAndForget(ctx, hookspec.SessionStart)
}

// Stop runs the Stop hooks when a run completes. v1 picks run-completion
// semantics and does not let a Stop hook keep the agent running, so the
// decision is discarded.
func (d *Dispatcher) Stop(ctx context.Context) {
	d.fireAndForget(ctx, hookspec.Stop)
}

// PreCompact runs the PreCompact hooks before conversation history is
// compacted. The decision is advisory only in v1 and discarded.
func (d *Dispatcher) PreCompact(ctx context.Context) {
	d.fireAndForget(ctx, hookspec.PreCompact)
}

// SubagentStop runs the SubagentStop hooks after a spawned subagent
// finishes. The decision is discarded in v1.
func (d *Dispatcher) SubagentStop(ctx context.Context) {
	d.fireAndForget(ctx, hookspec.SubagentStop)
}

// fireAndForget runs EVERY hook under a non-vetoable lifecycle event for
// its side effects, discarding decisions. Used by the SessionStart /
// Stop / PreCompact / SubagentStop emit points, none of which act on a
// deny in v1 — so stopOnDeny is false: a hook that denies must NOT
// suppress the lifecycle hooks that follow it.
func (d *Dispatcher) fireAndForget(ctx context.Context, ev hookspec.Event) {
	if d == nil {
		return
	}
	in := hookrun.Input{Event: string(ev), Cwd: d.cwd}
	// Lifecycle events are non-vetoable in v1; a deny is logged, not acted on.
	if dec := d.dispatch(ctx, ev, in, "", false, false); dec.Deny {
		slog.Debug("plugin hook denied non-vetoable lifecycle event; ignored", "event", ev, "reason", dec.Reason)
	}
}

// dispatch runs the hooks under ev across every config. When matchTool is
// true, only groups whose matcher matches toolName (via [hookmap.Matches],
// reconciling CC and nib tool names) fire; otherwise every group under the
// event fires (matchers are meaningless for non-tool events).
//
// stopOnDeny selects the semantics for a hook's deny. Vetoable events
// (Pre/PostToolUse, UserPromptSubmit) pass true: the first Deny short-
// circuits and is returned ("first deny wins", deterministic over the
// wiring layer's plugin-ID-sorted config slice). Non-vetoable lifecycle
// events pass false: a deny is ignored and the remaining hooks still run
// for their side effects, so the returned zero Decision is meaningless.
//
// Hooks run in config order, then group order, then hook order.
func (d *Dispatcher) dispatch(ctx context.Context, ev hookspec.Event, in hookrun.Input, toolName string, matchTool, stopOnDeny bool) Decision {
	for _, cfg := range d.configs {
		for _, g := range cfg.Hooks[ev] {
			if matchTool && !hookmap.Matches(g, toolName) {
				continue
			}
			for _, h := range g.Hooks {
				if !h.Runnable() {
					continue
				}
				res := d.runner.Run(ctx, h, in)
				if res.Err != nil {
					// Infrastructure failure: [hookrun] already failed open
					// (Decision=Proceed). Log it — a broken hook must not be
					// silently swallowed — but never convert it to a deny.
					slog.Warn("pluginhooks: hook execution failed; proceeding",
						"event", ev, "tool", toolName, "err", res.Err)
				}
				if res.Decision == hookrun.Deny && stopOnDeny {
					return Decision{Deny: true, Reason: res.Reason}
				}
			}
		}
	}
	return Decision{}
}

// rawJSON wraps a raw argument string as JSON for the hook input,
// returning nil for blank input so the omitempty field stays absent.
func rawJSON(s string) json.RawMessage {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return json.RawMessage(s)
}
