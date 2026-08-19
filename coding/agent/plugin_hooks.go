package agent

import (
	"context"
	"strings"

	upagent "github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/coding/pluginhooks"
)

// HookDispatcher runs Claude Code plugin lifecycle hooks at the run
// loop's emit points. [github.com/latebit-io/nib/coding/pluginhooks.Dispatcher]
// is the production implementation, built by the binary from the trusted
// plugins' converted hook configs and supplied via
// [NewOptions.HookDispatcher].
//
// It is an interface (not the concrete type) so construction — the trust
// gate and config loading the binary owns — stays decoupled from
// invocation here, and so tests can inject a fake. A nil dispatcher means
// "no plugin hooks": the gate methods below short-circuit to a proceed,
// so the agent never has to nil-check at the call sites.
//
// All agent-side emit points are wired: PreToolUse / PostToolUse through
// the gate methods below, and the prompt, run, and compaction lifecycle
// events through their respective helpers. SubagentStop is a parent
// session event fired by the spawner (wired by the binary), so it is
// deliberately not part of this interface.
type HookDispatcher interface {
	// PreToolUse returns a deny to block a pending tool call.
	PreToolUse(ctx context.Context, toolName, argsJSON string) pluginhooks.Decision
	// PostToolUse returns an optional content/error override for a
	// finished tool call (a deny maps to IsError + reason).
	PostToolUse(ctx context.Context, toolName, argsJSON, resultText string) (overrideContent *string, isError *bool)
	// UserPromptSubmit returns a deny to block a submitted prompt.
	UserPromptSubmit(ctx context.Context, prompt string) pluginhooks.Decision
	// SessionStart fires once when a session begins.
	SessionStart(ctx context.Context)
	// Stop fires when a run completes.
	Stop(ctx context.Context)
	// PreCompact fires before conversation history is compacted.
	PreCompact(ctx context.Context)
}

// pluginPreToolUseGate runs the trusted plugins' PreToolUse hooks as the
// final before-dispatch gate: it fires only for calls nib's own gates
// have already cleared, mirroring CC where PreToolUse is the last gate
// before execution.
//
// A hook deny maps to a Block — the foundation synthesizes an error
// tool-result the model observes — and NEVER to a returned error. A
// returned hook error is terminal in the foundation loop (it ends the
// whole run), so a plugin must not be able to kill a run by denying a
// single tool call. A nil dispatcher (no plugins, or unit tests that
// build the Agent directly) proceeds.
func (a *Agent) pluginPreToolUseGate(ctx context.Context, c upagent.BeforeToolCallInput) upagent.BeforeToolCallResult {
	if a.hooks == nil {
		return upagent.BeforeToolCallResult{}
	}
	if dec := a.hooks.PreToolUse(ctx, c.Name, c.Args); dec.Deny {
		return upagent.BeforeToolCallResult{Block: true, Reason: dec.Reason}
	}
	return upagent.BeforeToolCallResult{}
}

// pluginPostToolUse applies the trusted plugins' PostToolUse hooks to a
// finalized result. A deny flips the result to an error and replaces its
// content with the hook's reason; this takes precedence over any earlier
// override (e.g. the lifecycle auto-complete trailer), since a denied
// result is an error and the trailer is moot. v1 performs no other
// content substitution. Proceed (and a nil dispatcher) leaves res
// untouched.
//
// The hook sees the tool's ORIGINAL output (c.Result.Content), not a
// prior in-flight override, so its decision reflects what the tool
// actually produced.
func (a *Agent) pluginPostToolUse(ctx context.Context, c upagent.AfterToolCallInput, res upagent.AfterToolCallResult) upagent.AfterToolCallResult {
	if a.hooks == nil {
		return res
	}
	content, isErr := a.hooks.PostToolUse(ctx, c.Name, c.Args, c.Result.Content)
	// Each override is independent (mirroring [upagent.AfterToolCallResult]'s
	// per-field pointer semantics): apply whichever the hook returned. The
	// v1 dispatcher only ever returns both together (deny → reason + error)
	// or neither, but honoring them separately keeps this correct if a hook
	// ever sets content alone.
	if content != nil {
		res.Content = content
	}
	if isErr != nil {
		res.IsError = isErr
	}
	return res
}

// pluginSessionStart fires the trusted plugins' SessionStart hooks once
// per conversation (a fresh [Agent.RunWithMode], per CC's per-session
// semantics). Fire-and-forget — v1 does not inject any returned context.
// A nil dispatcher is a no-op.
func (a *Agent) pluginSessionStart(ctx context.Context) {
	if a.hooks == nil {
		return
	}
	a.hooks.SessionStart(ctx)
}

// pluginUserPromptSubmit runs the trusted plugins' UserPromptSubmit hooks
// for a submitted prompt (the initial goal, or a [Agent.Reply] follow-up).
// A deny blocks the prompt: the caller surfaces the reason and never
// submits it to the model. A nil dispatcher proceeds.
func (a *Agent) pluginUserPromptSubmit(ctx context.Context, prompt string) pluginhooks.Decision {
	if a.hooks == nil {
		return pluginhooks.Decision{}
	}
	return a.hooks.UserPromptSubmit(ctx, prompt)
}

// pluginStop fires the trusted plugins' Stop hooks at run completion (the
// AgentDone path). Fire-and-forget: v1 picks run-completion semantics and
// does not let a Stop hook keep the agent running. A nil dispatcher is a
// no-op.
func (a *Agent) pluginStop(ctx context.Context) {
	if a.hooks == nil {
		return
	}
	a.hooks.Stop(ctx)
}

// pluginPreCompact fires the trusted plugins' PreCompact hooks just
// before conversation history is compacted (manual /compact or the
// per-Stream auto-compaction). A nil dispatcher is a no-op.
func (a *Agent) pluginPreCompact(ctx context.Context) {
	if a.hooks == nil {
		return
	}
	a.hooks.PreCompact(ctx)
}

// promptBlockedReason renders the user-facing message for a prompt a
// UserPromptSubmit hook denied, falling back to a generic line when the
// hook gave no reason.
func promptBlockedReason(reason string) string {
	if strings.TrimSpace(reason) == "" {
		return "Prompt blocked by a plugin hook."
	}
	return reason
}
