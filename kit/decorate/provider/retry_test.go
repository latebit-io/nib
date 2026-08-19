package provider

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/contracttest"
)

// flakyProvider returns errFail on the first n calls then succeeds.
type flakyProvider struct {
	failUntil int32
	calls     int32
	errFail   error
}

func (f *flakyProvider) Stream(_ context.Context, _ []llm.Message, _ []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	n := atomic.AddInt32(&f.calls, 1)
	if n <= f.failUntil {
		return nil, f.errFail
	}
	ch := make(chan llm.StreamEvent, 1)
	ch <- llm.StreamEvent{Token: "ok", Done: true}
	close(ch)
	return ch, nil
}

func TestWithRetry_SucceedsWithinBudget(t *testing.T) {
	errTransient := errors.New("transient")
	inner := &flakyProvider{failUntil: 2, errFail: errTransient}
	p := kit.DecorateProvider(inner, WithRetry(RetryPolicy{
		MaxAttempts: 5,
		IsRetryable: func(err error) bool { return errors.Is(err, errTransient) },
	}))
	ch, err := p.Stream(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("Stream returned error after retries: %v", err)
	}
	ev := <-ch
	if ev.Token != "ok" || !ev.Done {
		t.Fatalf("unexpected event: %+v", ev)
	}
	if got := atomic.LoadInt32(&inner.calls); got != 3 {
		t.Fatalf("expected 3 calls (2 fail + 1 success), got %d", got)
	}
}

func TestWithRetry_ExhaustsBudget(t *testing.T) {
	errTransient := errors.New("transient")
	inner := &flakyProvider{failUntil: 99, errFail: errTransient}
	p := kit.DecorateProvider(inner, WithRetry(RetryPolicy{
		MaxAttempts: 3,
		IsRetryable: func(err error) bool { return true },
	}))
	_, err := p.Stream(context.Background(), nil, nil)
	if !errors.Is(err, errTransient) {
		t.Fatalf("expected errTransient, got %v", err)
	}
	if got := atomic.LoadInt32(&inner.calls); got != 3 {
		t.Fatalf("expected 3 calls before giving up, got %d", got)
	}
}

func TestWithRetry_NonRetryableReturnsImmediately(t *testing.T) {
	errPermanent := errors.New("401 unauthorized")
	inner := &flakyProvider{failUntil: 99, errFail: errPermanent}
	p := kit.DecorateProvider(inner, WithRetry(RetryPolicy{
		MaxAttempts: 5,
		IsRetryable: func(err error) bool { return false },
	}))
	_, err := p.Stream(context.Background(), nil, nil)
	if !errors.Is(err, errPermanent) {
		t.Fatalf("expected errPermanent, got %v", err)
	}
	if got := atomic.LoadInt32(&inner.calls); got != 1 {
		t.Fatalf("expected 1 call for non-retryable error, got %d", got)
	}
}

