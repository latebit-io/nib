package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/latebit-io/junto/ai/llm"
	"github.com/latebit-io/junto/engine/event"
)

// TestDrainStream_ResetsThinkStateOnReturn locks in that drainStream never
// leaks <think>...</think> state across stream boundaries. An unclosed
// <think> block at stream end used to keep inThink=true, causing the next
// turn's stripThinkTags to silently drop real output until a stray closing
// tag (which may never arrive) was seen.
func TestDrainStream_ResetsThinkStateOnReturn(t *testing.T) {
	tests := []struct {
		name    string
		events  []llm.StreamEvent
		wantErr error // nil for Done-event paths, errStreamClosedEarly for EOF
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
			wantErr: errStreamClosedEarly,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			evts := make(chan event.Event, 64)
			ag := &Agent{events: evts}

			ch := make(chan llm.StreamEvent, len(tc.events)+1)
			for _, e := range tc.events {
				ch <- e
			}
			close(ch) // safe for both paths — Done-event tests still read their event first

			thinkState := true // start dirty to prove defer reset fires
			_, err := ag.drainStream(context.Background(), ch, &thinkState)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("drainStream error = %v, want %v", err, tc.wantErr)
			}
			if thinkState {
				t.Error("thinkState should be false after drainStream returns")
			}
		})
	}
}

// TestDrainStream_ResetsThinkStateOnCtxCancel covers the remaining return
// path — ctx cancellation mid-think.
func TestDrainStream_ResetsThinkStateOnCtxCancel(t *testing.T) {
	evts := make(chan event.Event, 16)
	ag := &Agent{events: evts}

	ch := make(chan llm.StreamEvent, 1)
	ch <- llm.StreamEvent{Token: "<think>opening"}
	// Do not close — force drainStream to block until ctx cancels.

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately so the select races to ctx.Done

	thinkState := true
	// ctx cancellation has its own handling path in processLLMTurn (via
	// ctx.Err() check), so drainStream returns nil on this path — see
	// the function's doc comment.
	_, err := ag.drainStream(ctx, ch, &thinkState)
	if err != nil {
		t.Errorf("drainStream error on ctx cancel = %v, want nil", err)
	}
	if thinkState {
		t.Error("thinkState should be false after drainStream returns on ctx cancel")
	}
}
