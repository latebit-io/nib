package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit/dyncontext"
)

// bashToolRunner is the [dyncontext.Runner] the agent binds into shell-
// bearing extra tools (skill dynamic-context directives). Instead of
// spawning `sh -c` directly, it issues the command to the agent's own
// registered bash tool, so a directive is indistinguishable from a
// model-issued bash call at every gate: per-command approval and the
// always-allow list on the top-level agent, static grants on a
// subagent, and the bash guards in between. A nil tool (a child whose
// grants dropped bash entirely) refuses every directive.
type bashToolRunner struct {
	tool Tool
	seq  atomic.Uint64
}

// directiveCallPrefix namespaces the synthetic tool-call ids the runner
// mints; the id only labels the approval proposal in the frontend.
const directiveCallPrefix = "directive-"

// newBashToolRunner returns a runner over tool; tool may be nil.
func newBashToolRunner(tool Tool) *bashToolRunner {
	return &bashToolRunner{tool: tool}
}

// Run implements [dyncontext.Runner] by executing command through the
// bound bash tool. A tool-level error result (guard block, grant
// denial, rejected approval) is returned as a Go error so the directive
// renders as a visible `[error: …]` marker rather than inlining a
// refusal message as if it were command output.
func (r *bashToolRunner) Run(ctx context.Context, command string) (string, error) {
	if r.tool == nil {
		return "", errors.New("shell is not available to this agent")
	}
	args, err := json.Marshal(struct {
		Command string `json:"command"`
	}{Command: command})
	if err != nil {
		return "", fmt.Errorf("encode directive: %w", err)
	}
	call := llm.ToolCall{
		ID:       fmt.Sprintf("%s%d", directiveCallPrefix, r.seq.Add(1)),
		Type:     "function",
		Function: llm.FunctionCall{Name: "bash", Arguments: string(args)},
	}
	res := r.tool.Execute(ctx, call)
	if res.IsError {
		return "", errors.New(res.Content)
	}
	return res.Content, nil
}

// Compile-time assertion that bashToolRunner satisfies dyncontext.Runner.
var _ dyncontext.Runner = (*bashToolRunner)(nil)
