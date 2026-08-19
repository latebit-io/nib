package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/kit/budget"
)

// TestCheckTaskBudget_Disabled covers the three non-firing branches of
// [Agent.checkTaskBudget]: zero budget (disabled), already-latched, and
// under-cap. The check must return "" from each so a healthy turn is
// never erroneously aborted.
func TestCheckTaskBudget_Disabled(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		opts   *NewOptions
		mutate func(a *Agent)
	}{
		{
			name:   "zero budget skips check",
			opts:   &NewOptions{TaskTokenBudget: -1}, // -1 → unlimited (zero internally)
			mutate: func(a *Agent) { a.providerProxy.recordUsage(&llm.Usage{PromptTokens: 1_000_000}) },
		},
		{
			name: "already-latched returns empty",
			opts: &NewOptions{TaskTokenBudget: 100},
			mutate: func(a *Agent) {
				a.providerProxy.recordUsage(&llm.Usage{PromptTokens: 200})
				a.budgetExceeded = true
			},
		},
		{
			name:   "under cap returns empty",
			opts:   &NewOptions{TaskTokenBudget: 1000},
			mutate: func(a *Agent) { a.providerProxy.recordUsage(&llm.Usage{PromptTokens: 500}) },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ag := New(&multiTurnProvider{}, stubWorkspace{}, tc.opts)
			t.Cleanup(ag.Close)
			_ = subscribeForTest(t, ag)
			tc.mutate(ag)
			if msg := ag.checkTaskBudget(); msg != "" {
				t.Errorf("checkTaskBudget returned %q, want empty", msg)
			}
		})
	}
}

// TestCheckTaskBudget_FiresAndLatches verifies that a single check
// fires once when the budget is crossed and subsequent checks return
// empty because budgetExceeded latches. The latch keeps a Resume
// after abort from emitting a duplicate AgentError.
func TestCheckTaskBudget_FiresAndLatches(t *testing.T) {
	t.Parallel()
	ag := New(&multiTurnProvider{}, stubWorkspace{},
		&NewOptions{TaskTokenBudget: 1000})
	t.Cleanup(ag.Close)
	_ = subscribeForTest(t, ag)

	ag.providerProxy.recordUsage(&llm.Usage{PromptTokens: 800, CompletionTokens: 300}) // 1100 > 1000

	msg := ag.checkTaskBudget()
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

	if msg := ag.checkTaskBudget(); msg != "" {
		t.Errorf("second checkTaskBudget returned %q, want empty (latched)", msg)
	}
}

// TestAgent_TokenBudget_AbortsRun drives the agent through a turn
// whose provider-reported usage crosses an explicit small budget, and
// verifies the run aborts with an AgentError before another LLM call.
// The test guards the regression that motivated the budget: a runaway
// loop that burned 55M+ tokens on a broken pacman game.
//
// Post-cutover the abort flows through the foundation's
// TransformContext hook ([Agent.foundationBudgetCheck]) returning
// errBudgetExceeded; the foundation emits Error which the translator
// re-emits as AgentError + AgentDone(success=false). The user-visible
// shape (AgentError with "budget exceeded" substring + AgentDone with
// Success=false) is unchanged.
func TestAgent_TokenBudget_AbortsRun(t *testing.T) {
	t.Parallel()

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
			// Second scripted turn that must NEVER fire — the abort
			// fires on the next TransformContext (which precedes the
			// next Stream). If this turn runs the budget gate failed.
			{
				{Token: "should not see"},
				{Done: true},
			},
		},
	}

	ag := New(provider, stubWorkspace{}, &NewOptions{
		TaskTokenBudget: 500,
	})
	t.Cleanup(ag.Close)
	events := subscribeForTest(t, ag)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ag.RunWithMode(ctx, "main.go", "", "go", event.ModeExecution)

	errMsg, doneSuccess := collectAbortEvents(t, events, 2*time.Second)
	if !strings.Contains(errMsg, "budget exceeded") {
		t.Errorf("AgentError = %q, want substring 'budget exceeded'", errMsg)
	}
	if doneSuccess {
		t.Error("AgentDone.Success = true, want false on budget abort")
	}

	provider.mu.Lock()
	calls := provider.call
	provider.mu.Unlock()
	if calls != 1 {
		t.Errorf("provider calls = %d, want 1 (no second turn after abort)", calls)
	}
}

// collectAbortEvents drains the agent's event channel until both an
// [event.AgentError] and an [event.AgentDone] have been observed (or
// the timeout fires). Returns the AgentError's message and the
// AgentDone's Success flag. Order-tolerant: the two events may arrive
// in any sequence, and intervening events (tokens, status, edit
// proposals) are silently discarded. Tests construct the agent with
// no [FlushDirtyBuffersFunc] callback so the flush step is a no-op.
func collectAbortEvents(t *testing.T, events <-chan event.Event, timeout time.Duration) (errMsg string, doneSuccess bool) {
	t.Helper()
	var (
		errSeen  bool
		doneSeen bool
		deadline = time.After(timeout)
	)
	for !errSeen || !doneSeen {
		select {
		case ev := <-events:
			switch e := ev.(type) {
			case event.AgentError:
				if !errSeen {
					errMsg = e.Err
				}
				errSeen = true
			case event.AgentDone:
				doneSeen = true
				doneSuccess = e.Success
			}
		case <-deadline:
			if !errSeen {
				t.Fatal("timeout waiting for AgentError after budget exceeded")
			}
			if !doneSeen {
				t.Fatal("timeout waiting for AgentDone after budget abort")
			}
			return errMsg, doneSuccess
		}
	}
	return errMsg, doneSuccess
}

