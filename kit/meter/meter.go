// Package meter sits between an agent and its provider: it counts the
// tokens a session uses, stops the session at a budget, and lets the
// provider be replaced while the agent lives.
package meter

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit/budget"
)

// ErrBudgetExceeded is in the chain of the error that refuses a
// request once the session has used its token budget.
var ErrBudgetExceeded = errors.New("token budget exceeded")

// Provider is an [llm.Provider] that forwards to the one set on it.
type Provider struct {
	mu    sync.RWMutex
	inner llm.Provider
	used  budget.Session
	limit int
}

// New returns a meter with no provider yet. limit is the session's
// budget in tokens of any kind; zero or less means none.
func New(limit int) *Provider {
	return &Provider{limit: limit}
}

// Set replaces the provider. A request in flight finishes on the old one.
func (m *Provider) Set(p llm.Provider) {
	m.mu.Lock()
	m.inner = p
	m.mu.Unlock()
}

// Usage returns what the session has used so far.
func (m *Provider) Usage() budget.Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.used
}

// Stream forwards to the current provider and counts the usage it
// reports, before passing the final event on. Once the budget is used
// up it refuses the request, which ends the run.
func (m *Provider) Stream(ctx context.Context, msgs []llm.Message, tools []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	m.mu.RLock()
	inner := m.inner
	m.mu.RUnlock()
	return m.stream(ctx, inner, msgs, tools)
}

// Branch returns a provider that sends requests to p and counts them in
// this session, budget included: a subagent on another model still
// spends the session's tokens.
func (m *Provider) Branch(p llm.Provider) llm.Provider { return &branch{meter: m, inner: p} }

// branch is a provider counted by another's meter.
type branch struct {
	meter *Provider
	inner llm.Provider
}

// Stream implements [llm.Provider].
func (b *branch) Stream(ctx context.Context, msgs []llm.Message, tools []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	return b.meter.stream(ctx, b.inner, msgs, tools)
}

// MaxTokens implements [llm.OutputCapEscalator] for the branch's own
// provider. Zero when it has none.
func (b *branch) MaxTokens() int {
	if esc, ok := b.inner.(llm.OutputCapEscalator); ok {
		return esc.MaxTokens()
	}
	return 0
}

// SetMaxTokens implements [llm.OutputCapEscalator]: a retry after a cut
// raises this provider's limit, not the session's.
func (b *branch) SetMaxTokens(n int) {
	if esc, ok := b.inner.(llm.OutputCapEscalator); ok {
		esc.SetMaxTokens(n)
	}
}

// stream sends a request to inner, refusing it once the budget is used,
// and counts what it uses.
func (m *Provider) stream(ctx context.Context, inner llm.Provider, msgs []llm.Message, tools []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	m.mu.RLock()
	used := m.used.Total()
	m.mu.RUnlock()
	if inner == nil {
		return nil, errors.New("meter: no provider set")
	}
	if m.limit > 0 && used >= m.limit {
		return nil, fmt.Errorf("%w: %d of %d tokens used in this session", ErrBudgetExceeded, used, m.limit)
	}
	in, err := inner.Stream(ctx, msgs, tools)
	if err != nil {
		return nil, err
	}
	out := make(chan llm.StreamEvent)
	go func() {
		defer close(out)
		for ev := range in {
			m.count(ev)
			select {
			case out <- ev:
			case <-ctx.Done():
				// Nobody reads any more. Drain so the provider can finish,
				// still counting: a stopped request is paid for too.
				for ev := range in {
					m.count(ev)
				}
				return
			}
		}
	}()
	return out, nil
}

// count adds the usage a stream's final event reports.
func (m *Provider) count(ev llm.StreamEvent) {
	if !ev.Done {
		return
	}
	m.mu.Lock()
	m.used.AddUsage(ev.Usage)
	m.used.Turns++
	m.mu.Unlock()
}

// escalator returns the current provider's output limit control, or
// nil when it has none.
func (m *Provider) escalator() llm.OutputCapEscalator {
	m.mu.RLock()
	defer m.mu.RUnlock()
	esc, _ := m.inner.(llm.OutputCapEscalator)
	return esc
}

// MaxTokens implements [llm.OutputCapEscalator] for the provider in
// use, so a wrapper does not hide its output limit. Zero when it has none.
func (m *Provider) MaxTokens() int {
	if esc := m.escalator(); esc != nil {
		return esc.MaxTokens()
	}
	return 0
}

// SetMaxTokens implements [llm.OutputCapEscalator]. It does nothing
// for a provider without an output limit to set.
func (m *Provider) SetMaxTokens(n int) {
	if esc := m.escalator(); esc != nil {
		esc.SetMaxTokens(n)
	}
}
