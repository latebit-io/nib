package agent

import (
	"context"
	"sync"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit/budget"
)

// providerProxy delegates [llm.Provider.Stream] to whatever provider is
// active at call time. Used to plug [Agent.SetProvider]'s hot-swap into
// the kit/foundation [agent.Agent], which captures its provider once at
// construction time and has no way to swap it later.
//
// The proxy is the [llm.Provider] handed to [kit.New]. Stream-side
// reads share an [sync.RWMutex] read lock so concurrent turns do not
// contend; [providerProxy.Set] takes the write lock briefly when the
// developer toggles models in the TUI. A swap mid-Stream lets the in-
// flight call complete on the old provider — the local snapshot
// captured before the RUnlock is the one passed to Stream.
//
// The proxy ALSO owns the per-run session-usage counter. The wrapper
// goroutine attached to Stream's returned channel intercepts the
// terminal Done event and records its Usage synchronously, before
// forwarding Done downstream. Because drainStream sees Done only
// after the wrapper has recorded usage, the foundation's next
// TransformContext (which reads [providerProxy.Snapshot] via
// [Agent.foundationBudgetCheck]) is guaranteed to see fresh totals
// — eliminating the race where coding's async forwarder could
// otherwise let one extra Stream call slip past the budget gate.
type providerProxy struct {
	mu sync.RWMutex
	p  llm.Provider

	sessionMu sync.Mutex
	session   budget.Session
}

// newProviderProxy returns a proxy seeded with the initial provider.
// p must be non-nil; the foundation does not validate the provider it
// captures beyond [agent.New]'s own non-nil check, which the proxy
// satisfies regardless of whether p was nil at this call.
func newProviderProxy(p llm.Provider) *providerProxy {
	return &providerProxy{p: p}
}

// Stream delegates to the live provider under the read lock and
// returns a wrapper channel that intercepts the terminal Done event
// to record per-Stream usage synchronously before forwarding.
//
// Synchronous accumulation is the contract the foundation's pre-
// Stream budget check relies on: by the time foundation's drainStream
// has observed Done and the loop has re-entered TransformContext for
// the next turn, the new turn's TransformContext sees the previous
// turn's tokens already committed to [providerProxy.session].
func (pp *providerProxy) Stream(ctx context.Context, messages []llm.Message, tools []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	pp.mu.RLock()
	p := pp.p
	pp.mu.RUnlock()

	inner, err := p.Stream(ctx, messages, tools)
	if err != nil {
		return nil, err
	}

	out := make(chan llm.StreamEvent, cap(inner))
	go func() {
		defer close(out)
		for ev := range inner {
			if ev.Done && ev.Usage != nil {
				// Record BEFORE forwarding so drainStream's observation of
				// Done synchronizes with our session-usage update — the
				// foundation cannot start the next TransformContext until
				// drainStream returns, and drainStream cannot return until
				// it consumes Done from this channel.
				pp.recordUsage(ev.Usage)
			}
			out <- ev
		}
	}()
	return out, nil
}

// Set installs a new provider for subsequent Stream calls. Calls
// already in flight retain their original provider via the local
// captured before the RUnlock.
func (pp *providerProxy) Set(p llm.Provider) {
	pp.mu.Lock()
	pp.p = p
	pp.mu.Unlock()
}

// recordUsage accumulates one Stream's usage into the per-run session
// total. Called from the Stream wrapper goroutine on the inner
// stream's Done event.
func (pp *providerProxy) recordUsage(u *llm.Usage) {
	pp.sessionMu.Lock()
	pp.session.TotalPromptTokens += u.PromptTokens
	pp.session.TotalCompletionTokens += u.CompletionTokens
	pp.session.TotalCachedTokens += u.CachedTokens
	pp.session.Turns++
	pp.sessionMu.Unlock()
}

// Snapshot returns a copy of the current per-run session usage. Safe
// to call from any goroutine.
func (pp *providerProxy) Snapshot() budget.Session {
	pp.sessionMu.Lock()
	defer pp.sessionMu.Unlock()
	return pp.session
}

// ResetSession zeroes the per-run session counter. Called from
// [Agent.RunWithMode] and [Agent.Reply] resume path so each run
// starts with a fresh budget.
func (pp *providerProxy) ResetSession() {
	pp.sessionMu.Lock()
	pp.session = budget.Session{}
	pp.sessionMu.Unlock()
}
