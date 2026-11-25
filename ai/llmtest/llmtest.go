// Package llmtest provides fake [llm.Provider]s and stream builders for
// tests of code that drives a provider.
package llmtest

import (
	"context"
	"slices"
	"sync"

	"github.com/latebit-io/nib/ai/llm"
)

// Scripted is a provider that serves one prepared stream per Stream call,
// in order. A call past the last one gets a stream that closes at once,
// which a consumer sees as a provider that hung up early.
type Scripted struct {
	mu      sync.Mutex
	turns   [][]llm.StreamEvent
	offered []int // tools given to each call so far
}

// NewScripted returns a [Scripted] that will serve turns.
func NewScripted(turns ...[]llm.StreamEvent) *Scripted {
	return &Scripted{turns: turns}
}

// Stream serves the next prepared stream, fully buffered and closed.
func (p *Scripted) Stream(_ context.Context, _ []llm.Message, tools []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	p.mu.Lock()
	p.offered = append(p.offered, len(tools))
	var events []llm.StreamEvent
	if len(p.turns) > 0 {
		events, p.turns = p.turns[0], p.turns[1:]
	}
	p.mu.Unlock()

	ch := make(chan llm.StreamEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

// Calls returns how many times Stream was called.
func (p *Scripted) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.offered)
}

// Offered returns the number of tools each Stream call was given.
func (p *Scripted) Offered() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.offered)
}

// Failing returns a provider whose Stream fails with err.
func Failing(err error) llm.Provider { return failing{err} }

type failing struct{ err error }

func (p failing) Stream(context.Context, []llm.Message, []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	return nil, p.err
}

// Blocking returns a provider that streams tokens and then never finishes
// the turn: it ends only when the caller's ctx does.
func Blocking(tokens ...string) llm.Provider { return blocking(tokens) }

type blocking []string

func (p blocking) Stream(context.Context, []llm.Message, []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent, len(p))
	for _, tok := range p {
		ch <- llm.StreamEvent{Token: tok}
	}
	return ch, nil
}

// Text is a turn that streams chunks and ends cleanly.
func Text(chunks ...string) []llm.StreamEvent { return TextUsage(nil, chunks...) }

// TextUsage is [Text] for a provider that reports usage with the turn.
func TextUsage(usage *llm.Usage, chunks ...string) []llm.StreamEvent {
	events := make([]llm.StreamEvent, 0, len(chunks)+1)
	for _, c := range chunks {
		events = append(events, llm.StreamEvent{Token: c})
	}
	return append(events, llm.StreamEvent{Done: true, Usage: usage})
}

// ToolCalls is a turn whose only content is the given tool calls.
func ToolCalls(calls ...llm.ToolCall) []llm.StreamEvent {
	return []llm.StreamEvent{{Done: true, ToolCalls: calls}}
}

// Done is a turn with no text and no tool calls: the model has nothing
// more to say.
func Done() []llm.StreamEvent {
	return []llm.StreamEvent{{Done: true}}
}

// Call builds a tool call with raw JSON arguments.
func Call(id, name, args string) llm.ToolCall {
	return llm.ToolCall{ID: id, Type: "function", Function: llm.FunctionCall{Name: name, Arguments: args}}
}
