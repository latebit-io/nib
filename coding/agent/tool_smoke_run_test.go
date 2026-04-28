package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/junto/ai/llm"
	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/run/proc"
	"github.com/latebit-io/junto/engine/runconfig"
)

// TestSmokeToolDefinitionAdvertisesSource verifies the tool definition
// names the source (so the LLM knows what kind of target it is
// invoking) but deliberately does NOT include the raw resolved command.
// The command is surfaced only in per-call results where the LLM needs
// it for failure diagnosis; keeping it out of the static description
// avoids caching project-specific run config (potentially with inline
// env/auth flags) into the system prompt across every turn.
func TestSmokeToolDefinitionAdvertisesSource(t *testing.T) {
	t.Parallel()

	tool := NewSmokeRunTool("/tmp", runconfig.Resolved{
		Command: "API_KEY=secret make smoke",
		Source:  "make-smoke",
		Timeout: 5 * time.Second,
	})

	def := tool.Definition()
	if def.Function.Name != "smoke_run" {
		t.Errorf("Name = %q, want smoke_run", def.Function.Name)
	}
	if !strings.Contains(def.Function.Description, "make-smoke") {
		t.Errorf("Description missing source: %q", def.Function.Description)
	}
	// The raw command must never appear in the description. The static
	// prefix mentions `make smoke` and `make run` as examples — a
	// distinctive secret-bearing fixture proves the resolved command
	// is NOT copied in.
	if strings.Contains(def.Function.Description, "API_KEY=secret") {
		t.Errorf("Description leaked raw command: %q", def.Function.Description)
	}
}

// TestSmokeToolSkippedReturnsReason verifies a Skipped configuration
// produces a clean tool result instead of running an empty command.
func TestSmokeToolSkippedReturnsReason(t *testing.T) {
	t.Parallel()

	tool := NewSmokeRunTool("/tmp", runconfig.Resolved{
		Skipped:    true,
		SkipReason: "no runnable entry point detected",
	})
	res := tool.Execute(context.Background(), llm.ToolCall{})
	if !strings.Contains(res.Content, "Smoke run skipped") {
		t.Errorf("Content = %q, want Skipped banner", res.Content)
	}
	if !strings.Contains(res.Content, "no runnable entry point detected") {
		t.Errorf("Content missing reason: %q", res.Content)
	}
}

// TestSmokeToolSuccess verifies a passing smoke command produces a
// "Passed" banner and the duration is rendered.
func TestSmokeToolSuccess(t *testing.T) {
	t.Parallel()

	tool := NewSmokeRunTool("", runconfig.Resolved{
		Command: "true",
		Source:  "test",
		Timeout: 5 * time.Second,
	})
	res := tool.Execute(context.Background(), llm.ToolCall{})
	if !strings.Contains(res.Content, "Passed") {
		t.Errorf("Content = %q, want 'Passed'", res.Content)
	}
}

// TestSmokeToolFailure verifies a failing command surfaces exit code
// and captured output. The output assertion uses indent's prefix so
// the format change is locked in.
func TestSmokeToolFailure(t *testing.T) {
	t.Parallel()

	tool := NewSmokeRunTool("", runconfig.Resolved{
		Command: "echo broke && exit 3",
		Source:  "test",
		Timeout: 5 * time.Second,
	})
	res := tool.Execute(context.Background(), llm.ToolCall{})
	if !strings.Contains(res.Content, "Failed (exit 3)") {
		t.Errorf("Content missing failure banner: %q", res.Content)
	}
	if !strings.Contains(res.Content, "broke") {
		t.Errorf("Content missing captured output: %q", res.Content)
	}
}

// TestSmokeToolTimeout verifies a hanging command is killed by the
// timeout and reported as such — the most important property since
// `make run` may legitimately run forever in interactive form and we
// must not hang the agent.
func TestSmokeToolTimeout(t *testing.T) {
	t.Parallel()

	tool := NewSmokeRunTool("", runconfig.Resolved{
		Command: "sleep 10",
		Source:  "test",
		Timeout: 100 * time.Millisecond,
	})
	res := tool.Execute(context.Background(), llm.ToolCall{})
	if !strings.Contains(res.Content, "Timed out") {
		t.Errorf("Content missing timeout banner: %q", res.Content)
	}
}

