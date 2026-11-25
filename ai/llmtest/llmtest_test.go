package llmtest

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/latebit-io/nib/ai/llm"
)

func TestScripted_ServesTurnsInOrderThenHangsUp(t *testing.T) {
	t.Parallel()

	usage := &llm.Usage{PromptTokens: 3}
	p := NewScripted(ToolCalls(Call("c1", "echo", "{}")), TextUsage(usage, "a", "b"))
	tools := []llm.ToolDef{{}, {}}
	transcript := []llm.Message{{Role: "user", Content: "first"}}

	var texts [][]string
	for range 3 {
		ch, err := p.Stream(context.Background(), transcript, tools)
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		// The caller reuses its slice; what was recorded must not follow.
		transcript[0].Content = "overwritten"
		transcript = append(transcript, llm.Message{Role: "assistant", Content: "reply"})
		var text []string
		for ev := range ch {
			switch {
			case ev.Token != "":
				text = append(text, ev.Token)
			case len(ev.ToolCalls) > 0:
				text = append(text, "call "+ev.ToolCalls[0].ID)
			case ev.Done:
				text = append(text, "done")
			}
		}
		texts = append(texts, text)
		tools = tools[:1] // a different count on every call after the first
	}

	want := [][]string{{"call c1"}, {"a", "b", "done"}, nil}
	if !slices.EqualFunc(texts, want, slices.Equal[[]string]) {
		t.Errorf("streams = %v; want %v", texts, want)
	}
	if p.Calls() != 3 || !slices.Equal(p.Offered(), []int{2, 1, 1}) {
		t.Errorf("Calls = %d, Offered = %v; want 3, [2 1 1]", p.Calls(), p.Offered())
	}
	var sizes []int
	for _, request := range p.Requests() {
		sizes = append(sizes, len(request.Messages))
	}
	if first := p.Requests()[0].Messages[0].Content; first != "first" || !slices.Equal(sizes, []int{1, 2, 3}) {
		t.Errorf("first message = %q, transcript sizes = %v; want first, [1 2 3]", first, sizes)
	}
}

func TestFailing_ReturnsItsError(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")
	if _, err := Failing(boom).Stream(context.Background(), nil, nil); !errors.Is(err, boom) {
		t.Errorf("Stream error = %v; want %v", err, boom)
	}
}

func TestFailingFirst_FailsThenHandsOver(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")
	p := FailingFirst(2, boom, NewScripted(Text("ok")))

	var outcomes []string
	for range 3 {
		ch, err := p.Stream(context.Background(), nil, nil)
		if err != nil {
			outcomes = append(outcomes, err.Error())
			continue
		}
		outcomes = append(outcomes, (<-ch).Token)
	}

	if want := []string{"boom", "boom", "ok"}; !slices.Equal(outcomes, want) {
		t.Errorf("calls gave %v; want %v", outcomes, want)
	}
}

func TestBlocking_StreamsTokensThenClosesWithCtx(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := Blocking("a", "b").Stream(ctx, nil, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if a, b := <-ch, <-ch; a.Token != "a" || b.Token != "b" {
		t.Errorf("tokens = %q, %q; want a, b", a.Token, b.Token)
	}
	select {
	case ev, open := <-ch:
		t.Fatalf("stream yielded %+v (open %v); want it to stay open and silent", ev, open)
	default:
	}

	cancel()

	select {
	case ev, open := <-ch:
		if open {
			t.Errorf("stream yielded %+v after cancel; want it closed", ev)
		}
	case <-time.After(5 * time.Second):
		t.Error("stream still open after cancel")
	}
}
