package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/event"
)

func TestFoundationTurnCheck(t *testing.T) {
	t.Run("disabled cap never aborts", func(t *testing.T) {
		a := &Agent{maxTurns: 0}
		for i := 0; i < 50; i++ {
			if err := a.foundationTurnCheck(); err != nil {
				t.Fatalf("turn %d aborted with cap disabled: %v", i+1, err)
			}
		}
	})

	t.Run("proceeds up to the cap, aborts after", func(t *testing.T) {
		a := &Agent{maxTurns: 3}
		for i := 1; i <= 3; i++ {
			if err := a.foundationTurnCheck(); err != nil {
				t.Fatalf("turn %d should proceed under cap 3: %v", i, err)
			}
		}
		err := a.foundationTurnCheck() // turn 4
		if !errors.Is(err, errMaxTurnsExceeded) {
			t.Fatalf("turn 4 = %v, want errMaxTurnsExceeded", err)
		}
		if !strings.Contains(err.Error(), "3") {
			t.Errorf("abort message should name the cap: %q", err.Error())
		}
		// A re-entrant over-limit call aborts with the bare sentinel (no
		// duplicate user-facing message).
		if err := a.foundationTurnCheck(); !errors.Is(err, errMaxTurnsExceeded) {
			t.Fatalf("turn 5 = %v, want errMaxTurnsExceeded", err)
		}
	})

	t.Run("increments the turn counter", func(t *testing.T) {
		a := &Agent{maxTurns: 0}
		_ = a.foundationTurnCheck()
		_ = a.foundationTurnCheck()
		if a.turnCounter != 2 {
			t.Fatalf("turnCounter = %d, want 2", a.turnCounter)
		}
	})
}

// toolCallTurn is a scripted provider turn that calls a nonexistent tool,
// forcing the run loop to dispatch (get an error reply) and stream again —
// so the turn cap has multiple turns to count.
func toolCallTurn(id string) []llm.StreamEvent {
	return []llm.StreamEvent{{
		ToolCalls: []llm.ToolCall{{
			ID:       id,
			Type:     "function",
			Function: llm.FunctionCall{Name: "no_such_tool", Arguments: "{}"},
		}},
		Done: true,
	}}
}

// TestAgent_MaxTurns_AbortsRun drives a run that would loop forever
// (each turn calls a tool) under MaxTurns=2, and asserts the run aborts
// on the 3rd turn's pre-Stream check: exactly 2 provider calls, then an
// AgentError naming the cap and AgentDone{Success:false}.
func TestAgent_MaxTurns_AbortsRun(t *testing.T) {
	t.Parallel()
	provider := &multiTurnProvider{
		turns: [][]llm.StreamEvent{
			toolCallTurn("call-1"),
			toolCallTurn("call-2"),
			// A 3rd turn must never run — the cap aborts before it.
			{{Token: "should not see"}, {Done: true}},
		},
	}

	ag := New(provider, stubWorkspace{}, &NewOptions{MaxTurns: 2})
	events := subscribeForTest(t, ag)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ag.RunWithMode(ctx, "main.go", "", "go", event.ModeExecution)

	errMsg, doneSuccess := collectAbortEvents(t, events, 2*time.Second)
	if !strings.Contains(errMsg, "max turns") || !strings.Contains(errMsg, "2") {
		t.Errorf("AgentError = %q, want a max-turns message naming the cap", errMsg)
	}
	if doneSuccess {
		t.Error("AgentDone.Success = true, want false on max-turns abort")
	}

	provider.mu.Lock()
	calls := provider.call
	provider.mu.Unlock()
	if calls != 2 {
		t.Errorf("provider Stream calls = %d, want 2 (abort before the 3rd turn)", calls)
	}
}

func TestNew_MaxTurns_DisabledByDefault(t *testing.T) {
	ag := New(&multiTurnProvider{}, stubWorkspace{}, nil)
	t.Cleanup(ag.Close)
	if ag.maxTurns != 0 {
		t.Fatalf("maxTurns = %d, want 0 (disabled by default)", ag.maxTurns)
	}
}
