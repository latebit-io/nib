// Package smoke provides the smoke-run primitive: a thin wrapper over
// [proc.Run] that executes the project's resolved smoke command and
// renders the structured result for LLM consumption. Two layers
// consume this package:
//
//   - The smoke_run LLM tool ([coding/tools.SmokeRunTool]) delegates
//     to [RunSmoke] and [FormatSmokeResult] inside its Execute path.
//   - The agent's post-task auto-invocation
//     ([coding/agent.Agent.runSmokeReview]) calls the same pair
//     directly so a task marked complete is verified by the same
//     command the LLM would have invoked itself.
//
// Keeping the runner here (rather than inside `coding/tools`)
// separates the primitive from its LLM adapter and lets either path
// evolve without dragging the other.
package smoke

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/latebit-io/junto/engine/run/proc"
	"github.com/latebit-io/junto/engine/runconfig"
)

// RunSmoke invokes the configured smoke command via [proc.Run]. The
// effective timeout comes from [runconfig.EffectiveTimeout] — a single
// source of truth shared with [FormatSmokeResult] so the rendered
// "Timed out after X" message matches the actual deadline proc
// enforced.
func RunSmoke(ctx context.Context, projectRoot string, cfg runconfig.Resolved) proc.Result {
	timeout := runconfig.EffectiveTimeout(cfg)
	// Log source/timeout but NOT cfg.Command — `.project/run.json`
	// may contain inline env assignments or auth flags. The
	// FormatSmokeResult comment lays out the same redaction policy
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

// FormatSmokeResult renders a proc.Result into LLM-facing text.
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
func FormatSmokeResult(cfg runconfig.Resolved, res proc.Result) string {
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
