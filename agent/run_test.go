package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/latebit-io/nib/agent/event"
	"github.com/latebit-io/nib/ai/llm"
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
	// expired is live when Run starts and times out mid-turn.
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
			opts:     Options{Provider: newScriptedProvider(streamText([]string{"answer"}, nil))},
			want:     event.EndCompleted,
			wantText: "answer",
		},
		{
			name: "policy stop is not an error",
			opts: Options{Provider: newScriptedProvider(streamWithToolCalls([]llm.ToolCall{echoCall})), MaxTurns: 1},
			want: event.EndMaxTurns,
		},
		{
			name:    "failure returns its cause",
			opts:    Options{Provider: errorProvider{err: failing}},
			want:    event.EndError,
			wantErr: failing,
		},
		{
			name:    "truncation without a hook is a failure",
			opts:    Options{Provider: newScriptedProvider([]llm.StreamEvent{{Done: true, Truncated: true}})},
			want:    event.EndTruncated,
			wantErr: errProviderTruncated,
		},
		{
			name:    "cancelled ctx returns Canceled",
			opts:    Options{Provider: hangingProvider{}},
			ctx:     func() (context.Context, context.CancelFunc) { return cancelled, func() {} },
			want:    event.EndAborted,
			wantErr: context.Canceled,
		},
		{
			name:     "ctx deadline returns DeadlineExceeded and the partial text",
			opts:     Options{Provider: hangingProvider{"partial"}},
			ctx:      expired,
			want:     event.EndAborted,
			wantErr:  context.DeadlineExceeded,
			wantText: "partial",
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

// TestRun_NeverStarted: a run that cannot start reports why and leaves
// the active run alone.
func TestRun_NeverStarted(t *testing.T) {
	t.Parallel()

	a, err := New(Options{Provider: hangingProvider{}})
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

// TestRun_BackToBack: when Run returns, the agent must already be idle,
// or the next Run would be refused.
func TestRun_BackToBack(t *testing.T) {
	t.Parallel()

	const runs = 50
	turns := make([][]llm.StreamEvent, runs)
	for i := range turns {
		turns[i] = streamDone()
	}
	a, err := New(Options{Provider: newScriptedProvider(turns...)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := range runs {
		if _, err := runBounded(context.Background(), t, a, question); err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
	}
}
