package agent

import (
	"context"
	"sync"

	"github.com/latebit-io/nib/ai/llm"
)

// providerProxy delegates [llm.Provider.Stream] to whatever provider is
// active at call time. Used to plug [Agent.SetProvider]'s hot-swap into
// the foundation [upagent.Agent], which captures its provider once at
// construction time and has no way to swap it later.
//
// The proxy is the [llm.Provider] handed to [upagent.New]. Stream-side
// reads share an [sync.RWMutex] read lock so concurrent turns do not
// contend; [providerProxy.Set] takes the write lock briefly when the
// developer toggles models in the TUI. A swap mid-Stream lets the in-
// flight call complete on the old provider — the local snapshot
// captured before the RUnlock is the one passed to Stream.
type providerProxy struct {
	mu sync.RWMutex
	p  llm.Provider
}

// newProviderProxy returns a proxy seeded with the initial provider.
// p must be non-nil; the foundation does not validate the provider it
// captures beyond [upagent.New]'s own non-nil check, which the proxy
// satisfies regardless of whether p was nil at this call.
func newProviderProxy(p llm.Provider) *providerProxy {
	return &providerProxy{p: p}
}

// Stream delegates to the live provider under the read lock.
func (pp *providerProxy) Stream(ctx context.Context, messages []llm.Message, tools []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	pp.mu.RLock()
	p := pp.p
	pp.mu.RUnlock()
	return p.Stream(ctx, messages, tools)
}

// Set installs a new provider for subsequent Stream calls. Calls
// already in flight retain their original provider via the local
// captured before the RUnlock.
func (pp *providerProxy) Set(p llm.Provider) {
	pp.mu.Lock()
	pp.p = p
	pp.mu.Unlock()
}
