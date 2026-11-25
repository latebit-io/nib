package agent

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/latebit-io/nib/agent/event"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/ai/llmtest"
)

// handoverRuns is how many back-to-back runs the handover tests drive.
const handoverRuns = 300

// answers is one text turn per run.
func answers(n int) [][]llm.StreamEvent {
	return slices.Repeat([][]llm.StreamEvent{llmtest.Text("ok")}, n)
}

// TestAgentEnd_NextRunCanStartAtOnce: a consumer that answers AgentEnd
// with the next prompt must never be told a run is still in progress.
func TestAgentEnd_NextRunCanStartAtOnce(t *testing.T) {
	t.Parallel()

	events := make(chan event.Event, 64)
	a, err := New(Options{Provider: llmtest.NewScripted(answers(handoverRuns)...), Events: events, OneShot: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.PromptWithMessages(context.Background(), question); err != nil {
		t.Fatalf("PromptWithMessages: %v", err)
	}

	deadline := time.After(10 * time.Second)
	for ended := 0; ended < handoverRuns; {
		select {
		case ev := <-events:
			if _, ok := ev.(event.AgentEnd); !ok {
				continue
			}
			if ended++; ended == handoverRuns {
				continue
			}
			if err := a.PromptWithMessages(context.Background(), question); err != nil {
				t.Fatalf("PromptWithMessages right after AgentEnd %d: %v", ended, err)
			}
		case <-deadline:
			t.Fatalf("stalled after %d runs", ended)
		}
	}
}

// TestAgentEnd_RunsDoNotInterleave: the next run is started the instant
// the agent admits one, which is before the last AgentEnd is delivered.
// Its events must still come after that AgentEnd.
func TestAgentEnd_RunsDoNotInterleave(t *testing.T) {
	t.Parallel()

	events := make(chan event.Event, 64)
	a, err := New(Options{Provider: llmtest.NewScripted(answers(handoverRuns)...), Events: events, OneShot: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	go func() {
		for range handoverRuns {
			for errors.Is(a.PromptWithMessages(context.Background(), question), ErrRunInProgress) {
				runtime.Gosched()
			}
		}
	}()

	open := false
	deadline := time.After(10 * time.Second)
	for ended := 0; ended < handoverRuns; {
		select {
		case ev := <-events:
			switch ev.(type) {
			case event.AgentStart:
				if open {
					t.Fatalf("run %d started before run %d ended", ended+1, ended)
				}
				open = true
			case event.AgentEnd:
				open = false
				ended++
			default:
				if !open {
					t.Fatalf("%T arrived between runs, after AgentEnd %d", ev, ended)
				}
			}
		case <-deadline:
			t.Fatalf("stalled after %d runs", ended)
		}
	}
}

// TestWaitForIdle_WaitsForAgentEndDelivery: the agent admits a run before
// AgentEnd is delivered, but WaitForIdle still means the last run is all
// the way done, AgentEnd included.
func TestWaitForIdle_WaitsForAgentEndDelivery(t *testing.T) {
	t.Parallel()

	events := make(chan event.Event) // unbuffered: nothing moves unless read
	a, err := New(Options{Provider: llmtest.NewScripted(llmtest.Text("ok")), Events: events, OneShot: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.PromptWithMessages(context.Background(), question); err != nil {
		t.Fatalf("PromptWithMessages: %v", err)
	}
	idle := make(chan struct{})
	go func() {
		a.WaitForIdle()
		close(idle)
	}()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-events:
			if _, ok := ev.(event.AgentEnd); ok {
				select {
				case <-idle:
				case <-deadline:
					t.Fatal("WaitForIdle did not return after AgentEnd was read")
				}
				return
			}
		case <-idle:
			t.Fatal("WaitForIdle returned before AgentEnd was delivered")
		case <-deadline:
			t.Fatal("no AgentEnd")
		}
	}
}
