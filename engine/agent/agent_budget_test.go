package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/llm"
)

// TestCheckTaskBudget_Disabled covers the four non-firing branches of
// [Agent.checkTaskBudget]: zero budget, negative-budget unlimited resolved
// by [New], stale runID, and already-latched. The check must return ""
// from each so the run loop never erroneously aborts a healthy turn.
func TestCheckTaskBudget_Disabled(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		opts *NewOptions
		// mutate runs after construction so the test can poke runID,
		// sessionUsage, or budgetExceeded into the state under test.
		mutate func(a *Agent)
	}{
		{
			name:   "zero budget skips check",
			opts:   &NewOptions{TaskTokenBudget: -1}, // -1 → unlimited (zero internally)
			mutate: func(a *Agent) { a.sessionUsage.TotalPromptTokens = 1_000_000 },
		},
		{
			name: "stale runID returns early",
			opts: &NewOptions{TaskTokenBudget: 100},
			mutate: func(a *Agent) {
				a.sessionUsage.TotalPromptTokens = 200
				a.runID = 5 // checkTaskBudget called with runID=0 below
			},
		},
		{
			name: "already-latched returns empty",
			opts: &NewOptions{TaskTokenBudget: 100},
			mutate: func(a *Agent) {
				a.sessionUsage.TotalPromptTokens = 200
				a.budgetExceeded = true
			},
		},
		{
			name:   "under cap returns empty",
			opts:   &NewOptions{TaskTokenBudget: 1000},
			mutate: func(a *Agent) { a.sessionUsage.TotalPromptTokens = 500 },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			events := make(chan event.Event, 4)
			ag := New(&multiTurnProvider{}, stubWorkspace{}, events, tc.opts)
			tc.mutate(ag)
			runID := ag.runID
			if tc.name == "stale runID returns early" {
				runID = 0 // intentionally mismatched
			}
			if msg := ag.checkTaskBudget(runID); msg != "" {
				t.Errorf("checkTaskBudget returned %q, want empty", msg)
			}
		})
	}
}

// TestCheckTaskBudget_FiresAndLatches verifies that a single check fires
// once when the budget is crossed, and subsequent checks return empty
// because budgetExceeded latches. The latch keeps an Resume after abort
// from emitting a duplicate AgentError.
func TestCheckTaskBudget_FiresAndLatches(t *testing.T) {
	t.Parallel()
	events := make(chan event.Event, 4)
	ag := New(&multiTurnProvider{}, stubWorkspace{}, events,
		&NewOptions{TaskTokenBudget: 1000})

	ag.sessionUsage.TotalPromptTokens = 800
	ag.sessionUsage.TotalCompletionTokens = 300 // 1100 > 1000
	ag.sessionUsage.Turns = 1

	msg := ag.checkTaskBudget(ag.runID)
	if msg == "" {
		t.Fatal("checkTaskBudget returned empty; want budget-exceeded message")
	}
	if !strings.Contains(msg, "1100") {
		t.Errorf("message missing used-tokens value: %q", msg)
	}
	if !strings.Contains(msg, "1000") {
		t.Errorf("message missing budget cap: %q", msg)
	}
	if !ag.budgetExceeded {
		t.Error("budgetExceeded did not latch after first overrun")
	}

	// Subsequent calls return empty even though usage is still over.
	if msg := ag.checkTaskBudget(ag.runID); msg != "" {
		t.Errorf("second checkTaskBudget returned %q, want empty (latched)", msg)
	}
}

// TestAgent_TokenBudget_AbortsRun drives the agent through a turn whose
// provider-reported usage crosses an explicit small budget, and verifies
// the run aborts with an AgentError before another LLM call. The test
// guards the regression that motivated the budget: a runaway loop that
// burned 55M+ tokens on a broken pacman game.
func TestAgent_TokenBudget_AbortsRun(t *testing.T) {
	t.Parallel()

	// One scripted turn reporting 1500 prompt tokens via Usage. The default
	// budget is irrelevant — we override to 500 so the first turn alone
	// exceeds the cap.
	provider := &multiTurnProvider{
		turns: [][]llm.StreamEvent{
			{
				{Token: "thinking..."},
				{
					Done: true,
					Usage: &llm.Usage{
						PromptTokens:     1500,
						CompletionTokens: 50,
					},
				},
			},
		},
	}

	events := make(chan event.Event, 64)
	ag := New(provider, stubWorkspace{}, events, &NewOptions{
		TaskTokenBudget: 500,
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ag.RunWithMode(ctx, "main.go", "", "go", nil, ModeExecution)

	errEv := drainUntil(t, events, 2*time.Second, func(ev event.Event) bool {
		_, ok := ev.(event.AgentError)
		return ok
	})
	if errEv == nil {
		t.Fatal("timeout waiting for AgentError after budget exceeded")
	}
	msg := errEv.(event.AgentError).Err
	if !strings.Contains(msg, "budget exceeded") {
		t.Errorf("AgentError = %q, want substring 'budget exceeded'", msg)
	}

	doneEv := drainUntil(t, events, 2*time.Second, func(ev event.Event) bool {
		_, ok := ev.(event.AgentDone)
		return ok
	})
	if doneEv == nil {
		t.Fatal("timeout waiting for AgentDone after budget abort")
	}
	if doneEv.(event.AgentDone).Success {
		t.Error("AgentDone.Success = true, want false on budget abort")
	}

	// The provider must have been called exactly once — the abort cuts the
	// run before a second LLM round-trip can commit more tokens.
	provider.mu.Lock()
	calls := provider.call
	provider.mu.Unlock()
	if calls != 1 {
		t.Errorf("provider calls = %d, want 1 (no second turn after abort)", calls)
	}
}

// TestAgent_TokenBudget_DefaultApplied checks that when NewOptions.TaskTokenBudget
// is left zero, [New] resolves it to [defaultTaskTokenBudget] rather than
// leaving the budget disabled. Disabling the budget by default would make
// the safety net silently absent — the regression guard would not fire.
func TestAgent_TokenBudget_DefaultApplied(t *testing.T) {
	t.Parallel()
	events := make(chan event.Event, 1)
	ag := New(&multiTurnProvider{}, stubWorkspace{}, events, nil)
	if ag.taskTokenBudget != defaultTaskTokenBudget {
		t.Errorf("taskTokenBudget = %d, want default %d",
			ag.taskTokenBudget, defaultTaskTokenBudget)
	}
}

// TestAgent_TokenBudget_NegativeMeansUnlimited covers the explicit opt-out:
// passing a negative budget resolves to zero (the disable sentinel) so a
// developer who knows what they're doing can turn the safety net off.
func TestAgent_TokenBudget_NegativeMeansUnlimited(t *testing.T) {
	t.Parallel()
	events := make(chan event.Event, 1)
	ag := New(&multiTurnProvider{}, stubWorkspace{}, events,
		&NewOptions{TaskTokenBudget: -1})
	if ag.taskTokenBudget != 0 {
		t.Errorf("taskTokenBudget = %d, want 0 (unlimited)", ag.taskTokenBudget)
	}
}
