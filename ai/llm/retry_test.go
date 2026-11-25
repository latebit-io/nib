package llm

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// flakyProvider fails its first failUntil calls with errFail, then succeeds.
type flakyProvider struct {
	failUntil int32
	calls     atomic.Int32
	errFail   error
}

func (f *flakyProvider) Stream(context.Context, []Message, []ToolDef) (<-chan StreamEvent, error) {
	if f.calls.Add(1) <= f.failUntil {
		return nil, f.errFail
	}
	ch := make(chan StreamEvent, 1)
	ch <- StreamEvent{Token: "ok", Done: true}
	close(ch)
	return ch, nil
}

// TestWithRetry covers which errors are retried, how long each wait is and
// what OnRetry is told. The inner provider fails twice, then succeeds.
// Waits run on a fake clock.
func TestWithRetry(t *testing.T) {
	type retry struct {
		attempt int
		wait    time.Duration
	}
	limited := func(retryAfter time.Duration) error {
		return &StatusError{Provider: "acme", StatusCode: 429, Detail: "slow down", RetryAfter: retryAfter}
	}
	plain := errors.New("dial tcp: connection refused")
	always := func(error) bool { return true }

	tests := []struct {
		name      string
		err       error
		policy    RetryPolicy
		wantCalls int32
		wantRetry []retry
		wantErr   bool
	}{
		{
			name:      "rate limit is retried with backoff",
			err:       limited(0),
			policy:    RetryPolicy{MaxAttempts: 4, BaseDelay: time.Second},
			wantCalls: 3,
			wantRetry: []retry{{1, time.Second}, {2, 2 * time.Second}},
		},
		{
			name:      "backoff stops doubling at MaxDelay",
			err:       limited(0),
			policy:    RetryPolicy{MaxAttempts: 4, BaseDelay: 10 * time.Second, MaxDelay: 15 * time.Second},
			wantCalls: 3,
			wantRetry: []retry{{1, 10 * time.Second}, {2, 15 * time.Second}},
		},
		{
			name:      "no delay configured retries at once",
			err:       limited(0),
			policy:    RetryPolicy{MaxAttempts: 4},
			wantCalls: 3,
			wantRetry: []retry{{1, 0}, {2, 0}},
		},
		{
			name:      "Retry-After wins when it is the longer wait",
			err:       limited(7 * time.Second),
			policy:    RetryPolicy{MaxAttempts: 4, BaseDelay: time.Second, MaxDelay: 30 * time.Second},
			wantCalls: 3,
			wantRetry: []retry{{1, 7 * time.Second}, {2, 7 * time.Second}},
		},
		{
			name:      "backoff wins when it is the longer wait",
			err:       limited(time.Second),
			policy:    RetryPolicy{MaxAttempts: 4, BaseDelay: 4 * time.Second},
			wantCalls: 3,
			wantRetry: []retry{{1, 4 * time.Second}, {2, 8 * time.Second}},
		},
		{
			name:      "Retry-After beyond MaxDelay is not waited for",
			err:       limited(time.Minute),
			policy:    RetryPolicy{MaxAttempts: 4, BaseDelay: time.Second, MaxDelay: 30 * time.Second},
			wantCalls: 1,
			wantErr:   true,
		},
		{
			name:      "attempts run out",
			err:       limited(0),
			policy:    RetryPolicy{MaxAttempts: 2, BaseDelay: time.Second},
			wantCalls: 2,
			wantRetry: []retry{{1, time.Second}},
			wantErr:   true,
		},
		{
			name:      "bad request is not retried",
			err:       &StatusError{Provider: "acme", StatusCode: 400},
			policy:    RetryPolicy{MaxAttempts: 4},
			wantCalls: 1,
			wantErr:   true,
		},
		{
			name:      "an error that is not a provider status is not retried",
			err:       plain,
			policy:    RetryPolicy{MaxAttempts: 4},
			wantCalls: 1,
			wantErr:   true,
		},
		{
			name:      "a custom IsRetryable decides instead",
			err:       plain,
			policy:    RetryPolicy{MaxAttempts: 4, IsRetryable: always},
			wantCalls: 3,
			wantRetry: []retry{{1, 0}, {2, 0}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				inner := &flakyProvider{failUntil: 2, errFail: tc.err}
				var got []retry
				tc.policy.OnRetry = func(attempt int, wait time.Duration, err error) {
					if !errors.Is(err, tc.err) {
						t.Errorf("OnRetry error = %v; want %v", err, tc.err)
					}
					got = append(got, retry{attempt, wait})
				}
				start := time.Now()

				_, err := WithRetry(inner, tc.policy).Stream(context.Background(), nil, nil)

				if tc.wantErr != errors.Is(err, tc.err) || (!tc.wantErr && err != nil) {
					t.Errorf("Stream error = %v; want the provider's error: %v", err, tc.wantErr)
				}
				if calls := inner.calls.Load(); calls != tc.wantCalls {
					t.Errorf("provider calls = %d; want %d", calls, tc.wantCalls)
				}
				if !slices.Equal(got, tc.wantRetry) {
					t.Errorf("OnRetry calls = %v; want %v", got, tc.wantRetry)
				}
				var waited time.Duration
				for _, r := range tc.wantRetry {
					waited += r.wait
				}
				if elapsed := time.Since(start); elapsed != waited {
					t.Errorf("waited %v; want %v", elapsed, waited)
				}
			})
		})
	}
}

