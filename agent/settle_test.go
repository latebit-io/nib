package agent

import (
	"slices"
	"testing"

	"github.com/latebit-io/nib/agent/event"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/ai/llmtest"
)

// TestSettle: every tool call of the last assistant message ends up with a
// result, placed with the results already there.
func TestSettle(t *testing.T) {
	t.Parallel()

	user := llm.Message{Role: "user", Content: "q"}
	asks := llm.Message{Role: "assistant", ToolCalls: callsOf("echo", 3)}
	result := func(id string) llm.Message { return llm.Message{Role: "tool", ToolCallID: id, Content: "ok"} }
	open := func(id string) llm.Message {
		return llm.Message{Role: "tool", ToolCallID: id, Content: unfinishedToolResult}
	}

	tests := []struct {
		name string
		msgs []llm.Message
		want []llm.Message
	}{
		{name: "empty transcript"},
		{name: "no tool calls", msgs: []llm.Message{user, {Role: "assistant", Content: "a"}}, want: []llm.Message{user, {Role: "assistant", Content: "a"}}},
		{
			name: "all answered",
			msgs: []llm.Message{user, asks, result("c0"), result("c1"), result("c2")},
			want: []llm.Message{user, asks, result("c0"), result("c1"), result("c2")},
		},
		{
			name: "none answered",
			msgs: []llm.Message{user, asks},
			want: []llm.Message{user, asks, open("c0"), open("c1"), open("c2")},
		},
		{
			name: "batch cut short",
			msgs: []llm.Message{user, asks, result("c0")},
			want: []llm.Message{user, asks, result("c0"), open("c1"), open("c2")},
		},
		{
			name: "results go before what a hook appended later",
			msgs: []llm.Message{user, asks, result("c0"), user},
			want: []llm.Message{user, asks, result("c0"), open("c1"), open("c2"), user},
		},
	}
	equal := func(a, b llm.Message) bool {
		return a.Role == b.Role && a.Content == b.Content && a.ToolCallID == b.ToolCallID && len(a.ToolCalls) == len(b.ToolCalls)
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &Agent{messages: slices.Clone(tc.msgs)}
			a.settle()
			if !slices.EqualFunc(a.messages, tc.want, equal) {
				t.Errorf("settled transcript =\n%+v\nwant\n%+v", a.messages, tc.want)
			}
		})
	}
}

// TestSettle_TruncatedTurnWithToolCalls: a truncated turn nobody recovers
// ends the run with its tool calls on the transcript; they must be answered.
func TestSettle_TruncatedTurnWithToolCalls(t *testing.T) {
	t.Parallel()

	truncated := []llm.StreamEvent{{Done: true, Truncated: true, ToolCalls: callsOf("echo", 2)}}
	end, _ := runToEnd(t, Options{Provider: llmtest.NewScripted(truncated)}, question, nil)

	if end.Reason != event.EndTruncated {
		t.Errorf("Reason = %q; want truncated", end.Reason)
	}
	want := []string{"c0=" + unfinishedToolResult, "c1=" + unfinishedToolResult}
	if got := toolResults(end.Messages); !slices.Equal(got, want) {
		t.Errorf("tool results = %v; want %v", got, want)
	}
}
