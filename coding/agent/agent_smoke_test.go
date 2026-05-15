package agent

import (
	"testing"
	"time"

	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/engine/runconfig"
)

// TestAgent_SmokeRunUnregisteredWhenDisabled verifies the
// smoke-disabled env var is a complete kill-switch — when set, the
// LLM does NOT see smoke_run as an available tool, matching the
// auto-invocation suppression in runTaskReview. Pre-fix, the env
// var only gated the auto-run path; the LLM could still invoke
// smoke_run directly, defeating the disable in exactly the
// environments that set the flag.
func TestAgent_SmokeRunUnregisteredWhenDisabled(t *testing.T) {
	t.Setenv(brand.EnvKeySmokeDisabled, "1")

	ag := New(&multiTurnProvider{}, stubWorkspace{},
		&NewOptions{SmokeConfig: runconfig.Resolved{
			Command: "make smoke",
			Source:  "test",
			Timeout: 5 * time.Second,
		}})

	if _, ok := ag.tools["smoke_run"]; ok {
		t.Errorf("smoke_run registered with %s set; want absent", brand.EnvKeySmokeDisabled)
	}
	for _, def := range ag.toolDefs {
		if def.Function.Name == "smoke_run" {
			t.Errorf("smoke_run advertised in tool defs with %s set", brand.EnvKeySmokeDisabled)
		}
	}
}

// TestAgent_SmokeRunRegisteredWhenEnabled verifies the positive
// path: with no kill-switch and a resolved smoke config, the tool
// IS registered. Guards against an over-aggressive future tightening
// of appendSmokeTool that accidentally disables the happy path.
func TestAgent_SmokeRunRegisteredWhenEnabled(t *testing.T) {
	// Explicitly clear in case the test runner inherited it.
	t.Setenv(brand.EnvKeySmokeDisabled, "")

	ag := New(&multiTurnProvider{}, stubWorkspace{},
		&NewOptions{SmokeConfig: runconfig.Resolved{
			Command: "make smoke",
			Source:  "test",
			Timeout: 5 * time.Second,
		}})

	if _, ok := ag.tools["smoke_run"]; !ok {
		t.Errorf("smoke_run not registered without kill-switch; want present")
	}
}
