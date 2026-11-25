package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/latebit-io/nib/agent/event"
	"github.com/latebit-io/nib/ai/llm"
)

// TestSend_SlowConsumerLosesNoControlEvent: with no buffer and a reader
// that lags, every control event must still arrive, in order.
func TestSend_SlowConsumerLosesNoControlEvent(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)                // frees the run if the test fails mid-stream
	events := make(chan event.Event) // unbuffered: every send meets the reader
	a, err := New(Options{
		Provider: newScriptedProvider(streamWithToolCalls([]llm.ToolCall{echoCall}), streamText([]string{"done"}, nil)),
		Events:   events,
		Tools:    []Tool{namedTool{name: "echo"}},
		OneShot:  true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Prompt(ctx, "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	var got []string
	deadline := time.After(5 * time.Second)
	for ended := false; !ended; {
		time.Sleep(5 * time.Millisecond) // the lag
		select {
		case ev := <-events:
			switch ev.(type) {
			case event.MessageUpdate, event.TurnUsage: // droppable by contract
			default:
				got = append(got, fmt.Sprintf("%T", ev))
			}
			_, ended = ev.(event.AgentEnd)
		case <-deadline:
			t.Fatalf("no AgentEnd before the deadline; got %v", got)
		}
	}

	turn := []string{"event.TurnStart", "event.MessageStart", "event.MessageEnd", "event.TurnEnd"}
	want := slices.Concat(
		[]string{"event.AgentStart"}, turn,
		[]string{"event.ToolStart", "event.ToolEnd"}, turn,
		[]string{"event.AgentEnd"},
	)
	if !slices.Equal(got, want) {
		t.Errorf("control events = %v\nwant %v", got, want)
	}
}

// TestSend_ClosedAgentAndAgentEnd runs under a fake clock. Without Close
// AgentEnd waits for its reader; after Close the reader has endGrace left.
func TestSend_ClosedAgentAndAgentEnd(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		readAfter time.Duration // the reader takes AgentEnd this long after Close
		wantEnd   bool
	}{
		{name: "reader within the grace gets AgentEnd", readAfter: endGrace / 2, wantEnd: true},
		{name: "reader gone: the run lets go", readAfter: 2 * endGrace},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				events := make(chan event.Event) // nobody reads until readAfter
				a, err := New(Options{Provider: hangingProvider{}, Events: events})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				ran := make(chan runResult, 1)
				go func() {
					end, err := a.Run(ctx, question)
					ran <- runResult{end: end, err: err}
				}()
				synctest.Wait() // the run is stalled on its first event

				cancel()
				a.Close()
				time.Sleep(tc.readAfter)
				gotEnd := false
				select {
				case ev := <-events:
					_, gotEnd = ev.(event.AgentEnd)
				default:
				}

				res := <-ran
				if !errors.Is(res.err, context.Canceled) || res.end.Reason != event.EndAborted {
					t.Errorf("Run = %q, %v; want aborted, Canceled", res.end.Reason, res.err)
				}
				if gotEnd != tc.wantEnd {
					t.Errorf("reader got AgentEnd = %v; want %v", gotEnd, tc.wantEnd)
				}
			})
		})
	}
}
