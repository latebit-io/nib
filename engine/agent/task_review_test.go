package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/junto/engine/event"
)

// drainTokens reads every queued AgentToken from the channel and returns
// their concatenated text. Non-token events are ignored.
func drainTokens(ch <-chan event.Event) string {
	var b strings.Builder
	for {
		select {
		case ev := <-ch:
			if tok, ok := ev.(event.AgentToken); ok {
				b.WriteString(tok.Text)
			}
		default:
			return b.String()
		}
	}
}

func TestRunTaskReview_NoConfigurationSurfacesBanner(t *testing.T) {
	events := make(chan event.Event, 16)
	a := &Agent{events: events}
	a.turnEdits = []turnEdit{{Path: "main.lua", Search: "a", Replace: "b"}}
	// styleLintCmd nil and evaluator nil — nothing configured.

	msg := a.runTaskReview(context.Background(), "Task completed: X")
	if !strings.Contains(msg, "Task completed: X") {
		t.Errorf("tool message should pass through, got: %s", msg)
	}

	tokens := drainTokens(events)
	if !strings.Contains(tokens, "no lint or style evaluator configured") {
		t.Errorf("expected disambiguating banner, got: %q", tokens)
	}
}

func TestRunTaskReview_NoEditsNoBanner(t *testing.T) {
	// Task completed without any edits (e.g. read-only analysis task) — no
	// review banner should fire; an empty banner would confuse the agent.
	events := make(chan event.Event, 16)
	a := &Agent{events: events}
	// turnEdits is nil / empty.

	_ = a.runTaskReview(context.Background(), "Done")
	tokens := drainTokens(events)
	if strings.Contains(tokens, "no lint or style evaluator configured") {
		t.Errorf("should not emit banner without edits, got: %q", tokens)
	}
	if strings.Contains(tokens, "Task complete — running style lint") {
		t.Errorf("should not claim to run lint without edits, got: %q", tokens)
	}
}