func TestWithRetry_RespectsContextCancellation(t *testing.T) {
	errTransient := errors.New("transient")
	inner := &flakyProvider{failUntil: 99, errFail: errTransient}
	p := kit.DecorateProvider(inner, WithRetry(RetryPolicy{
		MaxAttempts: 5,
		BaseDelay:   200 * time.Millisecond,
		IsRetryable: func(err error) bool { return true },
	}))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := p.Stream(ctx, nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestWithRetry_ExponentialBackoffCapped(t *testing.T) {
	// Drive the delay math: BaseDelay=10ms, MaxDelay=15ms — after the
	// first sleep (10ms) the doubled 20ms should clamp to 15ms.
	errTransient := errors.New("transient")
	inner := &flakyProvider{failUntil: 3, errFail: errTransient}
	policy := RetryPolicy{
		MaxAttempts: 4,
		BaseDelay:   10 * time.Millisecond,
		MaxDelay:    15 * time.Millisecond,
		IsRetryable: func(err error) bool { return true },
	}
	p := kit.DecorateProvider(inner, WithRetry(policy))
	start := time.Now()
	_, err := p.Stream(context.Background(), nil, nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Stream errored: %v", err)
	}
	// 3 retry sleeps: 10ms + 15ms (capped) + 15ms (capped) = 40ms.
	// Allow generous slack for CI jitter.
	if elapsed < 40*time.Millisecond {
		t.Fatalf("expected >= 40ms of backoff, got %v", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("backoff ran far longer than expected: %v", elapsed)
	}
}

func TestWithRetry_PanicsOnInvalidPolicy(t *testing.T) {
	cases := []struct {
		name   string
		policy RetryPolicy
	}{
		{"zero MaxAttempts", RetryPolicy{IsRetryable: func(error) bool { return true }}},
		{"nil IsRetryable", RetryPolicy{MaxAttempts: 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("expected panic, got none")
				}
			}()
			_ = WithRetry(tc.policy)
		})
	}
}

func TestDecorateProvider_OrderingFirstIsOutermost(t *testing.T) {
	// Verify that the first-listed decorator wraps the second-listed
	// (which wraps base). Use two stub decorators that prepend tokens
	// to the first event.
	var base llm.Provider = &flakyProvider{failUntil: 0}
	prefixA := func(inner llm.Provider) llm.Provider {
		return providerFunc(func(ctx context.Context, m []llm.Message, t []llm.ToolDef) (<-chan llm.StreamEvent, error) {
			ch, err := inner.Stream(ctx, m, t)
			if err != nil {
				return nil, err
			}
			out := make(chan llm.StreamEvent, cap(ch))
			go func() {
				defer close(out)
				for ev := range ch {
					ev.Token = "A:" + ev.Token
					out <- ev
				}
			}()
			return out, nil
		})
	}
	prefixB := func(inner llm.Provider) llm.Provider {
		return providerFunc(func(ctx context.Context, m []llm.Message, t []llm.ToolDef) (<-chan llm.StreamEvent, error) {
			ch, err := inner.Stream(ctx, m, t)
			if err != nil {
				return nil, err
			}
			out := make(chan llm.StreamEvent, cap(ch))
			go func() {
				defer close(out)
				for ev := range ch {
					ev.Token = "B:" + ev.Token
					out <- ev
				}
			}()
			return out, nil
		})
	}
	p := kit.DecorateProvider(base, prefixA, prefixB)
	ch, err := p.Stream(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("Stream errored: %v", err)
	}
	ev := <-ch
	// A is outermost — its prefix is applied LAST on the way out.
	if ev.Token != "A:B:ok" {
		t.Fatalf("expected A:B:ok, got %q", ev.Token)
	}
}

func TestDecorateProvider_NilDecoratorsSkipped(t *testing.T) {
	base := &flakyProvider{failUntil: 0}
	p := kit.DecorateProvider(base, nil, nil)
	ch, err := p.Stream(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("Stream errored: %v", err)
	}
	ev := <-ch
	if ev.Token != "ok" {
		t.Fatalf("expected base unchanged, got %q", ev.Token)
	}
}

// providerFunc adapts a function into an [llm.Provider]. Test-only.
type providerFunc func(context.Context, []llm.Message, []llm.ToolDef) (<-chan llm.StreamEvent, error)

func (f providerFunc) Stream(ctx context.Context, m []llm.Message, t []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	return f(ctx, m, t)
}

// TestRetryProvider_SatisfiesContract verifies the retry decorator
// passes the [llm.Provider] contract suite when wrapping a happy-path
// inner. Locks in the invariant that adding/extending the retry
// decorator must not break channel-closure, no-events-after-Done, or
// concurrent-Stream guarantees.
func TestRetryProvider_SatisfiesContract(t *testing.T) {
	contracttest.Provider(t, func() llm.Provider {
		// Inner succeeds on first call; retry policy never fires.
		inner := &flakyProvider{failUntil: 0}
		return kit.DecorateProvider(inner, WithRetry(RetryPolicy{
			MaxAttempts: 3,
			IsRetryable: func(error) bool { return true },
		}))
	})
}

// escalatingProvider is a flakyProvider that also carries an output cap.
type escalatingProvider struct {
	flakyProvider
	cap int
}

func (e *escalatingProvider) MaxTokens() int     { return e.cap }
func (e *escalatingProvider) SetMaxTokens(v int) { e.cap = v }

// TestWithRetry_ForwardsOutputCapEscalator: the retry wrapper must not
// hide the inner provider's llm.OutputCapEscalator, and must not claim
// it when the inner provider lacks it.
func TestWithRetry_ForwardsOutputCapEscalator(t *testing.T) {
	policy := RetryPolicy{MaxAttempts: 1, IsRetryable: func(error) bool { return false }}

	inner := &escalatingProvider{cap: 10}
	p := kit.DecorateProvider(inner, WithRetry(policy))
	esc, ok := p.(llm.OutputCapEscalator)
	if !ok {
		t.Fatalf("decorated provider %T does not forward llm.OutputCapEscalator", p)
	}
	if esc.MaxTokens() != 10 {
		t.Fatalf("MaxTokens = %d, want 10", esc.MaxTokens())
	}
	esc.SetMaxTokens(20)
	if inner.cap != 20 {
		t.Fatalf("SetMaxTokens not forwarded: inner cap = %d", inner.cap)
	}

	plain := kit.DecorateProvider(&flakyProvider{}, WithRetry(policy))
	if _, ok := plain.(llm.OutputCapEscalator); ok {
		t.Fatalf("decorated plain provider %T must not claim llm.OutputCapEscalator", plain)
	}
}
