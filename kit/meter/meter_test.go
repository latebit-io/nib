package meter

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/ai/llmtest"
)

func usage(prompt, completion int) *llm.Usage {
	return &llm.Usage{PromptTokens: prompt, CompletionTokens: completion}
}

// drain reads a stream to its end and returns the text.
func drain(t *testing.T, ch <-chan llm.StreamEvent) string {
	t.Helper()
	text := ""
	for ev := range ch {
		text += ev.Token
	}
	return text
}

// TestProvider_CountsAndStopsAtTheBudget: usage is counted by the time
// the stream ends, and the request after the budget is used is refused
// without reaching the provider.
func TestProvider_CountsAndStopsAtTheBudget(t *testing.T) {
	inner := llmtest.NewScripted(llmtest.TextUsage(usage(40, 20), "one"), llmtest.TextUsage(usage(30, 20), "two"), llmtest.Text("never"))
	m := New(100)
	m.Set(inner)
	ctx := context.Background()
	for i, want := range []string{"one", "two"} {
		ch, err := m.Stream(ctx, nil, nil)
		if err != nil {
			t.Fatalf("request %d refused under the budget: %v", i+1, err)
		}
		if got := drain(t, ch); got != want {
			t.Errorf("stream %d = %q, want %q", i+1, got, want)
		}
	}
	if u := m.Usage(); u.TotalPromptTokens != 70 || u.TotalCompletionTokens != 40 || u.Turns != 2 {
		t.Errorf("usage = %+v, want 70 prompt, 40 completion, 2 turns", u)
	}
	if _, err := m.Stream(ctx, nil, nil); !errors.Is(err, ErrBudgetExceeded) || inner.Calls() != 2 {
		t.Errorf("request past the budget: err = %v, provider calls %d; want it refused before the provider", err, inner.Calls())
	}
}

// capped is a provider with an output limit.
type capped struct {
	*llmtest.Scripted
	max int
}

func (c *capped) MaxTokens() int     { return c.max }
func (c *capped) SetMaxTokens(n int) { c.max = n }

// TestProvider_SetSwapsTheModel: the next request goes to the new
// provider, whose output limit the meter passes through.
func TestProvider_SetSwapsTheModel(t *testing.T) {
	m := New(0)
	if _, err := m.Stream(context.Background(), nil, nil); err == nil {
		t.Error("a meter with no provider must fail the request")
	}
	if m.SetMaxTokens(10); m.MaxTokens() != 0 {
		t.Error("with no provider there is no output limit to report")
	}
	first := llmtest.NewScripted(llmtest.Text("old"))
	second := &capped{Scripted: llmtest.NewScripted(llmtest.Text("new")), max: 4096}
	m.Set(first)
	if m.SetMaxTokens(10); m.MaxTokens() != 0 {
		t.Error("a provider without an output limit must report none")
	}
	m.Set(second)
	ch, err := m.Stream(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := drain(t, ch); got != "new" || first.Calls() != 0 {
		t.Errorf("stream = %q, old provider called %d times; want the new provider only", got, first.Calls())
	}
	if m.SetMaxTokens(8192); second.max != 8192 || m.MaxTokens() != 8192 {
		t.Errorf("output limit = %d, want it set on the provider in use", second.max)
	}
}

// held streams a token, then waits for release before its final event.
type held struct {
	release chan struct{}
	usage   *llm.Usage
}

func (h held) Stream(ctx context.Context, _ []llm.Message, _ []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent)
	go func() {
		defer close(ch)
		ch <- llm.StreamEvent{Token: "partial"}
		<-h.release
		// Sent after the consumer has gone, as a provider winding down does.
		ch <- llm.StreamEvent{Done: true, Usage: h.usage}
	}()
	return ch, nil
}

// TestProvider_CountsAStoppedRequest: usage a provider reports after
// the consumer stopped reading still counts, so stopping requests
// cannot get around the budget.
func TestProvider_CountsAStoppedRequest(t *testing.T) {
	inner := held{release: make(chan struct{}), usage: usage(40, 20)}
	m := New(50)
	m.Set(inner)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := m.Stream(ctx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cancel() // the consumer stops before reading anything
	close(inner.release)
	// Not reading ch leaves the meter only the cancelled path to take.
	deadline := time.Now().Add(5 * time.Second)
	for m.Usage().Total() != 60 {
		if time.Now().After(deadline) {
			t.Fatalf("usage = %+v, want the stopped request's 60 tokens counted", m.Usage())
		}
		time.Sleep(time.Millisecond)
	}
	for range ch {
	}
	if _, err := m.Stream(context.Background(), nil, nil); !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("next request: err = %v, want ErrBudgetExceeded", err)
	}
}
