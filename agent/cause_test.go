package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/latebit-io/nib/agent/event"
	"github.com/latebit-io/nib/ai/llmtest"
)

// TestAgentEnd_AbortCause: an aborted run says who ended it. Each row
// stops a run that is under way and checks AgentEnd.Err.
func TestAgentEnd_AbortCause(t *testing.T) {
	t.Parallel()

	readerGone := errors.New("the reader went away")
	tests := []struct {
		name string
		stop func(a *Agent, cancel context.CancelCauseFunc)
		// want are all matched with errors.Is; not is what Err must not be.
		want []error
		not  error
	}{
		{
			name: "Abort",
			stop: func(a *Agent, _ context.CancelCauseFunc) { a.Abort() },
			want: []error{ErrAborted, context.Canceled},
			not:  ErrClosed,
		},
		{
			name: "Close",
			stop: func(a *Agent, _ context.CancelCauseFunc) { a.Close() },
			want: []error{ErrClosed, context.Canceled},
			not:  ErrAborted,
		},
		{
			name: "the caller's own cause",
			stop: func(_ *Agent, cancel context.CancelCauseFunc) { cancel(readerGone) },
			want: []error{readerGone},
			not:  ErrAborted,
		},
		{
			name: "the caller cancels without one",
			stop: func(_ *Agent, cancel context.CancelCauseFunc) { cancel(nil) },
			want: []error{context.Canceled},
			not:  ErrAborted,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			events := make(chan event.Event, 64)
			a, err := New(Options{Provider: llmtest.Blocking("partial"), Events: events})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := a.PromptWithMessages(ctx, question); err != nil {
				t.Fatalf("PromptWithMessages: %v", err)
			}

			deadline := time.After(2 * time.Second)
			for {
				select {
				case ev := <-events:
					switch e := ev.(type) {
					case event.MessageUpdate:
						tc.stop(a, cancel) // the run is under way
					case event.AgentEnd:
						if e.Reason != event.EndAborted {
							t.Errorf("Reason = %q; want aborted", e.Reason)
						}
						for _, want := range tc.want {
							if !errors.Is(e.Err, want) {
								t.Errorf("Err = %v; want it to match %v", e.Err, want)
							}
						}
						if errors.Is(e.Err, tc.not) {
							t.Errorf("Err = %v; must not match %v", e.Err, tc.not)
						}
						return
					}
				case <-deadline:
					t.Fatal("no AgentEnd")
				}
			}
		})
	}
}
