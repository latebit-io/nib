package agent

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/latebit-io/nib/agent/event"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/ai/llmtest"
)

// TestOnMaxTurns: at the turn cap the hook can ask for one closing turn.
// That turn offers no tools and its text is the run's final text.
func TestOnMaxTurns(t *testing.T) {
	t.Parallel()

	toolTurn := llmtest.ToolCalls(echoCall)
	nudge := func(context.Context) ([]llm.Message, error) {
		return []llm.Message{{Role: "user", Content: "answer now"}}, nil
	}
	tests := []struct {
		name  string
		turns [][]llm.StreamEvent
		hook  func(context.Context) ([]llm.Message, error)
		react reaction
		want  event.EndReason
		// wantOffered is the tool count offered on each provider call.
		wantOffered []int
		wantFinal   string
		wantUsage   event.RunUsage
		wantError   bool
	}{
		{
			name:        "hook asks for a closing turn",
			turns:       [][]llm.StreamEvent{toolTurn, llmtest.Text("the answer")},
			hook:        nudge,
			want:        event.EndMaxTurns,
			wantOffered: []int{1, 0},
			wantFinal:   "the answer",
			wantUsage:   event.RunUsage{Turns: 2, ToolCalls: 1},
		},
		{
			name:        "hook returns nothing: no closing turn",
			turns:       [][]llm.StreamEvent{toolTurn},
			hook:        func(context.Context) ([]llm.Message, error) { return nil, nil },
			want:        event.EndMaxTurns,
			wantOffered: []int{1},
			wantUsage:   event.RunUsage{Turns: 1, ToolCalls: 1},
		},
		{
			// The model calls a tool it was not offered: the call counts as
			// requested and is answered, not run.
			name:        "closing turn invents a tool call",
			turns:       [][]llm.StreamEvent{toolTurn, {{Token: "partly"}, {Done: true, ToolCalls: []llm.ToolCall{echoCall}}}},
			hook:        nudge,
			want:        event.EndMaxTurns,
			wantOffered: []int{1, 0},
			wantFinal:   "partly",
			wantUsage:   event.RunUsage{Turns: 2, ToolCalls: 2},
		},
		{
			// No OnTruncated hook: the default error, and no retry at the cap.
			name:        "closing turn is truncated",
			turns:       [][]llm.StreamEvent{toolTurn, {{Token: "cut"}, {Done: true, Truncated: true}}},
			hook:        nudge,
			want:        event.EndTruncated,
			wantOffered: []int{1, 0},
			wantFinal:   "cut",
			wantUsage:   event.RunUsage{Turns: 2, ToolCalls: 1},
			wantError:   true,
		},
		{
			name:  "run cancelled while the hook decides",
			turns: [][]llm.StreamEvent{toolTurn},
			hook: func(ctx context.Context) ([]llm.Message, error) {
				<-ctx.Done()
				return nil, nil
			},
			react:       on[event.MaxTurnsReached](func(a *Agent, _ context.CancelFunc) { a.Abort() }),
			want:        event.EndAborted,
			wantOffered: []int{1},
			wantUsage:   event.RunUsage{Turns: 1, ToolCalls: 1},
		},
		{
			name:        "hook fails",
			turns:       [][]llm.StreamEvent{toolTurn},
			hook:        func(context.Context) ([]llm.Message, error) { return nil, errors.New("boom") },
			want:        event.EndError,
			wantOffered: []int{1},
			wantUsage:   event.RunUsage{Turns: 1, ToolCalls: 1},
			wantError:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			provider := llmtest.NewScripted(tc.turns...)
			end, before := runToEnd(t, Options{
				Provider: provider,
				MaxTurns: 1,
				Hooks:    Hooks{OnMaxTurns: tc.hook},
			}, question, tc.react)

			if end.Reason != tc.want {
				t.Errorf("Reason = %q; want %q", end.Reason, tc.want)
			}
			if !slices.Equal(provider.Offered(), tc.wantOffered) {
				t.Errorf("tools offered per call = %v; want %v", provider.Offered(), tc.wantOffered)
			}
			if end.FinalText != tc.wantFinal {
				t.Errorf("FinalText = %q; want %q", end.FinalText, tc.wantFinal)
			}
			if end.Usage != tc.wantUsage {
				t.Errorf("Usage = %+v; want %+v", end.Usage, tc.wantUsage)
			}
			if got := countEvents[event.Error](before) > 0; got != tc.wantError {
				t.Errorf("Error event emitted = %v; want %v", got, tc.wantError)
			}
			if n := countEvents[event.MaxTurnsReached](before); n != 1 {
				t.Errorf("MaxTurnsReached count = %d; want 1", n)
			}
			// Every tool call in the transcript must have its result.
			calls := 0
			for _, m := range end.Messages {
				calls += len(m.ToolCalls)
			}
			if results := len(toolResults(end.Messages)); calls != results {
				t.Errorf("transcript has %d tool calls and %d results", calls, results)
			}
		})
	}
}

// TestOnTruncated_Consecutive: the hook is told how many responses in a
// row were cut off. A complete response starts the count over, and the
// closing turn at the cap continues the streak the last turns left.
func TestOnTruncated_Consecutive(t *testing.T) {
	t.Parallel()

	cut := []llm.StreamEvent{{Token: "cut"}, {Done: true, Truncated: true}}
	nudge := func(context.Context) ([]llm.Message, error) {
		return []llm.Message{{Role: "user", Content: "answer now"}}, nil
	}
	tests := []struct {
		name     string
		maxTurns int
		turns    [][]llm.StreamEvent
		want     []int
	}{
		{"every response cut off, the last at the cap", 2, [][]llm.StreamEvent{cut, cut, cut}, []int{1, 2, 3}},
		{"a complete response in between", 3, [][]llm.StreamEvent{cut, llmtest.ToolCalls(echoCall), cut, cut}, []int{1, 1, 2}},
		{"the closing turn after a complete one", 1, [][]llm.StreamEvent{llmtest.ToolCalls(echoCall), cut}, []int{1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var seen []int
			retry := func(_ context.Context, in TruncationInput) (TruncationResult, error) {
				seen = append(seen, in.Consecutive)
				return TruncationResult{Retry: true}, nil
			}
			runToEnd(t, Options{
				Provider: llmtest.NewScripted(tc.turns...),
				MaxTurns: tc.maxTurns,
				Hooks:    Hooks{OnTruncated: retry, OnMaxTurns: nudge},
			}, question, nil)
			if !slices.Equal(seen, tc.want) {
				t.Errorf("Consecutive = %v; want %v", seen, tc.want)
			}
		})
	}
}
