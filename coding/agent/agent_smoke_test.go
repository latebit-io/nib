package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/coding/event"
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

// TestAgent_SmokeReviewEmitsSmokeStatusBeforeRun locks the status-phase
// fix: runSmokeReview must emit StatusSmoke BEFORE the smoke banner (and
// thus before the blocking smoke command). Without it the frontend holds
// the prior StatusLinting for the lifetime of the launched process, so the
// badge reads "LINTING" while the artifact is actually being smoke-run.
func TestAgent_SmokeReviewEmitsSmokeStatusBeforeRun(t *testing.T) {
	a := &Agent{
		bus:       newBus(),
		workspace: stubWorkspace{},
		smokeConfig: runconfig.Resolved{
			Command: "true",
			Source:  "test",
			Timeout: 5 * time.Second,
		},
	}
	events := subscribeForTest(t, a)

	if msg := a.runSmokeReview(context.Background()); msg == "" {
		t.Fatal("runSmokeReview returned empty result for a non-skipped config")
	}

	// runSmokeReview publishes synchronously, so all events are buffered by
	// the time it returns; drain with a short deadline as a safety net.
	statusIdx, bannerIdx := -1, -1
	for i := 0; ; i++ {
		select {
		case ev := <-events:
			switch e := ev.(type) {
			case event.AgentStatus:
				if e.Status == event.StatusSmoke && statusIdx < 0 {
					statusIdx = i
				}
			case event.AgentToken:
				if strings.Contains(e.Text, "Smoke run") && bannerIdx < 0 {
					bannerIdx = i
				}
			}
		case <-time.After(time.Second):
			goto drained
		}
	}
drained:
	if statusIdx < 0 {
		t.Fatal("runSmokeReview did not emit StatusSmoke")
	}
	if bannerIdx < 0 {
		t.Fatal("runSmokeReview did not emit the smoke banner")
	}
	if statusIdx > bannerIdx {
		t.Errorf("StatusSmoke emitted after the smoke banner (status=%d, banner=%d); phase must flip before the run", statusIdx, bannerIdx)
	}
}
