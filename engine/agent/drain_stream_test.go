package agent

import (
	"context"
	"testing"

	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/llm"
)

// TestDrainStream_ResetsThinkStateOnReturn locks in that drainStream never
// leaks <think>...</think> state across stream boundaries. An unclosed
// <think> block at stream end used to keep inThink=true, causing the next
// turn's stripThinkTags to silently drop real output until a stray closing
// tag (which may never arrive) was seen.
func TestDrainStream_ResetsThinkStateOnReturn(t *testing.T) {
	tests := []struct {
		name   string
		events []llm.StreamEvent
		close  bool // if true, close channel without a Done event (EOF path)
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
			close: true,
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
			_, _ = ag.drainStream(context.Background(), ch, &thinkState)
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
	_, _ = ag.drainStream(ctx, ch, &thinkState)
	if thinkState {
		t.Error("thinkState should be false after drainStream returns on ctx cancel")
	}
}
