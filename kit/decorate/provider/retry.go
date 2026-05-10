// Package provider hosts bundled [github.com/latebit-io/nib/kit.ProviderDecorator]
// implementations. Import as `providerdec "github.com/latebit-io/nib/kit/decorate/provider"`
// to wrap an [llm.Provider] with retry, caching, tracing, etc.
package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit"
)

// RetryPolicy configures [WithRetry]. The zero value is invalid;
// callers must populate at least MaxAttempts and IsRetryable.
type RetryPolicy struct {
	// MaxAttempts is the maximum number of Stream calls including the
	// first. Must be >= 1; a value of 1 means "no retries."
	MaxAttempts int

	// BaseDelay is the delay before the first retry. Subsequent retries
	// double the delay (capped by MaxDelay). Zero means "retry
	// immediately." Negative is treated as zero.
	BaseDelay time.Duration

	// MaxDelay caps the exponential backoff. Zero means uncapped.
	MaxDelay time.Duration

	// IsRetryable decides whether a given error should trigger a retry.
	// REQUIRED — there is no default. A nil matcher would either retry
	// every error (silently looping on permanent failures like 401) or
	// retry nothing (defeating the decorator's purpose); refusing to
	// guess at construction time makes the policy explicit.
	IsRetryable func(error) bool
}

// validate ensures the policy is usable. Returns a non-nil error when
// MaxAttempts < 1 or IsRetryable is nil. Surfaced by [WithRetry] so
// composition-root misconfiguration fails loudly at startup, not on
// the first retry.
func (p RetryPolicy) validate() error {
	if p.MaxAttempts < 1 {
		return fmt.Errorf("provider.WithRetry: MaxAttempts must be >= 1 (got %d)", p.MaxAttempts)
	}
	if p.IsRetryable == nil {
		return fmt.Errorf("provider.WithRetry: IsRetryable must be non-nil")
	}
	return nil
}

// WithRetry returns a [kit.ProviderDecorator] that retries
// [llm.Provider.Stream]'s connection-establishment phase under the
// given policy.
//
// Scope: WithRetry retries only the synchronous error returned by
// Stream BEFORE the event channel begins flowing. Mid-stream failures
// — provider-side disconnections after some tokens have streamed, or
// the [llm.StreamEvent.Truncated] flag — are NOT retried. Re-driving
// a partially-consumed turn would duplicate or interleave tokens; a
// truncation recovery belongs to [agent.Hooks.OnTruncated], not here.
//
// Recommended chain position: outermost or near-outermost. Place
// retry before tracing so the trace captures every attempt; place
// retry after a hot-swap/lifecycle decorator so a swap mid-attempt
// is respected on the next try.
//
// Panics at construction if policy is invalid (see [RetryPolicy.validate]).
// Composition-root code should let this panic — a misconfigured retry
// is a programming error, not a runtime condition.
func WithRetry(policy RetryPolicy) kit.ProviderDecorator {
	if err := policy.validate(); err != nil {
		panic(err)
	}
	return func(inner llm.Provider) llm.Provider {
		return &retryProvider{inner: inner, policy: policy}
	}
}

// retryProvider implements [llm.Provider] by re-calling the inner
// provider's Stream on retryable errors. Bound to a single inner
// provider for its lifetime; concurrent Stream calls are safe because
// no shared state mutates.
type retryProvider struct {
	inner  llm.Provider
	policy RetryPolicy
}

// Compile-time assertion that retryProvider satisfies the port.
var _ llm.Provider = (*retryProvider)(nil)

// Stream calls the inner provider's Stream, retrying on retryable
// errors per the policy. Respects ctx — a context-cancelled retry
// loop returns ctx.Err() without sleeping further.
func (r *retryProvider) Stream(ctx context.Context, messages []llm.Message, tools []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	var lastErr error
	delay := r.policy.BaseDelay
	if delay < 0 {
		delay = 0
	}
	for attempt := 0; attempt < r.policy.MaxAttempts; attempt++ {
		ch, err := r.inner.Stream(ctx, messages, tools)
		if err == nil {
			return ch, nil
		}
		lastErr = err
		if !r.policy.IsRetryable(err) {
			return nil, err
		}
		// On the final attempt, do not sleep — caller wants the error now.
		if attempt == r.policy.MaxAttempts-1 {
			break
		}
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		// Exponential backoff with optional cap.
		if delay == 0 {
			// First iteration with BaseDelay=0 stays at 0 — caller asked
			// for immediate retries.
		} else {
			delay *= 2
			if r.policy.MaxDelay > 0 && delay > r.policy.MaxDelay {
				delay = r.policy.MaxDelay
			}
		}
	}
	return nil, lastErr
}
