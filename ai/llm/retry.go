package llm

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// RetryPolicy configures [WithRetry]. Only MaxAttempts is required: the
// zero value of every other field is a usable default.
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

	// OnRetry, when set, is called before each wait with the attempt that
	// failed (1-based), the wait ahead and the error. It lets a host say
	// "rate limited, retrying in 4s" instead of going silent.
	OnRetry func(attempt int, wait time.Duration, err error)
}

// Validate reports a policy [WithRetry] refuses to run with.
func (p RetryPolicy) Validate() error {
	if p.MaxAttempts < 1 {
		return fmt.Errorf("llm.WithRetry: MaxAttempts must be >= 1 (got %d)", p.MaxAttempts)
	}
	return nil
}

// IsTemporary reports whether err is a provider answer that may succeed
// if the request is sent again; see [StatusError.Temporary]. A transport
// failure is not one: the caller's own deadline would look the same.
func IsTemporary(err error) bool {
	status, ok := errors.AsType[*StatusError](err)
	return ok && status.Temporary()
}

// WithRetry wraps inner so that an error Stream returns before any event
// flows is retried under policy; a failure mid-stream never is. It
// forwards [OutputCapEscalator]. Panics on an invalid policy.
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

// Stream calls the inner provider's Stream, retrying per the policy. It
// waits the longer of the backoff and the provider's Retry-After, and
// returns ctx.Err() when ctx ends during a wait.
func (r *retryProvider) Stream(ctx context.Context, messages []Message, tools []ToolDef) (<-chan StreamEvent, error) {
	limit := r.policy.MaxDelay
	delay := r.policy.BaseDelay
	for attempt := 1; ; attempt++ {
		ch, err := r.inner.Stream(ctx, messages, tools)
		if err == nil {
			return ch, nil
		}
		if attempt == r.policy.MaxAttempts || !r.policy.IsRetryable(err) {
			return nil, err
		}

		wait := min(delay, limit)
		if status, ok := errors.AsType[*StatusError](err); ok {
			wait = max(wait, status.RetryAfter)
		}
		if wait > limit {
			return nil, err
		}
		if r.policy.OnRetry != nil {
			r.policy.OnRetry(attempt, wait, err)
		}
		if wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		// Doubling stops at the cap, which also keeps it from overflowing.
		delay = min(delay, limit/2) * 2
	}
}
