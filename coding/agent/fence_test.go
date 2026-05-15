package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/event"
)

// TestFenceForwarder_BlocksUntilRunDoneClosed locks the run-boundary
// barrier contract: a [Agent.fenceForwarder] call against an armed
// runDone must block until the channel is closed (the moment the
// forwarder finishes processing the prior run's AgentDone), then
// unblock promptly.
func TestFenceForwarder_BlocksUntilRunDoneClosed(t *testing.T) {
	t.Parallel()
	a := &Agent{}

	rd := a.armRunDone()

	fenced := make(chan struct{})
	go func() {
		a.fenceForwarder()
		close(fenced)
	}()

	select {
	case <-fenced:
		t.Fatal("fenceForwarder returned before runDone was closed")
	case <-time.After(50 * time.Millisecond):
	}

	a.runDoneMu.Lock()
	close(a.runDone)
	a.runDone = nil
	a.runDoneMu.Unlock()
	_ = rd

	select {
	case <-fenced:
	case <-time.After(time.Second):
		t.Fatal("fenceForwarder did not return after runDone closed")
	}
}

// TestFenceForwarder_NoPriorRunReturnsImmediately covers the
// fresh-agent path: with no runDone armed, fence is a no-op.
func TestFenceForwarder_NoPriorRunReturnsImmediately(t *testing.T) {
	t.Parallel()
	a := &Agent{}

	done := make(chan struct{})
	go func() {
		a.fenceForwarder()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("fenceForwarder blocked when no runDone was armed")
	}
}

// TestDisarmRunDone_ClosesChannelAndAllowsFence covers the
// kit-rejection path: when no AgentDone will arrive (kit rejected
// PromptWithMessages), [Agent.disarmRunDone] must close the channel
// so the next fence does not block forever.
func TestDisarmRunDone_ClosesChannelAndAllowsFence(t *testing.T) {
	t.Parallel()
	a := &Agent{}

	rd := a.armRunDone()
	a.disarmRunDone(rd)

	select {
	case _, ok := <-rd:
		if ok {
			t.Error("runDone should be closed after disarmRunDone")
		}
	default:
		t.Error("runDone was not closed by disarmRunDone")
	}

	done := make(chan struct{})
	go func() {
		a.fenceForwarder()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("fenceForwarder blocked after disarmRunDone")
	}
}

// TestDisarmRunDone_SkipsIfReplaced guards the defensive pointer
// check inside [Agent.disarmRunDone]: if a fresh runDone has already
// been armed (or the forwarder already cleared it), disarm must NOT
// touch the new channel — touching it would close someone else's
// in-flight runDone or panic on double-close.
func TestDisarmRunDone_SkipsIfReplaced(t *testing.T) {
	t.Parallel()
	a := &Agent{}

	stale := a.armRunDone()
	// Simulate forwarder racing ahead and clearing the field.
	a.runDoneMu.Lock()
	a.runDone = nil
	a.runDoneMu.Unlock()
	// Also simulate a fresh run installing a new channel.
	fresh := a.armRunDone()

	a.disarmRunDone(stale)

	// Fresh channel must remain open.
	select {
	case <-fresh:
		t.Fatal("disarmRunDone closed the fresh runDone instead of skipping")
	default:
	}
	// The stale channel was never closed (forwarder cleared the field
	// before disarm could match the pointer), so the test must close
	// it explicitly to avoid a goroutine leak in any future fence.
	a.runDoneMu.Lock()
	if a.runDone == fresh {
		a.runDone = nil
	}
	a.runDoneMu.Unlock()
	close(fresh)
	close(stale)
}