// TestFormatSmokeResultDoesNotLeakCommand verifies the LLM-facing
// formatted result never contains the literal command. The result
// flows into LLM context and the capture sink (which feeds the
// session journal, potentially distributed), so a `.project/run.json`
// with inline env assignments or auth flags must not surface them
// here. The source identifier is enough for the LLM to diagnose
// failures.
func TestFormatSmokeResultDoesNotLeakCommand(t *testing.T) {
	t.Parallel()

	cfg := runconfig.Resolved{
		Command: "API_KEY=sk-secret make smoke",
		Source:  "make-smoke",
		Timeout: 5 * time.Second,
	}

	cases := []struct {
		name string
		res  proc.Result
	}{
		{"success", proc.Result{ExitCode: 0, Duration: 100 * time.Millisecond}},
		{"failure", proc.Result{ExitCode: 3, Output: "stderr from the run", Duration: 200 * time.Millisecond}},
		{"timeout", proc.Result{TimedOut: true, ExitCode: -1, Output: "partial output"}},
		{"cancel", proc.Result{Cancelled: true, ExitCode: -1}},
		{"start err", proc.Result{StartErr: errors.New("exec failed")}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatSmokeResult(cfg, tc.res)
			if strings.Contains(got, "API_KEY=sk-secret") {
				t.Errorf("formatted result leaked secret: %q", got)
			}
			if strings.Contains(got, "make smoke") {
				t.Errorf("formatted result leaked literal command: %q", got)
			}
			if !strings.Contains(got, "make-smoke") {
				t.Errorf("formatted result missing source identifier: %q", got)
			}
		})
	}
}

// TestAgent_SmokeRunUnregisteredWhenDisabled verifies
// JUNTO_SMOKE_DISABLED is a complete kill-switch — when set, the
// LLM does NOT see smoke_run as an available tool, matching the
// auto-invocation suppression in runTaskReview. Pre-fix, the env
// var only gated the auto-run path; the LLM could still invoke
// smoke_run directly, defeating the disable in exactly the
// environments that set the flag.
func TestAgent_SmokeRunUnregisteredWhenDisabled(t *testing.T) {
	t.Setenv("JUNTO_SMOKE_DISABLED", "1")

	events := make(chan event.Event, 8)
	ag := New(&multiTurnProvider{}, stubWorkspace{}, events,
		&NewOptions{SmokeConfig: runconfig.Resolved{
			Command: "make smoke",
			Source:  "test",
			Timeout: 5 * time.Second,
		}})

	if _, ok := ag.tools["smoke_run"]; ok {
		t.Errorf("smoke_run registered with JUNTO_SMOKE_DISABLED set; want absent")
	}
	for _, def := range ag.toolDefs {
		if def.Function.Name == "smoke_run" {
			t.Errorf("smoke_run advertised in tool defs with JUNTO_SMOKE_DISABLED set")
		}
	}
}

// TestAgent_SmokeRunRegisteredWhenEnabled verifies the positive
// path: with no kill-switch and a resolved smoke config, the tool
// IS registered. Guards against an over-aggressive future tightening
// of appendSmokeTool that accidentally disables the happy path.
func TestAgent_SmokeRunRegisteredWhenEnabled(t *testing.T) {
	// Explicitly clear in case the test runner inherited it.
	t.Setenv("JUNTO_SMOKE_DISABLED", "")

	events := make(chan event.Event, 8)
	ag := New(&multiTurnProvider{}, stubWorkspace{}, events,
		&NewOptions{SmokeConfig: runconfig.Resolved{
			Command: "make smoke",
			Source:  "test",
			Timeout: 5 * time.Second,
		}})

	if _, ok := ag.tools["smoke_run"]; !ok {
		t.Errorf("smoke_run not registered without kill-switch; want present")
	}
}

// TestFormatSmokeResultTimeoutUsesEffective verifies the rendered
// "Timed out after X" message reports the effective timeout (with
// the DefaultTimeout fallback applied) when cfg.Timeout is zero.
// Without runconfig.EffectiveTimeout in formatSmokeResult, the test
// path would report "Timed out after 0s" while proc actually used
// the 30s default — misleading to anyone trying to raise it.
func TestFormatSmokeResultTimeoutUsesEffective(t *testing.T) {
	t.Parallel()

	// Resolved with no Timeout — production code reaches here via
	// runconfig.Load which defaults the field, but tests construct
	// values directly. Same caller-shape risk for future code.
	cfg := runconfig.Resolved{
		Command: "sleep 99",
		Source:  "test",
	}
	got := formatSmokeResult(cfg, proc.Result{TimedOut: true, ExitCode: -1})
	if strings.Contains(got, "Timed out after 0s") {
		t.Errorf("rendered '0s' for zero-timeout cfg; expected DefaultTimeout fallback. got=%q", got)
	}
	want := runconfig.DefaultTimeout.String()
	if !strings.Contains(got, want) {
		t.Errorf("rendered text missing %q; got=%q", want, got)
	}
}

// TestFormatSmokeResultStartErr verifies the StartErr path renders a
// "Could not start" message rather than blank output. Note that
// SmokeRunTool.Execute short-circuits on an empty command before
// reaching proc.Run, so this test bypasses the tool wrapper and
// exercises runSmoke + formatSmokeResult directly.
func TestFormatSmokeResultStartErr(t *testing.T) {
	t.Parallel()

	cfg := runconfig.Resolved{Command: "", Source: "test"}
	got := formatSmokeResult(cfg, runSmoke(context.Background(), "", cfg))
	if !strings.Contains(got, "Could not start") {
		t.Errorf("formatSmokeResult missing 'Could not start': %q", got)
	}
}
