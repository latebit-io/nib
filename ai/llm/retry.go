package llm

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// RetryPolicy configures [WithRetry]. Only MaxAttempts is required: the
// zero value of every other field is a usable default. To test a policy's
// waits, run the test in testing/synctest: they then cost no real time.
type RetryPolicy struct {
	// MaxAttempts is the maximum number of Stream calls including the
	// first. Must be >= 1; a value of 1 means "no retries."
	MaxAttempts int

	// BaseDelay is the wait before the first retry; each further retry
	// doubles it, up to MaxDelay. Zero or negative retries immediately.
	BaseDelay time.Duration

	// MaxDelay caps a single wait; zero means uncapped. When a provider
	// asks for longer through Retry-After, the error is returned instead:
	// retrying sooner would only be refused again.
	MaxDelay time.Duration

	// IsRetryable decides whether an error is worth another attempt. Nil
	// means [IsTemporary]: timeouts, rate limits and provider-side failures.
	IsRetryable func(error) bool

	// OnRetry, when set, is called before each wait with the Stream call's
	// ctx, the attempt that failed (1-based), the wait ahead and the error.
	// It lets a host say "rate limited, retrying in 4s" into the right run.
	OnRetry func(ctx context.Context, attempt int, wait time.Duration, err error)
}

// Validate reports a policy [WithRetry] refuses to run with.
func (p RetryPolicy) Validate() error {
	if p.MaxAttempts < 1 {
		return fmt.Errorf("llm.WithRetry: MaxAttempts must be >= 1 (got %d)", p.MaxAttempts)
	}
	return nil
}

// IsTemporary reports whether err is a provider answer that may succeed
// if the request is sent again: a temporary [StatusError] or [StreamError].
// A transport failure is not one: the caller's own deadline looks the same.
func IsTemporary(err error) bool {
	if status, ok := errors.AsType[*StatusError](err); ok {
		return status.Temporary()
	}
	reported, ok := errors.AsType[*StreamError](err)
	return ok && reported.Temporary()
}

// WithRetry retries, under policy, a failure before any event is forwarded:
// an error from Stream, or one its first event carries. So Stream returns at
// that event, the last one for a turn of tool calls alone. Panics on an invalid policy.
func WithRetry(inner Provider, policy RetryPolicy) Provider {
	if err := policy.Validate(); err != nil {
		panic(err)
	}
	if policy.IsRetryable == nil {
		policy.IsRetryable = IsTemporary
	}
	policy.BaseDelay = max(0, policy.BaseDelay)
	if policy.MaxDelay <= 0 {
		policy.MaxDelay = math.MaxInt64
	}
	rp := &retryProvider{inner: inner, policy: policy}
	// Claim the output-cap capability only when the wrapped provider has it.
	if esc, ok := inner.(OutputCapEscalator); ok {
		return &escalatingRetryProvider{retryProvider: rp, OutputCapEscalator: esc}
	}
	return rp
}

// retryProvider re-calls its inner provider's Stream per the policy. It
// holds no mutable state, so concurrent Stream calls are safe.
type retryProvider struct {
	inner  Provider
	policy RetryPolicy
}

// escalatingRetryProvider is a retryProvider over an inner provider that
// also implements [OutputCapEscalator].
type escalatingRetryProvider struct {
	*retryProvider
	OutputCapEscalator
}

// Stream opens a stream, retrying per the policy, and returns once its
// first event is in. It waits the longer of the backoff and the provider's
// Retry-After, and returns ctx.Err() when ctx ends before the next attempt.
func (r *retryProvider) Stream(ctx context.Context, messages []Message, tools []ToolDef) (<-chan StreamEvent, error) {
	limit := r.policy.MaxDelay
	delay := r.policy.BaseDelay
	for attempt := 1; ; attempt++ {
		stream, err := r.attempt(ctx, messages, tools)
		if err == nil {
			return stream, nil
		}
		wait := min(delay, limit)
		if status, ok := errors.AsType[*StatusError](err); ok {
			wait = max(wait, status.RetryAfter)
		}
		if attempt == r.policy.MaxAttempts || !r.policy.IsRetryable(err) || wait > limit {
			// A stream that failed at its first event is handed on as it
			// came: its consumer reads the error where the provider put it.
			if stream != nil {
				return stream, nil
			}
			return nil, err
		}
		if r.policy.OnRetry != nil {
			r.policy.OnRetry(ctx, attempt, wait, err)
		}
		if wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
			}
		}
		// Checked after every wait, a zero one included: OnRetry may cancel.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Doubling stops at the cap, which also keeps it from overflowing.
		delay = min(delay, limit/2) * 2
	}
}

// attempt opens one stream and reads its first event. err is why the
// attempt failed: what Stream returned, or what that first event carries,
// in which case the returned stream still yields the event.
func (r *retryProvider) attempt(ctx context.Context, messages []Message, tools []ToolDef) (<-chan StreamEvent, error) {
	ch, err := r.inner.Stream(ctx, messages, tools)
	if err != nil {
		return nil, err
	}
	select {
	case first, open := <-ch:
		if !open {
			return ch, nil
		}
		return prepend(ctx, first, ch), first.Err
	case <-ctx.Done():
		// The provider closes ch on its own; the consumer sees that.
		return ch, nil
	}
}

// prepend returns a stream of first followed by rest. It may be dropped
// unread, as a failed attempt is: the copying ends when rest closes or
// ctx does.
func prepend(ctx context.Context, first StreamEvent, rest <-chan StreamEvent) <-chan StreamEvent {
	out := make(chan StreamEvent, 1)
	out <- first
	go func() {
		defer close(out)
		for ev := range rest {
			if !trySend(ctx, out, ev) {
				return
			}
		}
	}()
	return out
}
