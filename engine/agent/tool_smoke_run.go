package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/latebit-io/junto/ai/llm"
	"github.com/latebit-io/junto/engine/run/proc"
	"github.com/latebit-io/junto/engine/runconfig"
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
	desc := "Run the project's smoke command (typically `make smoke` or `make run`) " +
		"to verify the artifact actually launches and does not crash on startup. " +
		"Use this before claiming a task is complete on a runnable project. " +
		"On failure, the stack trace and exit code are returned so you can fix issues " +
		"the static validators (parser, lint, architecture) cannot catch — runtime " +
		"errors, missing initialisation, broken wiring."

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

	res := runSmoke(ctx, t.projectRoot, t.cfg)
	return textResult(formatSmokeResult(t.cfg, res))
}

// runSmoke invokes the configured smoke command via [proc.Run]. The
// effective timeout comes from runconfig.EffectiveTimeout — a single
// source of truth shared with formatSmokeResult so the rendered
// "Timed out after X" message matches the actual deadline proc
// enforced.
func runSmoke(ctx context.Context, projectRoot string, cfg runconfig.Resolved) proc.Result {
	timeout := runconfig.EffectiveTimeout(cfg)
	// Log source/timeout but NOT cfg.Command — `.project/run.json`
	// may contain inline env assignments or auth flags. The
	// formatSmokeResult comment lays out the same redaction policy
	// for LLM-facing/persisted surfaces; debug logs can flow to
	// JUNTO_LOG files, bug reports, and CI captures, so they get
	// the same treatment. cfg.Source identifies which command ran.
	slog.Debug("smoke: running", "source", cfg.Source, "timeout", timeout)

	res := proc.Run(ctx, proc.Request{
		Shell:   cfg.Command,
		Dir:     projectRoot,
		Timeout: timeout,
	})

	slog.Debug("smoke: completed",
		"source", cfg.Source, "exit", res.ExitCode,
		"timed_out", res.TimedOut, "duration", res.Duration)
	return res
}

// formatSmokeResult renders a proc.Result into LLM-facing text.
//
// The header deliberately surfaces the source identifier (make-smoke,
// lua-main, config, …) but NOT the resolved command. The result
// string flows into the LLM context (and onward to the model
// provider's logs) and into the capture sink (and onward to
// distributed session journals). `.project/run.json` is the developer's
// trust boundary, but the LLM-facing tool result is not — keep
// inline env assignments / auth flags out of persisted surfaces. The
// LLM has enough signal from the source label and captured output to
// diagnose failures without seeing the literal command.
func formatSmokeResult(cfg runconfig.Resolved, res proc.Result) string {
	header := fmt.Sprintf("Smoke run (%s)\n", cfg.Source)

	switch {
	case res.StartErr != nil:
		return header + "Could not start: " + res.StartErr.Error()
	case res.TimedOut:
		// Use EffectiveTimeout so a Resolved with a zero Timeout
		// (test paths bypassing runconfig.Load) reports the actual
		// deadline proc enforced rather than "Timed out after 0s".
		return header + fmt.Sprintf(
			"Timed out after %s. The command may run forever (e.g. an interactive UI). "+
				"Adjust .project/run.json `smoke_command` to a non-blocking smoke target, "+
				"or raise `timeout_ms` if the artifact legitimately takes longer to start.\n\n%s",
			runconfig.EffectiveTimeout(cfg), indent(res.Output))
	case res.Cancelled:
		return header + "Cancelled (developer interrupted)."
	case res.ExitCode == 0:
		return header + fmt.Sprintf("Passed in %s.", roundDuration(res.Duration))
	default:
		return header + fmt.Sprintf("Failed (exit %d) in %s. Captured output:\n\n%s",
			res.ExitCode, roundDuration(res.Duration), indent(res.Output))
	}
}

// indent prefixes every non-empty line of s with two spaces so it
// renders as a fenced block inside the tool result without needing
// markdown awareness in callers.
func indent(s string) string {
	if s == "" {
		return "  (no output)"
	}
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		b.WriteString("  ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// roundDuration rounds d to a human-readable precision so timing
// noise (microsecond-level) does not pollute the LLM's input.
func roundDuration(d time.Duration) time.Duration {
	switch {
	case d > time.Second:
		return d.Round(100 * time.Millisecond)
	case d > 100*time.Millisecond:
		return d.Round(10 * time.Millisecond)
	default:
		return d.Round(time.Millisecond)
	}
}
