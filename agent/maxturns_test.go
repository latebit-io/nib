package agent

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/latebit-io/nib/agent/event"
	"github.com/latebit-io/nib/ai/llm"
)

// TestOnMaxTurns: at the turn cap the hook can ask for one closing turn.
// That turn offers no tools and its text is the run's final text.
func TestOnMaxTurns(t *testing.T) {
	t.Parallel()

	toolTurn := streamWithToolCalls([]llm.ToolCall{echoCall})
	nudge := func(context.Context) ([]llm.Message, error) {
		return []llm.Message{{Role: "user", Content: "answer now"}}, nil
	}
	tests := []struct {
		name  string
		turns [][]llm.StreamEvent
		hook  func(context.Context) ([]llm.Message, error)
		want  event.EndReason
		// wantOffered is the tool count offered on each provider call.
		wantOffered []int
		wantFinal   string
		wantUsage   event.RunUsage
		wantError   bool
	}{
		{
			name:        "hook asks for a closing turn",
			turns:       [][]llm.StreamEvent{toolTurn, streamText([]string{"the answer"}, nil)},
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
			// The model calls a tool it was not offered: it is answered, not run.
			name:        "closing turn invents a tool call",
			turns:       [][]llm.StreamEvent{toolTurn, {{Token: "partly"}, {Done: true, ToolCalls: []llm.ToolCall{echoCall}}}},
			hook:        nudge,
			want:        event.EndMaxTurns,
			wantOffered: []int{1, 0},
			wantFinal:   "partly",
			wantUsage:   event.RunUsage{Turns: 2, ToolCalls: 1},
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

			provider := newScriptedProvider(tc.turns...)
			end, before := runToEnd(t, Options{
				Provider: provider,
				MaxTurns: 1,
				Hooks:    Hooks{OnMaxTurns: tc.hook},
			}, question, nil)

			if end.Reason != tc.want {
				t.Errorf("Reason = %q; want %q", end.Reason, tc.want)
			}
			if !slices.Equal(provider.offered, tc.wantOffered) {
				t.Errorf("tools offered per call = %v; want %v", provider.offered, tc.wantOffered)
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
