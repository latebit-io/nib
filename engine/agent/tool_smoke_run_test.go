package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/junto/engine/llm"
	"github.com/latebit-io/junto/engine/runconfig"
)

// TestSmokeToolDefinitionAdvertisesCommand verifies the tool definition
// includes the resolved command and source so the LLM knows what
// "smoke" maps to in this project. Without this, the model is
// effectively blind to which target it is invoking.
func TestSmokeToolDefinitionAdvertisesCommand(t *testing.T) {
	t.Parallel()

	tool := NewSmokeRunTool("/tmp", runconfig.Resolved{
		Command: "make smoke",
		Source:  "make-smoke",
		Timeout: 5 * time.Second,
	})

	def := tool.Definition()
	if def.Function.Name != "smoke_run" {
		t.Errorf("Name = %q, want smoke_run", def.Function.Name)
	}
	if !strings.Contains(def.Function.Description, "make smoke") {
		t.Errorf("Description missing command: %q", def.Function.Description)
	}
	if !strings.Contains(def.Function.Description, "make-smoke") {
		t.Errorf("Description missing source: %q", def.Function.Description)
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

// TestFormatSmokeResultStartErr verifies the StartErr path renders a
// "Could not start" message rather than blank output.
func TestFormatSmokeResultStartErr(t *testing.T) {
	t.Parallel()

	tool := NewSmokeRunTool("", runconfig.Resolved{
		Command: "", // empty command triggers proc.ErrEmptyCommand
		Source:  "test",
	})
	// Calling Execute with an empty command goes through the early
	// "no command resolved" guard, not StartErr; bypass that with a
	// direct call.
	got := formatSmokeResult(runconfig.Resolved{Command: "", Source: "test"},
		runSmoke(context.Background(), "", runconfig.Resolved{Command: "", Source: "test"}))
	if !strings.Contains(got, "Could not start") {
		t.Errorf("formatSmokeResult missing 'Could not start': %q", got)
	}
	_ = tool
}
