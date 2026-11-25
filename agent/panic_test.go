package agent

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/latebit-io/nib/agent/event"
	"github.com/latebit-io/nib/ai/llm"
)

// panicProvider panics when asked for a turn.
type panicProvider struct{}

func (panicProvider) Stream(context.Context, []llm.Message, []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	panic("provider bug")
}

// TestPanic_FailsTheRunNotTheHost: a panic in a hook or the provider ends
// the run with an error, and every tool call still has its result.
func TestPanic_FailsTheRunNotTheHost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		opts     Options
		wantOpen int // tool calls the run left for settle to answer
	}{
		{
			name: "BeforeToolCall panics mid-batch",
			opts: Options{
				Provider: newScriptedProvider(streamWithToolCalls(callsOf("echo", 2))),
				Hooks: Hooks{BeforeToolCall: func(context.Context, BeforeToolCallInput) (BeforeToolCallResult, error) {
					panic("hook bug")
				}},
			},
			wantOpen: 2,
		},
		{
			name: "AfterToolCall panics mid-batch",
			opts: Options{
				Provider: newScriptedProvider(streamWithToolCalls(callsOf("echo", 2))),
				Hooks: Hooks{AfterToolCall: func(context.Context, AfterToolCallInput) (AfterToolCallResult, error) {
					panic("hook bug")
				}},
			},
			wantOpen: 2,
		},
		{
			name: "provider panics",
			opts: Options{Provider: panicProvider{}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			end, before := runToEnd(t, tc.opts, question, nil)

			if end.Reason != event.EndError {
				t.Errorf("Reason = %q; want error", end.Reason)
			}
			if errEv, ok := findEvent[event.Error](before); !ok || !strings.Contains(errEv.Err, "panic") {
				t.Errorf("Error event = %q (found %v); want one naming the panic", errEv.Err, ok)
			}
			want := slices.Repeat([]string{unfinishedToolResult}, tc.wantOpen)
			got := toolResults(end.Messages)
			for i := range got {
				_, got[i], _ = strings.Cut(got[i], "=")
			}
			if !slices.Equal(got, want) {
				t.Errorf("tool results = %v; want %v", got, want)
			}
		})
	}
}

// TestPanic_ToolResultAndReuse: a panicking tool is an error result and
// the run goes on; the agent takes a new run after a run that panicked.
func TestPanic_ToolResultAndReuse(t *testing.T) {
	t.Parallel()

	provider := newScriptedProvider(
		streamWithToolCalls([]llm.ToolCall{echoCall}),
		streamDone(),
		streamDone(),
	)
	hooks := Hooks{TransformContext: func(_ context.Context, msgs []llm.Message) ([]llm.Message, error) {
		if msgs[0].Content == "second" {
			panic("hook bug")
		}
		return nil, nil
	}}
	a, err := New(Options{Provider: provider, Tools: []Tool{funcTool{namedTool{name: "echo"}, boom}}, Hooks: hooks})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	first, err := runBounded(context.Background(), t, a, question)
	if err != nil || first.Reason != event.EndCompleted {
		t.Fatalf("first Run = %q, %v; want completed, nil", first.Reason, err)
	}
	if got, want := toolResults(first.Messages), []string{"c1=Error: agent: panic in tool echo: tool bug"}; !slices.Equal(got, want) {
		t.Errorf("tool results = %v; want %v", got, want)
	}

	if _, err := runBounded(context.Background(), t, a, []llm.Message{{Role: "user", Content: "second"}}); err == nil || !strings.Contains(err.Error(), "panic in run: hook bug") {
		t.Errorf("second Run error = %v; want the recovered panic", err)
	}
	if end, err := runBounded(context.Background(), t, a, []llm.Message{{Role: "user", Content: "third"}}); err != nil || end.Reason != event.EndCompleted {
		t.Errorf("third Run = %q, %v; want completed, nil", end.Reason, err)
	}
}