// TestAgent_TokenBudget_AbortsBetweenInnerStreams verifies the
// per-Stream budget gate. A single agent turn can call Stream
// multiple times (one per tool-call round-trip); the budget must
// abort BEFORE the next Stream — not just after the outer turn
// finishes. The scripted provider emits a tool call to a non-existent
// tool on the first Stream; the agent dispatches it (gets an error
// reply), then the foundation's TransformContext fires before the
// next Stream and aborts.
//
// Without the per-Stream gate, the run loop would loop indefinitely
// (or until truncation) and the run-end abort would only fire after
// sessionUsage caught up — many tokens too late.
func TestAgent_TokenBudget_AbortsBetweenInnerStreams(t *testing.T) {
	t.Parallel()
	provider := &multiTurnProvider{
		turns: [][]llm.StreamEvent{
			// Inner Stream #1: emits a tool call to a non-existent
			// tool. The dispatcher returns an error string; the loop
			// would normally call Stream again. Reports 1500 tokens —
			// already over the 500-token budget when committed.
			{
				{
					ToolCalls: []llm.ToolCall{{
						ID:       "call-1",
						Type:     "function",
						Function: llm.FunctionCall{Name: "no_such_tool", Arguments: "{}"},
					}},
					Done: true,
					Usage: &llm.Usage{
						PromptTokens:     1500,
						CompletionTokens: 50,
					},
				},
			},
			// Inner Stream #2: must NEVER be invoked. If the test
			// reaches this turn, the per-Stream gate failed.
			{
				{Token: "should not see"},
				{Done: true},
			},
		},
	}

	ag := New(provider, stubWorkspace{}, &NewOptions{
		TaskTokenBudget: 500,
	})
	t.Cleanup(ag.Close)
	events := subscribeForTest(t, ag)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ag.RunWithMode(ctx, "main.go", "", "go", event.ModeExecution)

	errMsg, doneSuccess := collectAbortEvents(t, events, 2*time.Second)
	if !strings.Contains(errMsg, "budget exceeded") {
		t.Errorf("AgentError = %q, want substring 'budget exceeded'", errMsg)
	}
	if doneSuccess {
		t.Error("AgentDone.Success = true, want false on per-Stream budget abort")
	}

	provider.mu.Lock()
	calls := provider.call
	provider.mu.Unlock()
	if calls != 1 {
		t.Errorf("provider Stream calls = %d, want 1 (per-Stream budget gate failed)", calls)
	}
}

// TestAgent_TokenBudget_DisabledByDefault checks that when
// NewOptions.TaskTokenBudget is left zero, [New] resolves it to 0 —
// the cap is OFF by default and must be armed explicitly (via a
// positive NewOptions value or NIB_TASK_TOKEN_BUDGET).
func TestAgent_TokenBudget_DisabledByDefault(t *testing.T) {
	t.Parallel()
	ag := New(&multiTurnProvider{}, stubWorkspace{}, nil)
	t.Cleanup(ag.Close)
	if ag.taskTokenBudget != 0 {
		t.Errorf("taskTokenBudget = %d, want 0 (disabled by default)",
			ag.taskTokenBudget)
	}
}

// TestAgent_TokenBudget_RecommendedOptIn checks that a caller can arm
// the recommended cap by passing [budget.RecommendedTaskTokens].
func TestAgent_TokenBudget_RecommendedOptIn(t *testing.T) {
	t.Parallel()
	ag := New(&multiTurnProvider{}, stubWorkspace{},
		&NewOptions{TaskTokenBudget: budget.RecommendedTaskTokens})
	t.Cleanup(ag.Close)
	if ag.taskTokenBudget != budget.RecommendedTaskTokens {
		t.Errorf("taskTokenBudget = %d, want %d",
			ag.taskTokenBudget, budget.RecommendedTaskTokens)
	}
}

// TestAgent_TokenBudget_NegativeMeansUnlimited covers the explicit
// opt-out: passing a negative budget resolves to zero (the disable
// sentinel) so a developer who knows what they're doing can turn the
// safety net off.
func TestAgent_TokenBudget_NegativeMeansUnlimited(t *testing.T) {
	t.Parallel()
	ag := New(&multiTurnProvider{}, stubWorkspace{},
		&NewOptions{TaskTokenBudget: -1})
	t.Cleanup(ag.Close)
	if ag.taskTokenBudget != 0 {
		t.Errorf("taskTokenBudget = %d, want 0 (unlimited)", ag.taskTokenBudget)
	}
}
