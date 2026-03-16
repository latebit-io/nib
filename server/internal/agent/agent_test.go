package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/latebit-io/junto/server/internal/llm"
)

// mockProvider supports multi-turn conversations.
// Each call to Stream returns the next turn's events.
type mockProvider struct {
	mu    sync.Mutex
	turns [][]llm.StreamEvent // one slice of events per turn
	call  int
}

func (m *mockProvider) Stream(_ context.Context, _ []llm.Message) (<-chan llm.StreamEvent, error) {
	m.mu.Lock()
	idx := m.call
	m.call++
	m.mu.Unlock()

	var events []llm.StreamEvent
	if idx < len(m.turns) {
		events = m.turns[idx]
	} else {
		// Default: just done with no tool calls (stops the loop)
		events = []llm.StreamEvent{{Done: true}}
	}

	ch := make(chan llm.StreamEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

// mockSender collects sent messages.
type mockSender struct {
	mu   sync.Mutex
	msgs []any
}

func (s *mockSender) Send(msg any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, msg)
	return nil
}

func TestAgentRunReasoningOnly(t *testing.T) {
	provider := &mockProvider{
		turns: [][]llm.StreamEvent{
			// Turn 1: just reasoning, no tool calls
			{
				{Token: "Hello "},
				{Token: "world\n"},
				{Done: true},
			},
		},
	}
	sender := &mockSender{}
	sess := NewSession()

	a := &Agent{
		Provider: provider,
		Client:   sender,
		Session:  sess,
	}

	a.Run(context.Background(), "test.go", "package main\n", "review this")

	sender.mu.Lock()
	count := len(sender.msgs)
	sender.mu.Unlock()

	// "Agent thinking...\n\n" + "Hello " + "world\n" + "\n--- Plan complete ---\n"
	if count < 3 {
		t.Fatalf("expected at least 3 messages, got %d", count)
	}
}

func TestAgentRunWithToolCall(t *testing.T) {
	provider := &mockProvider{
		turns: [][]llm.StreamEvent{
			// Turn 1: reasoning + tool call
			{
				{Token: "Let me add a function.\n"},
				{Done: true, ToolCalls: []llm.ToolCall{
					{
						ID:   "call_1",
						Type: "function",
						Function: llm.FunctionCall{
							Name:      "edit_file",
							Arguments: `{"search":"package main\n","replace":"package main\n\nfunc foo() {}\n","reason":"add foo"}`,
						},
					},
				}},
			},
			// Turn 2: LLM sees tool result, responds with text only (done)
			{
				{Token: "Done!\n"},
				{Done: true},
			},
		},
	}
	sender := &mockSender{}
	sess := NewSession()

	a := &Agent{
		Provider: provider,
		Client:   sender,
		Session:  sess,
	}

	// Approve the op in background
	go func() {
		sess.Mu.Lock()
		sess.Rejected = false
		sess.Mu.Unlock()
		sess.Advance <- struct{}{}
		sess.Proceed <- struct{}{}
	}()

	a.Run(context.Background(), "test.go", "package main\n", "add a function")

	sess.Mu.Lock()
	opID := sess.CurrentOpID
	sess.Mu.Unlock()

	if opID != "step-1" {
		t.Fatalf("expected current op step-1, got %q", opID)
	}

	// Verify provider was called twice (two turns)
	provider.mu.Lock()
	calls := provider.call
	provider.mu.Unlock()
	if calls != 2 {
		t.Fatalf("expected 2 provider calls (multi-turn), got %d", calls)
	}
}

func TestAgentRunRejection(t *testing.T) {
	provider := &mockProvider{
		turns: [][]llm.StreamEvent{
			// Turn 1: tool call
			{
				{Token: "Adding code.\n"},
				{Done: true, ToolCalls: []llm.ToolCall{
					{
						ID:   "call_1",
						Type: "function",
						Function: llm.FunctionCall{
							Name:      "edit_file",
							Arguments: `{"search":"package main","replace":"package main\nx","reason":"test"}`,
						},
					},
				}},
			},
			// Turn 2: LLM sees rejection, responds with text only
			{
				{Token: "OK, moving on.\n"},
				{Done: true},
			},
		},
	}
	sender := &mockSender{}
	sess := NewSession()

	a := &Agent{
		Provider: provider,
		Client:   sender,
		Session:  sess,
	}

	// Reject the op
	go func() {
		sess.Mu.Lock()
		sess.Rejected = true
		sess.Mu.Unlock()
		sess.Advance <- struct{}{}
	}()

	a.Run(context.Background(), "test.go", "package main\n", "test rejection")

	// Should complete without hanging (no proceed wait after rejection)
	// And should have made 2 provider calls
	provider.mu.Lock()
	calls := provider.call
	provider.mu.Unlock()
	if calls != 2 {
		t.Fatalf("expected 2 provider calls, got %d", calls)
	}
}