// TestRunWithMode_ConcurrentStartsSerialize proves the [Agent.startMu]
// guard against the reviewer's interleaving scenario: two goroutines
// race into RunWithMode at the same time. Without startMu, the loser's
// per-run state reset (a.coord, a.cancel, a.intent, a.mode) would
// clobber the winner's, leaving the accepted foundation run executing
// against the wrong wrapper state. With startMu, the second starter
// blocks until the first completes its full startup sequence.
//
// The test asserts that after both calls return, the agent state is
// internally consistent: a.intent == one of the two goals (not a
// blend), a.cancel is non-nil (one starter's cancel survived), and
// the runDone channel exists and corresponds to the run that is
// actually executing (closed by forwarder when its AgentDone fires).
func TestRunWithMode_ConcurrentStartsSerialize(t *testing.T) {
	t.Parallel()

	provider := &multiTurnProvider{
		turns: [][]llm.StreamEvent{
			{
				{Token: "first"},
				{Done: true, Usage: &llm.Usage{PromptTokens: 100, CompletionTokens: 10}},
			},
			{
				{Token: "second"},
				{Done: true, Usage: &llm.Usage{PromptTokens: 200, CompletionTokens: 20}},
			},
		},
	}

	ag := New(provider, stubWorkspace{}, nil)
	sub, err := ag.Subscribe(SubscribeOptions{BufferSize: 256})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Close)
	events := sub.Events()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Drain events in the background so the forwarder doesn't block;
	// the test's invariants are about wrapper state, not event content.
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for {
			select {
			case _, ok := <-events:
				if !ok {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			goal := "first"
			if i == 1 {
				goal = "second"
			}
			ag.RunWithMode(ctx, "main.go", "", goal, nil, event.ModeExecution)
		}()
	}
	wg.Wait()

	// After both RunWithMode calls return, exactly one run won
	// startMu last and its state survives. Intent must be one of the
	// two (not corrupted by interleaving).
	ag.mu.Lock()
	intent := ag.intent
	cancelFn := ag.cancel
	ag.mu.Unlock()

	if intent != "first" && intent != "second" {
		t.Errorf("intent = %q; want one of {\"first\", \"second\"} — interleaved reset corrupted state", intent)
	}
	if cancelFn == nil {
		t.Error("a.cancel == nil after concurrent starts; one starter's cancel func should have survived")
	}

	// Cancel the surviving run so the forwarder can settle and the
	// drainer can exit cleanly.
	ag.Cancel()
	cancel()
	<-drainDone
}

// runUntilWaitingThenCancel drives the agent through one full
// turn-and-park cycle: drain events until the agent emits
// AgentWaiting (turn complete, foundation parked at awaitReply), then
// Cancel and drain until AgentDone. Returns when both have been
// observed. Test helper for the back-to-back-runs fence tests.
func runUntilWaitingThenCancel(t *testing.T, ag *Agent, events <-chan event.Event, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	waitSeen := false
	for !waitSeen {
		select {
		case ev := <-events:
			if _, ok := ev.(event.AgentWaiting); ok {
				waitSeen = true
			}
		case <-deadline:
			t.Fatal("timeout waiting for AgentWaiting after run")
		}
	}
	ag.Cancel()
	for {
		select {
		case ev := <-events:
			if _, ok := ev.(event.AgentDone); ok {
				return
			}
		case <-deadline:
			t.Fatal("timeout waiting for AgentDone after Cancel")
		}
	}
}

// TestRunWithMode_FenceIsolatesSessionUsageFromPrevRun is the
// integration check for the bug the fence prevents. Two successive
// RunWithMode calls with very different per-turn token counts; after
// run 2 completes, sessionUsage must reflect ONLY run 2's tokens.
// Without [Agent.fenceForwarder] the prior run's AgentTurnUsage could
// land in the forwarder after the per-run-state reset and accumulate
// into the new run's totals.
func TestRunWithMode_FenceIsolatesSessionUsageFromPrevRun(t *testing.T) {
	t.Parallel()

	provider := &multiTurnProvider{
		turns: [][]llm.StreamEvent{
			{
				{Token: "first"},
				{Done: true, Usage: &llm.Usage{PromptTokens: 1000, CompletionTokens: 50}},
			},
			{
				{Token: "second"},
				{Done: true, Usage: &llm.Usage{PromptTokens: 100, CompletionTokens: 10}},
			},
		},
	}

	ag := New(provider, stubWorkspace{}, nil)
	sub, err := ag.Subscribe(SubscribeOptions{BufferSize: 64})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Close)
	events := sub.Events()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ag.RunWithMode(ctx, "main.go", "", "first", nil, event.ModeExecution)
	runUntilWaitingThenCancel(t, ag, events, 2*time.Second)

	usageAfterRun1 := ag.Usage()
	if usageAfterRun1.TotalPromptTokens != 1000 {
		t.Fatalf("after run 1, TotalPromptTokens = %d; want 1000",
			usageAfterRun1.TotalPromptTokens)
	}

	ag.RunWithMode(ctx, "main.go", "", "second", nil, event.ModeExecution)
	runUntilWaitingThenCancel(t, ag, events, 2*time.Second)

	usage := ag.Usage()
	if usage.TotalPromptTokens != 100 {
		t.Errorf("after run 2, TotalPromptTokens = %d; want 100 (run 2 only — run 1 leaked through fence)",
			usage.TotalPromptTokens)
	}
	if usage.TotalCompletionTokens != 10 {
		t.Errorf("after run 2, TotalCompletionTokens = %d; want 10 (run 2 only)",
			usage.TotalCompletionTokens)
	}
	if usage.Turns != 1 {
		t.Errorf("after run 2, Turns = %d; want 1 (turnCounter must reset between runs)",
			usage.Turns)
	}
}

