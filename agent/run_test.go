package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/latebit-io/nib/agent/event"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/ai/llmtest"
)

// runBounded calls Run and fails the test, instead of hanging it, when
// Run does not return.
func runBounded(ctx context.Context, t *testing.T, a *Agent, msgs []llm.Message) (event.AgentEnd, error) {
	t.Helper()
	done := make(chan runResult, 1)
	go func() {
		end, err := a.Run(ctx, msgs)
		done <- runResult{end: end, err: err}
	}()
	select {
	case res := <-done:
		return res.end, res.err
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
		return event.AgentEnd{}, nil
	}
}

// TestRun covers what Run hands back for each kind of ending. No agent
// here has an events channel or the OneShot option: Run needs neither.
func TestRun(t *testing.T) {
	t.Parallel()

	failing := errors.New("boom")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	// expired times out during the run, or before it under load: the
	// row must hold either way.
	expired := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), 20*time.Millisecond)
	}

	tests := []struct {
		name string
		opts Options
		ctx  func() (context.Context, context.CancelFunc) // nil: Background
		want event.EndReason
		// wantErr is matched with errors.Is; nil means Run returns no error.
		wantErr  error
		wantText string
	}{
		{
			name:     "completes instead of parking",
			opts:     Options{Provider: llmtest.NewScripted(llmtest.Text("answer"))},
			want:     event.EndCompleted,
			wantText: "answer",
		},
		{
			name: "policy stop is not an error",
			opts: Options{Provider: llmtest.NewScripted(llmtest.ToolCalls(echoCall)), MaxTurns: 1},
			want: event.EndMaxTurns,
		},
		{
			name:    "failure returns its cause",
			opts:    Options{Provider: llmtest.Failing(failing)},
			want:    event.EndError,
			wantErr: failing,
		},
		{
			name:    "truncation without a hook is a failure",
			opts:    Options{Provider: llmtest.NewScripted([]llm.StreamEvent{{Done: true, Truncated: true}})},
			want:    event.EndTruncated,
			wantErr: errProviderTruncated,
		},
		{
			name:    "cancelled ctx returns Canceled",
			opts:    Options{Provider: llmtest.Blocking()},
			ctx:     func() (context.Context, context.CancelFunc) { return cancelled, func() {} },
			want:    event.EndAborted,
			wantErr: context.Canceled,
		},
		{
			name:    "ctx deadline returns DeadlineExceeded",
			opts:    Options{Provider: llmtest.Blocking()},
			ctx:     expired,
			want:    event.EndAborted,
			wantErr: context.DeadlineExceeded,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			if tc.ctx != nil {
				var release context.CancelFunc
				ctx, release = tc.ctx()
				defer release()
			}
			tc.opts.Tools = []Tool{namedTool{name: "echo"}}
			a, err := New(tc.opts)
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			end, err := runBounded(ctx, t, a, question)

			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Run error = %v; want %v", err, tc.wantErr)
			}
			if end.Reason != tc.want {
				t.Errorf("Reason = %q; want %q", end.Reason, tc.want)
			}
			if end.FinalText != tc.wantText {
				t.Errorf("FinalText = %q; want %q", end.FinalText, tc.wantText)
			}
			if len(end.Messages) == 0 {
				t.Error("Messages is empty; want the run's transcript")
			}
		})
	}
}

// TestRun_AbortKeepsPartialText cancels only once the first token has
// been streamed, so the partial text is there whatever the scheduling.
func TestRun_AbortKeepsPartialText(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan event.Event, 64)
	a, err := New(Options{Provider: llmtest.Blocking("partial"), Events: events})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	go func() {
		for ev := range events {
			switch ev.(type) {
			case event.MessageUpdate:
				cancel()
			case event.AgentEnd:
				return
			}
		}
	}()

	end, err := runBounded(ctx, t, a, question)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run error = %v; want context.Canceled", err)
	}
	if end.Reason != event.EndAborted || end.FinalText != "partial" {
		t.Errorf("Reason = %q, FinalText = %q; want aborted, %q", end.Reason, end.FinalText, "partial")
	}
}

// TestRun_NeverStarted: a run that cannot start reports why and leaves
// the active run alone.
func TestRun_NeverStarted(t *testing.T) {
	t.Parallel()

	a, err := New(Options{Provider: llmtest.Blocking()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.Run(context.Background(), nil); !errors.Is(err, ErrInvalidOptions) {
		t.Errorf("Run with no messages: error = %v; want ErrInvalidOptions", err)
	}

	if err := a.Prompt(context.Background(), "holds the agent"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if _, err := a.Run(context.Background(), question); !errors.Is(err, ErrRunInProgress) {
		t.Errorf("Run during a run: error = %v; want ErrRunInProgress", err)
	}
	a.Abort()
	a.WaitForIdle()
}

// TestClose_RefusesNewRuns: after Close no entry point may start a run.
func TestClose_RefusesNewRuns(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	starts := map[string]func(a *Agent) error{
		"Prompt":             func(a *Agent) error { return a.Prompt(ctx, "go") },
		"PromptWithMessages": func(a *Agent) error { return a.PromptWithMessages(ctx, question) },
		"Run":                func(a *Agent) error { _, err := a.Run(ctx, question); return err },
	}
	for name, start := range starts {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// A provider that finishes: a wrongly admitted run fails the
			// test instead of hanging it.
			a, err := New(Options{Provider: llmtest.NewScripted(llmtest.Done())})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			a.Close()
			a.Close() // idempotent
			if err := start(a); !errors.Is(err, ErrClosed) {
				t.Errorf("%s after Close: error = %v; want ErrClosed", name, err)
			}
		})
	}
}

// TestRun_BackToBack: when Run returns, the agent must already be idle,
// or the next Run would be refused.
func TestRun_BackToBack(t *testing.T) {
	t.Parallel()

	const runs = 50
	turns := make([][]llm.StreamEvent, runs)
	for i := range turns {
		turns[i] = llmtest.Done()
	}
	a, err := New(Options{Provider: llmtest.NewScripted(turns...)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := range runs {
		if _, err := runBounded(context.Background(), t, a, question); err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
	}
}
