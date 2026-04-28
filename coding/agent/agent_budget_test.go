package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/junto/ai/llm"
	"github.com/latebit-io/junto/coding/budget"
	"github.com/latebit-io/junto/engine/event"
)

// TestCheckTaskBudget_Disabled covers the four non-firing branches of
// [Agent.checkTaskBudget]: zero budget, negative-budget unlimited resolved
// by [New], stale runID, and already-latched. The check must return ""
// from each so the run loop never erroneously aborts a healthy turn.
func TestCheckTaskBudget_Disabled(t *testing.T) {
	t.Parallel()

	const useAgentRunID = -1 // sentinel: tc.runID == useAgentRunID → use ag.runID

	cases := []struct {
		name string
		opts *NewOptions
		// mutate runs after construction so the test can poke runID,
		// sessionUsage, or budgetExceeded into the state under test.
		mutate func(a *Agent)
		// runID is the value passed to checkTaskBudget. useAgentRunID
		// (default-equivalent for any case that does not opt in) means
		// "use the agent's current runID"; any other value (e.g. 0) is
		// passed verbatim to exercise the stale-runID branch without a
		// brittle string-match on tc.name.
		runID int64
	}{
		{
			name:   "zero budget skips check",
			opts:   &NewOptions{TaskTokenBudget: -1}, // -1 → unlimited (zero internally)
			mutate: func(a *Agent) { a.sessionUsage.TotalPromptTokens = 1_000_000 },
			runID:  useAgentRunID,
		},
		{
			name: "stale runID returns early",
			opts: &NewOptions{TaskTokenBudget: 100},
			mutate: func(a *Agent) {
				a.sessionUsage.TotalPromptTokens = 200
				a.runID = 5
			},
			runID: 0, // intentionally mismatched against the agent's runID=5
		},
		{
			name: "already-latched returns empty",
			opts: &NewOptions{TaskTokenBudget: 100},
			mutate: func(a *Agent) {
				a.sessionUsage.TotalPromptTokens = 200
				a.budgetExceeded = true
			},
			runID: useAgentRunID,
		},
		{
			name:   "under cap returns empty",
			opts:   &NewOptions{TaskTokenBudget: 1000},
			mutate: func(a *Agent) { a.sessionUsage.TotalPromptTokens = 500 },
			runID:  useAgentRunID,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			events := make(chan event.Event, 4)
			ag := New(&multiTurnProvider{}, stubWorkspace{}, events, tc.opts)
			tc.mutate(ag)
			runID := uint64(ag.runID)
			if tc.runID != useAgentRunID {
				runID = uint64(tc.runID)
			}
			if msg := ag.checkTaskBudget(runID); msg != "" {
				t.Errorf("checkTaskBudget returned %q, want empty", msg)
			}
		})
	}
}