// TestWithRetry_CancelDuringWait: a cancelled ctx ends the wait at once.
func TestWithRetry_CancelDuringWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inner := &flakyProvider{failUntil: 99, errFail: &StatusError{StatusCode: 503}}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		start := time.Now()

		_, err := WithRetry(inner, RetryPolicy{MaxAttempts: 5, BaseDelay: time.Hour}).Stream(ctx, nil, nil)

		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 50*time.Millisecond {
			t.Errorf("Stream = %v after %v; want DeadlineExceeded after 50ms", err, time.Since(start))
		}
	})
}

func TestWithRetry_PanicsOnInvalidPolicy(t *testing.T) {
	for _, attempts := range []int{0, -1} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("MaxAttempts %d: no panic", attempts)
				}
			}()
			WithRetry(&flakyProvider{}, RetryPolicy{MaxAttempts: attempts})
		}()
	}
}

// escalatingProvider is a flakyProvider that also carries an output cap.
type escalatingProvider struct {
	flakyProvider
	cap int
}

func (e *escalatingProvider) MaxTokens() int     { return e.cap }
func (e *escalatingProvider) SetMaxTokens(v int) { e.cap = v }

// TestWithRetry_ForwardsOutputCapEscalator: the wrapper must not hide the
// inner provider's output cap, nor claim one the inner provider lacks.
func TestWithRetry_ForwardsOutputCapEscalator(t *testing.T) {
	policy := RetryPolicy{MaxAttempts: 1}

	inner := &escalatingProvider{cap: 10}
	esc, ok := WithRetry(inner, policy).(OutputCapEscalator)
	if !ok || esc.MaxTokens() != 10 {
		t.Fatalf("wrapped provider does not forward the output cap (ok %v)", ok)
	}
	esc.SetMaxTokens(20)
	if inner.cap != 20 {
		t.Errorf("SetMaxTokens not forwarded: inner cap = %d", inner.cap)
	}
	if _, ok := WithRetry(&flakyProvider{}, policy).(OutputCapEscalator); ok {
		t.Error("wrapped plain provider claims an output cap")
	}
}

func TestIsTemporary(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"rate limit", &StatusError{StatusCode: 429}, true},
		{"wrapped rate limit", errors.Join(errors.New("turn 3"), &StatusError{StatusCode: 429}), true},
		{"bad request", &StatusError{StatusCode: 400}, false},
		{"rejected credential", &AuthError{StatusCode: 401}, false},
		{"caller deadline", context.DeadlineExceeded, false},
		{"nil", nil, false},
	}
	for _, tc := range tests {
		if got := IsTemporary(tc.err); got != tc.want {
			t.Errorf("%s: IsTemporary = %v; want %v", tc.name, got, tc.want)
		}
	}
}
