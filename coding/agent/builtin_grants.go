package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/latebit-io/nib/kit"
	"log/slog"
	"strings"

	upagent "github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit/toolperm"
)

// Built-in tool gating for child (subagent) agents.
//
// A subagent definition's `tools:` / `disallowedTools:` grants must
// constrain the BUILT-IN tools (read_file, bash, …) the agent registers
// internally — something the spawner cannot do from outside, since
// builtins never leave [Agent.registerTools]. [NewOptions.BuiltinToolGrants]
// carries a nib-keyed [toolperm.Matcher] here; gateBuiltins enforces it.
//
// Enforcement is two-level: tool-level (drop a builtin the grants do not
// admit, so the LLM never sees it) and command-level (wrap bash so each
// command is checked against the grant's argument patterns — the place
// `Bash(git *)` actually constrains a child).

// gateBuiltins filters builtins to those the grants admit and wraps the
// bash tool with a per-command gate. The top-level agent passes nil grants
// and never calls this.
func gateBuiltins(builtins []Tool, grants *toolperm.Matcher) []Tool {
	out := make([]Tool, 0, len(builtins))
	for _, t := range builtins {
		name := strings.ToLower(t.Definition().Function.Name)
		if !builtinGranted(grants, name) {
			slog.Debug("subagent: built-in tool dropped by grants", "tool", name)
			continue
		}
		if name == "bash" {
			t = bashGrantGate{inner: t, grants: grants}
		}
		out = append(out, t)
	}
	return out
}

// builtinGranted reports whether a built-in tool may be registered under
// the grants. A bare deny (`disallowedTools: Bash`) removes it; with an
// allow list present only named tools survive; a deny-only grant admits
// every tool it does not name.
func builtinGranted(grants *toolperm.Matcher, name string) bool {
	if grants.DeniesTool(name) {
		return false
	}
	if grants.HasAllowList() {
		return grants.GrantsTool(name)
	}
	return true
}

// bashGrantGate wraps the bash tool so each command is checked against the
// child's grants before it runs. A blocked command yields an error
// ToolResult (the LLM sees the rejection and can adapt) — never a Go error,
// which would terminate the run.
type bashGrantGate struct {
	inner  Tool
	grants *toolperm.Matcher
}

// Definition forwards the wrapped tool's schema unchanged.
func (g bashGrantGate) Definition() llm.ToolDef { return g.inner.Definition() }

// Execute gates the command, then delegates to the wrapped bash tool.
func (g bashGrantGate) Execute(ctx context.Context, call llm.ToolCall) upagent.ToolResult {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		// Unparseable arguments are the wrapped tool's contract to reject;
		// forward so it produces its own validation error rather than a
		// confusing grant denial.
		return g.inner.Execute(ctx, call)
	}
	// When the grant scopes bash by argument (e.g. Bash(git *) or
	// disallowedTools: Bash(rm *)), the glob is checked against the whole
	// command string — which a compound command defeats: `git status; rm
	// -rf /` matches `git *`, and `true; rm -rf /` evades `rm *`. A single
	// arg-glob can only be trusted against one simple command, so reject
	// shell control/substitution operators conservatively. A bare `Bash`
	// grant imposes no arg rules and is left unrestricted.
	if g.grants.HasArgRules("bash") && toolperm.HasShellControl(args.Command) {
		return upagent.ToolResult{
			Content: "Command blocked: compound, piped, or substituted shell commands are not permitted under this subagent's argument-scoped bash grant. Run a single command.",
			IsError: true,
		}
	}
	if !g.grants.PermitsArg("bash", args.Command) {
		return upagent.ToolResult{
			Content: fmt.Sprintf("Command blocked by this subagent's tool grants: %q is not permitted.", args.Command),
			IsError: true,
		}
	}
	return g.inner.Execute(ctx, call)
}

// PromptGuidelines forwards the gated tool's prompt guidance
// (kit.PromptContributor) — the grant gate restricts execution, not
// the tool's prompt-level self-documentation.
func (g bashGrantGate) PromptGuidelines() []string {
	return kit.ToolPromptGuidelines(g.inner)
}
