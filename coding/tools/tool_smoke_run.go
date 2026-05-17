package tools

import (
	"context"
	"fmt"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/smoke"
	"github.com/latebit-io/nib/engine/runconfig"
)

// SmokeRunTool lets the LLM execute the project's smoke-run command —
// the answer to "I think I'm done; does the artifact actually launch?"
//
// Trust model: the smoke command comes from `.project/run.json` (or a
// language-default fallback), reviewed and committed alongside the rest
// of the project. Unlike [BashTool], smoke commands legitimately touch
// the filesystem (build artifacts, log files) and bypass
// [fileWriteGuard] for that reason. Reviewers vet the command at PR
// time, the same way they vet any build script.
//
// The runner primitives ([smoke.RunSmoke], [smoke.FormatSmokeResult])
// live in `coding/smoke` so the agent's post-task auto-invocation can
// reuse them without depending on the tool registration layer.
type SmokeRunTool struct {
	projectRoot string
	cfg         runconfig.Resolved
}

// NewSmokeRunTool creates a tool that runs the project's smoke command.
// The cfg should come from [runconfig.Load] at the composition root and
// must not be Skipped — callers gate registration on Skipped before
// constructing the tool.
func NewSmokeRunTool(projectRoot string, cfg runconfig.Resolved) *SmokeRunTool {
	return &SmokeRunTool{projectRoot: projectRoot, cfg: cfg}
}

// Definition returns the OpenAI-compatible tool schema for smoke_run.
//
// The description deliberately does NOT include the resolved command
// string — it is reused across every turn the tool is registered, and
// project run configs occasionally include inline env assignments or
// auth flags that should not be cached into the system prompt. The
// command IS surfaced to the LLM in the per-call tool result (where
// the LLM is already running it and needs to know what executed for
// failure diagnosis), and that path is governed by the documented
// .project/run.json trust boundary.
func (t *SmokeRunTool) Definition() llm.ToolDef {
	desc := "Run the project's smoke command (typically `make smoke` / `make run`) to verify the artifact launches. Call before claiming a runnable task complete. Failure returns stack trace + exit code."

	if t.cfg.Source != "" {
		desc += fmt.Sprintf(" (resolved via: %s)", t.cfg.Source)
	}

	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        "smoke_run",
			Description: desc,
			Parameters: llm.FunctionParams{
				Type:       "object",
				Properties: map[string]llm.FunctionParam{},
				Required:   []string{},
			},
		},
	}
}

// Execute runs the smoke command and returns the structured result.
// The tool ignores any arguments — smoke command resolution is a
// composition-root concern; per-call overrides would let the LLM
// silently change what "smoke" means and would defeat the trust
// boundary the .project/run.json review path establishes.
func (t *SmokeRunTool) Execute(ctx context.Context, _ llm.ToolCall) ToolResult {
	if t.cfg.Skipped {
		return textResult("Smoke run skipped: " + t.cfg.SkipReason)
	}
	if t.cfg.Command == "" {
		return textResult("Smoke run unavailable: no command resolved (check .project/run.json or add a Makefile)")
	}

	res := smoke.RunSmoke(ctx, t.projectRoot, t.cfg)
	return textResult(smoke.FormatSmokeResult(t.cfg, res))
}
