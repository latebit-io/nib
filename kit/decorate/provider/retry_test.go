package provider

import (
	"context"
	"sync/atomic"
	"testing"

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

func TestWithRetry_PanicsOnInvalidPolicy(t *testing.T) {
	cases := []struct {
		name   string
		policy RetryPolicy
	}{
		{"zero MaxAttempts", RetryPolicy{}},
		{"negative MaxAttempts", RetryPolicy{MaxAttempts: -1}},
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

// TestRetryProvider_SatisfiesContract: wrapping a provider in retry must
// keep the [llm.Provider] contract (channel closure, nothing after Done,
// concurrent Stream calls).
func TestRetryProvider_SatisfiesContract(t *testing.T) {
	contracttest.Provider(t, func() llm.Provider {
		// Inner succeeds on first call; retry policy never fires.
		inner := &flakyProvider{failUntil: 0}
		return kit.DecorateProvider(inner, WithRetry(RetryPolicy{MaxAttempts: 3}))
	})
}
