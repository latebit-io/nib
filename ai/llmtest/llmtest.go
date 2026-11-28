// Package llmtest provides fake [llm.Provider]s and stream builders for
// tests of code that drives a provider.
package llmtest

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/latebit-io/nib/ai/llm"
)

// Scripted is a provider that serves one prepared stream per Stream call,
// in order. A call past the last one gets a stream that closes at once,
// which a consumer sees as a provider that hung up early.
type Scripted struct {
	mu       sync.Mutex
	turns    [][]llm.StreamEvent
	requests []Request
}

// Request is what one Stream call was given.
type Request struct {
	Messages []llm.Message
	Tools    []llm.ToolDef
}

// NewScripted returns a [Scripted] that will serve turns.
func NewScripted(turns ...[]llm.StreamEvent) *Scripted {
	return &Scripted{turns: turns}
}

// Stream serves the next prepared stream, fully buffered and closed.
func (p *Scripted) Stream(_ context.Context, messages []llm.Message, tools []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	p.mu.Lock()
	// Copied: the caller goes on appending to its transcript.
	p.requests = append(p.requests, Request{Messages: slices.Clone(messages), Tools: slices.Clone(tools)})
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
	return len(p.requests)
}

// Requests returns what each Stream call so far was given, in order.
func (p *Scripted) Requests() []Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.requests)
}

// Offered returns the number of tools each Stream call was given.
func (p *Scripted) Offered() []int {
	offered := []int{}
	for _, request := range p.Requests() {
		offered = append(offered, len(request.Tools))
	}
	return offered
}

// Failing returns a provider whose Stream fails with err.
func Failing(err error) llm.Provider { return failing{err} }

type failing struct{ err error }

// Stream implements [llm.Provider]: it fails at once.
func (p failing) Stream(context.Context, []llm.Message, []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	return nil, p.err
}

// FailingFirst returns a provider whose first n Stream calls fail with
// err; every later call goes to then.
func FailingFirst(n int, err error, then llm.Provider) llm.Provider {
	p := &failingFirst{err: err, then: then}
	p.left.Store(int64(n))
	return p
}

type failingFirst struct {
	left atomic.Int64
	err  error
	then llm.Provider
}

// Stream implements [llm.Provider]: it fails until its count runs out.
func (p *failingFirst) Stream(ctx context.Context, messages []llm.Message, tools []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	if p.left.Add(-1) >= 0 {
		return nil, p.err
	}
	return p.then.Stream(ctx, messages, tools)
}

// Blocking returns a provider that streams tokens and then never finishes
// the turn: the stream closes, without a Done event, when ctx ends.
func Blocking(tokens ...string) llm.Provider { return blocking(tokens) }

type blocking []string

// Stream implements [llm.Provider]: it sends its tokens, then waits for ctx.
func (p blocking) Stream(ctx context.Context, _ []llm.Message, _ []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent, len(p))
	for _, tok := range p {
		ch <- llm.StreamEvent{Token: tok}
	}
	// AfterFunc costs no goroutine while ctx lives, so nothing leaks.
	context.AfterFunc(ctx, func() { close(ch) })
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