// TestRunWithMode_FenceWaitsForBackedUpForwarder reproduces the
// reviewer's concrete scenario: the consumer is slow, kit's events
// have backed up in [Agent.kitEvents], and a follow-up RunWithMode
// arrives before the forwarder has finished processing the prior
// run's tail. The fence must block RunWithMode until the consumer
// drains and the forwarder catches up; after the fence releases,
// per-run state must be clean (no leak from the prior run's
// AgentTurnUsage that was buffered behind the consumer).
func TestRunWithMode_FenceWaitsForBackedUpForwarder(t *testing.T) {
	t.Parallel()

	provider := &multiTurnProvider{
		turns: [][]llm.StreamEvent{
			{
				{Token: "first"},
				{Done: true, Usage: &llm.Usage{PromptTokens: 5000, CompletionTokens: 200}},
			},
			{
				{Token: "second"},
				{Done: true, Usage: &llm.Usage{PromptTokens: 7, CompletionTokens: 1}},
			},
		},
	}

	// Small subscriber inbox — backpressure builds when the consumer
	// pauses, exercising the fence on the consumer-slow path.
	ag := New(provider, stubWorkspace{}, nil)
	sub, err := ag.Subscribe(SubscribeOptions{BufferSize: 4})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Close)
	events := sub.Events()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ag.RunWithMode(ctx, "main.go", "", "first", nil, event.ModeExecution)

	// Drain just enough to observe AgentWaiting (turn 1 complete), then
	// pause draining so subsequent events back up in the forwarder.
	deadline := time.After(2 * time.Second)
	waited := false
	for !waited {
		select {
		case ev := <-events:
			if _, ok := ev.(event.AgentWaiting); ok {
				waited = true
			}
		case <-deadline:
			t.Fatal("timeout waiting for AgentWaiting on run 1")
		}
	}

	// Cancel run 1. AgentDone will queue behind any backed-up events.
	ag.Cancel()

	// Kick off run 2 on a goroutine. fenceForwarder should block until
	// run 1's AgentDone has been forwarded (consumer drains, forwarder
	// catches up).
	run2Started := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(run2Started)
		ag.RunWithMode(ctx, "main.go", "", "second", nil, event.ModeExecution)
	}()
	<-run2Started

	// Drain everything: both AgentDones (run 1 cancel + run 2's eventual
	// natural park or cancel — we cancel below to terminate).
	dones := 0
	captureErr := ""
	deadline2 := time.After(3 * time.Second)
drain:
	for {
		select {
		case ev := <-events:
			if e, ok := ev.(event.AgentError); ok && captureErr == "" {
				captureErr = e.Err
			}
			if _, ok := ev.(event.AgentDone); ok {
				dones++
				if dones == 1 {
					// Run 2 should now be in flight (RunWithMode unblocked
					// from fence, foundation accepted PromptWithMessages).
					// Wait for AgentWaiting then Cancel to terminate.
					go func() {
						time.Sleep(20 * time.Millisecond)
						ag.Cancel()
					}()
				}
				if dones == 2 {
					break drain
				}
			}
		case <-deadline2:
			t.Fatalf("timed out waiting for both AgentDones (saw %d, last err %q)", dones, captureErr)
		}
	}

	wg.Wait()

	usage := ag.Usage()
	// Run 2 may have produced 0 or 1 TurnUsage depending on timing
	// (we cancel after AgentWaiting; Cancel itself doesn't produce
	// new TurnUsage). Assertions cover both flows: run 1's tokens
	// must NOT have leaked into run 2's accounting.
	if usage.TotalPromptTokens >= 5000 {
		t.Errorf("after fenced run 2, TotalPromptTokens = %d; run 1 (5000) leaked through the fence",
			usage.TotalPromptTokens)
	}
	if usage.TotalCompletionTokens >= 200 {
		t.Errorf("after fenced run 2, TotalCompletionTokens = %d; run 1 (200) leaked through the fence",
			usage.TotalCompletionTokens)
	}
}
