package smoke

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/nib/coding/runconfig"
	"github.com/latebit-io/nib/kit/proc"
)

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
			got := FormatSmokeResult(cfg, tc.res)
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

// TestFormatSmokeResultTimeoutUsesEffective verifies the rendered
// "Timed out after X" message reports the effective timeout (with
// the DefaultTimeout fallback applied) when cfg.Timeout is zero.
// Without runconfig.EffectiveTimeout in FormatSmokeResult, the test
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
	got := FormatSmokeResult(cfg, proc.Result{TimedOut: true, ExitCode: -1})
	if strings.Contains(got, "Timed out after 0s") {
		t.Errorf("rendered '0s' for zero-timeout cfg; expected DefaultTimeout fallback. got=%q", got)
	}
	want := runconfig.DefaultTimeout.String()
	if !strings.Contains(got, want) {
		t.Errorf("rendered text missing %q; got=%q", want, got)
	}
}

// TestFormatSmokeResultStartErr verifies the StartErr path renders a
// "Could not start" message rather than blank output. Note that the
// smoke_run tool's Execute short-circuits on an empty command before
// reaching proc.Run, so this test bypasses the tool wrapper and
// exercises RunSmoke + FormatSmokeResult directly.
func TestFormatSmokeResultStartErr(t *testing.T) {
	t.Parallel()

	cfg := runconfig.Resolved{Command: "", Source: "test"}
	got := FormatSmokeResult(cfg, RunSmoke(context.Background(), "", cfg))
	if !strings.Contains(got, "Could not start") {
		t.Errorf("FormatSmokeResult missing 'Could not start': %q", got)
	}
}