// TestShouldAbortForBudget covers the inner-loop budget gate that
// processLLMTurn consults BETWEEN provider Stream calls. Distinct from
// checkTaskBudget in two ways: it sums committed+pending usage (so a
// turn that has not yet been recorded is still counted), and it does
// not latch budgetExceeded (the abort path owns the latch). Each row
// confirms one branch of the gate.
func TestShouldAbortForBudget(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		opts    *NewOptions
		mutate  func(a *Agent)
		pending budget.Turn
		want    bool
	}{
		{
			name:    "zero budget disables gate",
			opts:    &NewOptions{TaskTokenBudget: -1}, // -1 → unlimited (zero internally)
			pending: budget.Turn{PromptTokens: 1_000_000},
			want:    false,
		},
		{
			name: "already latched suppresses gate",
			opts: &NewOptions{TaskTokenBudget: 100},
			mutate: func(a *Agent) {
				a.budgetExceeded = true
				a.sessionUsage.TotalPromptTokens = 200
			},
			want: false,
		},
		{
			// Wiring smoke: confirms shouldAbortForBudget delegates to
			// budget.WouldExceed for the math (committed + pending). The
			// pure-math edge cases are exhaustively covered in
			// coding/budget; this single positive row is enough to catch
			// a wiring regression here.
			name:    "math is wired through to budget.WouldExceed",
			opts:    &NewOptions{TaskTokenBudget: 100},
			mutate:  func(a *Agent) { a.sessionUsage.TotalPromptTokens = 60 },
			pending: budget.Turn{PromptTokens: 50}, // 60+50 = 110 > 100
			want:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			events := make(chan event.Event, 4)
			ag := New(&multiTurnProvider{}, stubWorkspace{}, events, tc.opts)
			if tc.mutate != nil {
				tc.mutate(ag)
			}
			// Capture the latch state before the call. shouldAbortForBudget
			// must NOT mutate budgetExceeded — only the abort path
			// (checkTaskBudget after recordTurnUsage commits) is allowed
			// to latch. If this helper accidentally latched, a later
			// checkTaskBudget call would short-circuit and the abort
			// AgentError would never fire. Locking the invariant in tests
			// keeps that pre-condition load-bearing.
			latchBefore := ag.budgetExceeded
			if got := ag.shouldAbortForBudget(tc.pending); got != tc.want {
				t.Errorf("shouldAbortForBudget(%+v) = %v, want %v", tc.pending, got, tc.want)
			}
			if ag.budgetExceeded != latchBefore {
				t.Errorf("shouldAbortForBudget mutated budgetExceeded latch: before=%v, after=%v",
					latchBefore, ag.budgetExceeded)
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

	errMsg, doneSuccess := collectAbortEvents(t, events, 2*time.Second)
	if !strings.Contains(errMsg, "budget exceeded") {
		t.Errorf("AgentError = %q, want substring 'budget exceeded'", errMsg)
	}
	if doneSuccess {
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

// collectAbortEvents drains the agent's event channel until both an
// [event.AgentError] and an [event.AgentDone] have been observed (or
// the timeout fires). Returns the AgentError's message and the
// AgentDone's Success flag. Order-tolerant: the two events may arrive
// in any sequence, and intervening events (tokens, status, edit
// proposals) are silently discarded. [event.FlushBuffers] is auto-
// resolved with an empty result so the agent does not block on a flush
// that the test harness has no real buffer to satisfy.
//
// Used by every test that asserts on the budget-abort surface so the
// drain logic stays in one place — duplicating it across tests
// historically silently dropped one event when its order shifted.
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
			if fb, ok := ev.(event.FlushBuffers); ok {
				fb.Result <- event.FlushResult{}
				continue
			}
			switch e := ev.(type) {
			case event.AgentError:
				errSeen = true
				errMsg = e.Err
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

// TestAgent_TokenBudget_AbortsBetweenInnerStreams verifies the inner-
// loop budget gate: a single agent turn can call Stream multiple times
// (once per tool-call round-trip), and the budget must abort BETWEEN
// those calls — not just after the whole turn finishes. The scripted
// provider emits a tool call to a non-existent tool on the first
// Stream; the agent dispatches it (gets an error reply), then loops to
// call Stream a second time. With the budget set so the FIRST stream's
// usage already crosses the cap, the second Stream must never fire.
//
// Without the inner gate, processLLMTurn would loop indefinitely (or
// until truncation) and runLoop's outer abort would only fire after
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
			// reaches this turn, the inner-loop gate failed.
			{
				{Token: "should not see"},
				{Done: true},
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

	errMsg, doneSuccess := collectAbortEvents(t, events, 2*time.Second)
	if !strings.Contains(errMsg, "budget exceeded") {
		t.Errorf("AgentError = %q, want substring 'budget exceeded'", errMsg)
	}
	if doneSuccess {
		t.Error("AgentDone.Success = true, want false on inner-loop budget abort")
	}

	// The inner gate must have prevented the second Stream call.
	provider.mu.Lock()
	calls := provider.call
	provider.mu.Unlock()
	if calls != 1 {
		t.Errorf("provider Stream calls = %d, want 1 (inner-loop budget gate failed)", calls)
	}
}

// TestAgent_StaleRunGuard_SkipsSecondStream verifies the runID guard
// inside processLLMTurn's inner for-loop. The provider's first Stream
// call returns a tool call (forcing processLLMTurn to loop) AND
// signals the test once the call lands — at which point the test
// bumps a.runID (simulating a competing RunWithMode that replaced
// the current run in the window between unlock and prevCancel) and
// cancels the agent's context (mirroring prevCancel). Without the
// guard, processLLMTurn would call Stream a second time and burn
// tokens on a request whose accounting recordTurnUsage would later
// drop. With the guard, processLLMTurn returns ctx.Canceled at the
// top of the next iteration before Stream fires.
func TestAgent_StaleRunGuard_SkipsSecondStream(t *testing.T) {
	t.Parallel()

	streamCalled := make(chan struct{}, 1)
	provider := &signallingProvider{streamCalled: streamCalled}
	provider.turns = [][]llm.StreamEvent{
		// Turn 1: tool call forces processLLMTurn to loop back.
		{
			{
				ToolCalls: []llm.ToolCall{{
					ID:       "call-1",
					Type:     "function",
					Function: llm.FunctionCall{Name: "no_such_tool", Arguments: "{}"},
				}},
				Done: true,
			},
		},
		// Turn 2: must NEVER fire. If reached, the guard failed.
		{
			{Token: "should not see"},
			{Done: true},
		},
	}

	events := make(chan event.Event, 64)
	ag := New(provider, stubWorkspace{}, events, nil)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Drain events so the agent does not block on FlushBuffers or a
	// full channel during the unwind.
	go func() {
		for ev := range events {
			if fb, ok := ev.(event.FlushBuffers); ok {
				fb.Result <- event.FlushResult{}
			}
		}
	}()

	ag.RunWithMode(ctx, "main.go", "", "go", nil, ModeExecution)

	// Wait for the first Stream call to land, then race the runID
	// bump against the second iteration of processLLMTurn's for loop.
	select {
	case <-streamCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("first Stream call never observed")
	}

	// Bump runID WITHOUT canceling ctx. The agent's existing post-
	// drainStream `ctx.Err()` check (right after a successful Stream)
	// would also stop the loop if ctx were canceled, masking whether
	// the runID guard at the TOP of the loop is actually load-bearing.
	// Bumping only runID isolates this test to the guard under
	// inspection.
	ag.mu.Lock()
	ag.runID++
	ag.mu.Unlock()

	// Allow the goroutine to react. 50ms is generous: the guard runs
	// at the top of the next iteration before any IO. With the guard
	// in place, processLLMTurn returns immediately; without it, the
	// second Stream call fires well within this window.
	time.Sleep(50 * time.Millisecond)

	provider.mu.Lock()
	calls := provider.call
	provider.mu.Unlock()
	if calls != 1 {
		t.Errorf("provider Stream calls = %d, want 1 (stale-run guard failed)", calls)
	}

	// Cancel for cleanup so the run goroutine exits before the test
	// returns. With ctx canceled, the run loop's existing waiting-
	// state select will pick the ctx.Done branch and return.
	cancel()
}

// signallingProvider is a multiTurnProvider that pings a channel
// each time Stream is invoked. Used by stale-run guard tests to
// synchronize the runID flip with a known point in the agent loop.
type signallingProvider struct {
	multiTurnProvider
	streamCalled chan<- struct{}
}

func (p *signallingProvider) Stream(ctx context.Context, messages []llm.Message, defs []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	ch, err := p.multiTurnProvider.Stream(ctx, messages, defs)
	select {
	case p.streamCalled <- struct{}{}:
	default:
	}
	return ch, err
}

// TestAgent_TokenBudget_DefaultApplied checks that when NewOptions.TaskTokenBudget
// is left zero, [New] resolves it to [budget.DefaultTaskTokens] rather than
// leaving the budget disabled. Disabling the budget by default would make
// the safety net silently absent — the regression guard would not fire.
func TestAgent_TokenBudget_DefaultApplied(t *testing.T) {
	t.Parallel()
	events := make(chan event.Event, 1)
	ag := New(&multiTurnProvider{}, stubWorkspace{}, events, nil)
	if ag.taskTokenBudget != budget.DefaultTaskTokens {
		t.Errorf("taskTokenBudget = %d, want default %d",
			ag.taskTokenBudget, budget.DefaultTaskTokens)
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
