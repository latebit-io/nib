package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	upagent "github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/approval"
	"github.com/latebit-io/nib/kit/cmdallow"
	"github.com/latebit-io/nib/kit/tools/bash"
)

// Per-command approval gating for the top-level agent's bash tool.
//
// commandApprovalGate mirrors the edit-approval flow at the command
// boundary: before a bash command executes, the agent emits a critical
// [event.AgentCommandProposed] and blocks on the run's
// [approval.Coordinator] until the frontend approves or rejects. It is
// the interactive counterpart to [bashGrantGate] (which enforces a
// subagent's static grants and never prompts); the two are mutually
// exclusive — grants imply a child agent, and a child must never block
// on an approval surface it has no frontend for.
//
// The shared Coordinator carries at most one pending decision at a
// time. That invariant holds because tool dispatch is serial: an edit
// proposal and a command proposal cannot be in flight together. If
// dispatch ever parallelizes, this gate (and the edit orchestrator)
// need per-proposal routing.
//
// Dark-launched behind [NewOptions.ApproveBashCommands]
// (brand.EnvKeyBashApproval at the composition roots) until the TUI
// approval surface ships.

// proposeCommandFunc runs the interactive approval flow for a single
// command. reason is the guard classification that made the command
// approval-worthy ("" for a plain command that is simply not
// allowlisted). Returns approved=true when the command may execute;
// otherwise body is the tool result the LLM sees (rejection note or
// delivery/cancellation error) and isError marks the fatal paths.
type proposeCommandFunc func(ctx context.Context, id, command, reason string) (approved bool, body string, isError bool)

// commandApprovalGate wraps the bash tool so each command is proposed
// to the developer before it runs, unless the always-allow list
// permits it. A rejected command yields a normal tool result (the LLM
// sees the rejection and adapts); only delivery failure and
// cancellation surface as tool errors.
type commandApprovalGate struct {
	inner Tool
	// allow is the persisted always-allow list. Nil-safe (nil permits
	// nothing, so every command is proposed).
	allow   *cmdallow.List
	propose proposeCommandFunc
}

// Definition forwards the wrapped tool's schema unchanged.
func (g commandApprovalGate) Definition() llm.ToolDef { return g.inner.Definition() }

// Execute proposes the command for approval, then delegates to the
// wrapped bash tool when approved.
func (g commandApprovalGate) Execute(ctx context.Context, call llm.ToolCall) upagent.ToolResult {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || strings.TrimSpace(args.Command) == "" {
		// Unparseable or empty arguments are the wrapped tool's contract
		// to reject; forward so it produces its own validation error
		// rather than proposing a blank command for approval.
		return g.inner.Execute(ctx, call)
	}
	class, _ := bash.Classify(args.Command)
	if class == bash.GuardSearch {
		// The search guard is a hard redirect to structured tools —
		// approval cannot improve the outcome, so skip the proposal and
		// let the tool produce its own refusal message.
		return g.inner.Execute(ctx, call)
	}
	if g.allow.Permits(args.Command) {
		slog.Debug("command auto-approved by allowlist", "command", args.Command)
		return g.inner.Execute(ctx, call)
	}
	approved, body, isError := g.propose(ctx, call.ID, args.Command, class.Reason())
	if !approved {
		return upagent.ToolResult{Content: body, IsError: isError}
	}
	return g.inner.Execute(ctx, call)
}

// PromptGuidelines forwards the gated tool's prompt guidance
// (kit.PromptContributor) — the approval gate intercepts execution,
// not the tool's prompt-level self-documentation.
func (g commandApprovalGate) PromptGuidelines() []string {
	return kit.ToolPromptGuidelines(g.inner)
}

// proposeCommand runs the interactive approval flow for a bash command:
// deliver [event.AgentCommandProposed] (critical), block on the run's
// Coordinator, translate the decision. Mirrors the edit flow in
// editflow.Orchestrator minus validation and cache seeding, which have
// no command equivalent. The run's Coordinator is recovered from ctx
// (stashed by RunWithMode/Reply via [ctxWithCoord]) for the same
// run-handoff-race reason as [Agent.Propose].
func (a *Agent) proposeCommand(ctx context.Context, id, command, reason string) (approved bool, body string, isError bool) {
	coord := coordFromCtx(ctx)
	if coord == nil {
		return false, "Error: command proposal received without a run-scoped approval coordinator (no active run?)", true
	}

	a.send(event.AgentStatus{Status: event.StatusReviewing})

	// Critical: if the frontend never sees the proposal, the await
	// below blocks forever with nothing for the developer to decide.
	if err := a.sendCritical(ctx, event.AgentCommandProposed{
		Command: event.PendingCommand{ID: id, Command: command, Reason: reason},
	}); err != nil {
		slog.Error("command proposal delivery failed", "err", err)
		return false, fmt.Sprintf("Error: could not deliver command proposal to frontend: %v", err), true
	}

	decision, err := coord.AwaitApproval(ctx)
	a.send(event.AgentStatus{Status: event.StatusThinking})
	if err != nil {
		if errors.Is(err, approval.ErrChannelClosed) {
			return false, "Error: approval channel closed", true
		}
		return false, "Error: agent canceled", true
	}
	if decision.Approved {
		return true, "", false
	}
	a.send(event.AgentToken{Text: "\n[Command rejected]\n\n"})
	return false, "The developer rejected this command. Try a different approach or move on.", false
}
