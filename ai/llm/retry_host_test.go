package llm_test

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/ai/llmtest"
)

// TestWithRetry_HostTestOnAFakeClock is the pattern for a host's test of
// its own retry policy: inside a synctest bubble the waits cost no real
// time, so the real delays can be asserted. Nothing needs injecting.
func TestWithRetry_HostTestOnAFakeClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		type runKey struct{}
		rateLimited := &llm.StatusError{Provider: "acme", StatusCode: 429, RetryAfter: 7 * time.Second}
		provider := llmtest.FailingFirst(2, rateLimited, llmtest.NewScripted(llmtest.Text("ok")))
		var narrated []string
		policy := llm.RetryPolicy{
			MaxAttempts: 4, BaseDelay: 2 * time.Second, MaxDelay: 30 * time.Second,
			// The ctx is the Stream call's: it says which run is waiting.
			OnRetry: func(ctx context.Context, attempt int, wait time.Duration, _ error) {
				narrated = append(narrated, fmt.Sprintf("%v: attempt %d failed, waiting %s", ctx.Value(runKey{}), attempt, wait))
			},
		}
		ctx := context.WithValue(context.Background(), runKey{}, "run 1")
		start := time.Now()

		stream, err := llm.WithRetry(provider, policy).Stream(ctx, nil, nil)
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		first := <-stream

		want := []string{"run 1: attempt 1 failed, waiting 7s", "run 1: attempt 2 failed, waiting 7s"}
		if !slices.Equal(narrated, want) || time.Since(start) != 14*time.Second || first.Token != "ok" {
			t.Errorf("narrated %q, first token %q after %v; want %q, ok after 14s", narrated, first.Token, time.Since(start), want)
		}
	})
}

// collect reads a stream to its close.
func collect(ch <-chan llm.StreamEvent) []llm.StreamEvent {
	var events []llm.StreamEvent
	for ev := range ch {
		events = append(events, ev)
	}
	return events
}

// TestWithRetry_FirstEventError: a provider error that arrives as a
// stream's first event is retried like a failed call; once anything has
// been forwarded, or when it is not worth retrying, the stream passes as is.
func TestWithRetry_FirstEventError(t *testing.T) {
	busy := llm.StreamEvent{Done: true, Err: &llm.StreamError{Provider: "acme", Code: "overloaded_error", Detail: "Overloaded"}}
	refused := llm.StreamEvent{Done: true, Err: &llm.StreamError{Provider: "acme", Code: "invalid_request_error", Detail: "bad"}}
	answer := llmtest.Text("a", "b")
	turn := func(events ...llm.StreamEvent) []llm.StreamEvent { return events }

	tests := []struct {
		name      string
		turns     [][]llm.StreamEvent
		wantCalls int
		wantWaits []time.Duration
		want      []llm.StreamEvent
	}{
		{name: "busy twice, then an answer", turns: [][]llm.StreamEvent{turn(busy), turn(busy), answer},
			wantCalls: 3, wantWaits: []time.Duration{time.Second, 2 * time.Second}, want: answer},
		{name: "attempts run out: the last failure is handed on", turns: [][]llm.StreamEvent{turn(busy), turn(busy), turn(busy), answer},
			wantCalls: 3, wantWaits: []time.Duration{time.Second, 2 * time.Second}, want: turn(busy)},
		{name: "not worth retrying", turns: [][]llm.StreamEvent{turn(refused), answer},
			wantCalls: 1, want: turn(refused)},
		{name: "busy after a token is not retried", turns: [][]llm.StreamEvent{turn(llm.StreamEvent{Token: "a"}, busy), answer},
			wantCalls: 1, want: turn(llm.StreamEvent{Token: "a"}, busy)},
		{name: "a stream that closes empty", turns: [][]llm.StreamEvent{turn(), answer}, wantCalls: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				inner := llmtest.NewScripted(tc.turns...)
				var waits []time.Duration
				policy := llm.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Second, OnRetry: func(_ context.Context, _ int, wait time.Duration, _ error) {
					waits = append(waits, wait)
				}}

				stream, err := llm.WithRetry(inner, policy).Stream(context.Background(), nil, nil)
				if err != nil {
					t.Fatalf("Stream: %v", err)
				}
				got := collect(stream)

				if !reflect.DeepEqual(got, tc.want) {
					t.Errorf("events = %+v; want %+v", got, tc.want)
				}
				if inner.Calls() != tc.wantCalls || !slices.Equal(waits, tc.wantWaits) {
					t.Errorf("calls = %d, waits = %v; want %d, %v", inner.Calls(), waits, tc.wantCalls, tc.wantWaits)
				}
			})
		})
	}
}

// TestWithRetry_CancelBeforeFirstEvent: waiting for the first event must
// not outlive the caller's ctx.
func TestWithRetry_CancelBeforeFirstEvent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		start := time.Now()

		stream, err := llm.WithRetry(llmtest.Blocking(), llm.RetryPolicy{MaxAttempts: 3}).Stream(ctx, nil, nil)
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		events := collect(stream)

		if len(events) != 0 || time.Since(start) != time.Minute {
			t.Errorf("events = %+v after %v; want a closed stream after 1m", events, time.Since(start))
		}
	})
}
