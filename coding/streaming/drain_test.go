package streaming

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/latebit-io/junto/ai/llm"
	"github.com/latebit-io/junto/engine/event"
)

// TestDrain_ResetsThinkStateOnReturn locks in that Drain never leaks
// <think>...</think> state across stream boundaries. An unclosed
// <think> block at stream end used to keep inThink=true, causing the
// next turn's stripThinkTags to silently drop real output until a
// stray closing tag (which may never arrive) was seen.
func TestDrain_ResetsThinkStateOnReturn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		events  []llm.StreamEvent
		wantErr error // nil for Done-event paths, ErrClosedEarly for EOF
	}{
		{
			name: "clean think block closes state",
			events: []llm.StreamEvent{
				{Token: "<think>reasoning</think>answer"},
				{Done: true},
			},
		},
		{
			name: "unclosed think block at Done",
			events: []llm.StreamEvent{
				{Token: "<think>reasoning without close"},
				{Done: true, Truncated: true},
			},
		},
		{
			name: "channel closed mid-think (EOF path)",
			events: []llm.StreamEvent{
				{Token: "<think>incomplete"},
			},
			wantErr: ErrClosedEarly,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sent := make([]event.Event, 0, 4)
			send := func(ev event.Event) { sent = append(sent, ev) }

			ch := make(chan llm.StreamEvent, len(tc.events)+1)
			for _, e := range tc.events {
				ch <- e
			}
			close(ch) // safe for both paths — Done-event tests still read their event first

			thinkState := true // start dirty to prove defer reset fires
			_, err := Drain(context.Background(), ch, &thinkState, send)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Drain error = %v, want %v", err, tc.wantErr)
			}
			if thinkState {
				t.Error("thinkState should be false after Drain returns")
			}
		})
	}
}

// TestDrain_ResetsThinkStateOnCtxCancel covers the remaining return
// path — ctx cancellation mid-think.
func TestDrain_ResetsThinkStateOnCtxCancel(t *testing.T) {
	t.Parallel()
	send := func(event.Event) {}

	ch := make(chan llm.StreamEvent, 1)
	ch <- llm.StreamEvent{Token: "<think>opening"}
	// Do not close — force Drain to block until ctx cancels.

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately so the select races to ctx.Done

	thinkState := true
	// ctx cancellation has its own handling path in processLLMTurn
	// (via ctx.Err() check), so Drain returns nil on this path —
	// see the function's doc comment.
	_, err := Drain(ctx, ch, &thinkState, send)
	if err != nil {
		t.Errorf("Drain error on ctx cancel = %v, want nil", err)
	}
	if thinkState {
		t.Error("thinkState should be false after Drain returns on ctx cancel")
	}
}

// TestDrain_ForwardsTokensAndCollectsTerminalState verifies the
// happy-path: text tokens stream out via the Sender, the terminal
// Done event populates the Result fields, and Content reflects the
// stripped concatenation.
func TestDrain_ForwardsTokensAndCollectsTerminalState(t *testing.T) {
	t.Parallel()
	ch := make(chan llm.StreamEvent, 4)
	ch <- llm.StreamEvent{Token: "Hello, "}
	ch <- llm.StreamEvent{Token: "<think>plotting</think>"}
	ch <- llm.StreamEvent{Token: "world."}
	ch <- llm.StreamEvent{
		Done:      true,
		Truncated: false,
		ToolCalls: []llm.ToolCall{{ID: "call-1"}},
		Usage:     &llm.Usage{PromptTokens: 10, CompletionTokens: 5},
	}
	close(ch)

	var tokens []string
	send := func(ev event.Event) {
		if tok, ok := ev.(event.AgentToken); ok {
			tokens = append(tokens, tok.Text)
		}
	}

	thinkState := false
	res, err := Drain(context.Background(), ch, &thinkState, send)
	if err != nil {
		t.Fatalf("Drain err = %v, want nil", err)
	}
	if res.Content != "Hello, world." {
		t.Errorf("Result.Content = %q, want %q", res.Content, "Hello, world.")
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].ID != "call-1" {
		t.Errorf("Result.ToolCalls = %+v, want one call with ID call-1", res.ToolCalls)
	}
	if res.Usage == nil || res.Usage.PromptTokens != 10 {
		t.Errorf("Result.Usage = %+v, want PromptTokens=10", res.Usage)
	}
	// Tokens forwarded to Sender should be the cleaned (think-stripped)
	// text — never the raw <think>...</think> bytes.
	for _, tok := range tokens {
		if strings.Contains(tok, "<think>") || strings.Contains(tok, "</think>") {
			t.Errorf("Sender saw raw think-tag bytes: %q", tok)
		}
	}
}
