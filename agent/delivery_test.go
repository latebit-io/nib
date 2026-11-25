package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
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

// TestSend_CloseReleasesRunNobodyReads: with no reader the run stalls on
// its first event, then on AgentEnd once its ctx expires. Closing the
// agent is what gives up on that consumer and lets Run return.
func TestSend_CloseReleasesRunNobodyReads(t *testing.T) {
	t.Parallel()

	a, err := New(Options{Provider: hangingProvider{}, Events: make(chan event.Event)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	go func() {
		<-ctx.Done()
		a.Close()
	}()

	end, err := runBounded(ctx, t, a, question)

	if !errors.Is(err, context.DeadlineExceeded) || end.Reason != event.EndAborted {
		t.Errorf("Run = %q, %v; want aborted, DeadlineExceeded", end.Reason, err)
	}
}
