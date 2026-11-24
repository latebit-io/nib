package kit_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/event"
)

// TestAgentDone_SuccessFollowsReason: Success is derived from the reason
// the loop ended, so the two cannot disagree. The turn cap and a completed
// run are covered by TestMaxTurns_… and TestRun_….
func TestAgentDone_SuccessFollowsReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		provider llm.Provider
		hooks    kit.Hooks
		// atWait ends a run that parked; nil means it must end on its own.
		atWait      func(a *kit.Agent, cancelCaller context.CancelFunc)
		want        event.EndReason
		wantSuccess bool
	}{
		{
			name:     "tool batch terminates",
			provider: newScriptedProvider(streamWithToolCall("c1", "nop", "{}")),
			hooks: kit.Hooks{AfterToolCall: func(context.Context, kit.AfterToolCallInput) (kit.AfterToolCallResult, error) {
				return kit.AfterToolCallResult{Terminate: true}, nil
			}},
			want:        event.EndTerminated,
			wantSuccess: true,
		},
		{
			name:     "Cancel while waiting",
			provider: newScriptedProvider(streamText([]string{"hi"}, nil)),
			atWait:   func(a *kit.Agent, _ context.CancelFunc) { a.Cancel() },
			want:     event.EndAborted,
		},
		{
			// No kit.Cancel involved: the foundation still knows it was aborted.
			name:     "caller ctx cancelled while waiting",
			provider: newScriptedProvider(streamText([]string{"hi"}, nil)),
			atWait:   func(_ *kit.Agent, cancelCaller context.CancelFunc) { cancelCaller() },
			want:     event.EndAborted,
		},
		{
			name:     "provider fails",
			provider: errorProvider{err: errors.New("boom")},
			want:     event.EndError,
		},
		{
			// No AgentError is published here, yet the run did not succeed.
			name:     "truncated, hook declines retry",
			provider: newScriptedProvider([]llm.StreamEvent{{Done: true, Truncated: true}}),
			hooks: kit.Hooks{OnTruncated: func(context.Context, kit.TruncationInput) (kit.TruncationResult, error) {
				return kit.TruncationResult{}, nil
			}},
			want: event.EndTruncated,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a, err := kit.New(kit.Config{
				Provider: tc.provider,
				Toolset:  kit.Toolset{Tools: []kit.Tool{nopTool{name: "nop"}}, Hooks: tc.hooks},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer a.Close()
			events := subscribeEvents(t, a)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			if err := a.Prompt(ctx, "go"); err != nil {
				t.Fatalf("Prompt: %v", err)
			}
			got := drainUntil(events, func(ev event.Event) bool {
				if _, waiting := ev.(event.AgentWaiting); waiting && tc.atWait != nil {
					tc.atWait(a, cancel)
				}
				return untilDone(ev)
			})

			done, ok := lastDone(got)
			if !ok {
				t.Fatalf("no AgentDone; events = %v", eventTypeSeq(got))
			}
			if done.Reason != tc.want || done.Success != tc.wantSuccess {
				t.Errorf("AgentDone = {Reason: %q, Success: %v}; want {%q, %v}",
					done.Reason, done.Success, tc.want, tc.wantSuccess)
			}
		})
	}
}

// TestRun_ReturnsResultAndPublishesDone: Run hands back the run's result
// and subscribers see the same outcome on AgentDone.
func TestRun_ReturnsResultAndPublishesDone(t *testing.T) {
	t.Parallel()

	usage := &llm.Usage{PromptTokens: 7, CompletionTokens: 3}
	a, err := kit.New(kit.Config{Provider: newScriptedProvider(streamText([]string{"The ", "answer."}, usage))})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	events := subscribeEvents(t, a)

	res, err := a.Run(context.Background(), []llm.Message{{Role: "user", Content: "go"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := event.AgentDone{
		Success:   true,
		Reason:    event.EndCompleted,
		Usage:     event.RunUsage{Turns: 1, PromptTokens: 7, CompletionTokens: 3},
		FinalText: "The answer.",
	}
	if res.Done != want {
		t.Errorf("Run result = %+v; want %+v", res.Done, want)
	}
	if len(res.Messages) != 2 {
		t.Errorf("Run result has %d messages; want 2 (user, assistant)", len(res.Messages))
	}
	if done, ok := lastDone(drainUntil(events, untilDone)); !ok || done != want {
		t.Errorf("published AgentDone = %+v (found %v); want %+v", done, ok, want)
	}
}

// lateProvider records a turn requested after the test marked the agent
// closed.
type lateProvider struct{ closed, late atomic.Bool }

func (p *lateProvider) Stream(context.Context, []llm.Message, []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	if p.closed.Load() {
		p.late.Store(true)
	}
	ch := make(chan llm.StreamEvent, 1)
	ch <- llm.StreamEvent{Done: true}
	close(ch)
	return ch, nil
}

// TestClose_NoRunStartsBehindIt races Run against Close. Whichever wins,
// no turn may be requested once Close has returned.
func TestClose_NoRunStartsBehindIt(t *testing.T) {
	t.Parallel()

	for range 300 {
		provider := &lateProvider{}
		a, err := kit.New(kit.Config{Provider: provider})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		ran := make(chan struct{})
		go func() {
			defer close(ran)
			// Either outcome is fine: a finished run or ErrClosed.
			_, _ = a.Run(context.Background(), []llm.Message{{Role: "user", Content: "go"}})
		}()
		a.Close()
		provider.closed.Store(true)
		<-ran
		if provider.late.Load() {
			t.Fatal("a turn was requested after Close returned")
		}
	}
}
