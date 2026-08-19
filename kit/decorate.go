package kit

// The three Decorate* functions in this file apply the
// cross-cutting-concern pattern (a wrapper that implements the port and
// delegates to an inner value) uniformly to all kit-side ports. Bundled
// decorator implementations live in `kit/decorate/{provider,store,tool}`.

import (
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit/memory"
)

// (See `/nib/engineering-values.md#2-decorator-chain-for-cross-cutting`
// and `/nib/architecture.md#provider-chain-decorator-pattern` for the
// design rationale.)

// ProviderDecorator wraps an [llm.Provider] with a cross-cutting
// concern (retry, tracing, caching, rate-limiting, hot-swap, lifecycle
// accounting). Implementations call the wrapped provider through their
// own [llm.Provider.Stream] implementation; the decorator type itself
// is a plain function so chains compose by function application.
//
// Decorators are the canonical pattern for cross-cutting concerns in
// the nib codebase: consumers wrap a provider with hot-swap or usage
// accounting the same way. Bundled reusable decorators live under
// `kit/decorate/provider`.
type ProviderDecorator func(llm.Provider) llm.Provider

// StoreDecorator wraps a [memory.Store] with a cross-cutting concern
// (caching, retry, tracing, replication). Composition rules match
// [ProviderDecorator]. Bundled decorators live under
// `kit/decorate/store`.
type StoreDecorator func(memory.Store) memory.Store

// ToolDecorator wraps a [Tool] with a cross-cutting concern (tracing,
// rate-limiting, dry-run, sandbox). Composition rules match
// [ProviderDecorator]. Bundled decorators live under
// `kit/decorate/tool`.
type ToolDecorator func(Tool) Tool

// DecorateProvider composes decorators around base. The first decorator
// in the list ends up as the outermost wrapper:
//
//	DecorateProvider(base, A, B, C)
//
// produces A(B(C(base))) — calls hit A first, then B, then C, then base.
// This matches HTTP-middleware ordering: listed-first runs first on the
// way in.
//
// Decorators with no entries return base unchanged. Nil decorators are
// skipped (passing nil is legal and equivalent to omitting it).
func DecorateProvider(base llm.Provider, decorators ...ProviderDecorator) llm.Provider {
	p := base
	for i := len(decorators) - 1; i >= 0; i-- {
		if decorators[i] == nil {
			continue
		}
		p = decorators[i](p)
	}
	return p
}

// DecorateStore composes decorators around base. Ordering rules match
// [DecorateProvider].
func DecorateStore(base memory.Store, decorators ...StoreDecorator) memory.Store {
	s := base
	for i := len(decorators) - 1; i >= 0; i-- {
		if decorators[i] == nil {
			continue
		}
		s = decorators[i](s)
	}
	return s
}

// DecorateTool composes decorators around base. Ordering rules match
// [DecorateProvider].
func DecorateTool(base Tool, decorators ...ToolDecorator) Tool {
	t := base
	for i := len(decorators) - 1; i >= 0; i-- {
		if decorators[i] == nil {
			continue
		}
		t = decorators[i](t)
	}
	return t
}
