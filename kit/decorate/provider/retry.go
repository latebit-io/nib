// Package provider hosts bundled [github.com/latebit-io/nib/kit.ProviderDecorator]
// implementations. Import as `providerdec "github.com/latebit-io/nib/kit/decorate/provider"`
// to wrap an [llm.Provider] with retry, caching, tracing, etc.
package provider

import (
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit"
)

// RetryPolicy is [llm.RetryPolicy], named here for decorator chains.
type RetryPolicy = llm.RetryPolicy

// WithRetry adapts [llm.WithRetry] to a [kit.ProviderDecorator]; chain it
// outermost. It panics on an invalid policy here, at the composition
// root, not when the chain is applied.
func WithRetry(policy RetryPolicy) kit.ProviderDecorator {
	if err := policy.Validate(); err != nil {
		panic(err)
	}
	return func(inner llm.Provider) llm.Provider { return llm.WithRetry(inner, policy) }
}
