package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/latebit-io/junto/server/internal/llm"
)

// mockProvider streams a predefined sequence of tokens.
type mockProvider struct {
	tokens []string
}

func (m *mockProvider) Stream(_ context.Context, _ []llm.Message) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent, len(m.tokens)+1)
	for _, tok := range m.tokens {
		ch <- llm.StreamEvent{Token: tok}
	}
	ch <- llm.StreamEvent{Done: true}
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
		tokens: []string{"Hello ", "world\n"},
	}
	sender := &mockSender{}
	sess := NewSession()

	a := &Agent{
		Provider: provider,
		Client:   sender,
		Session:  sess,
	}

	a.Run(context.Background(), "test.go", "package main\n", "review this")

	// Should have: "Agent thinking...\n\n" + "Hello " + "world\n" + "\n--- Plan complete ---\n"
	sender.mu.Lock()
	count := len(sender.msgs)
	sender.mu.Unlock()

	if count < 3 {
		t.Fatalf("expected at least 3 messages, got %d", count)
	}
}

func TestAgentRunWithOp(t *testing.T) {
	provider := &mockProvider{
		tokens: []string{
			"Let me add a function.\n",
			"```op\n",
			`{"id":"step-1","kind":"insert","line":2,"col":1,"text":"func foo() {}\n","reason":"add foo"}` + "\n",
			"```\n",
		},
	}
	sender := &mockSender{}
	sess := NewSession()

	a := &Agent{
		Provider: provider,
		Client:   sender,
		Session:  sess,
	}

	// Approve the op in background after a short delay
	go func() {
		// Wait for advance signal request (handleOp sends PendingOpMsg then blocks)
		sess.Mu.Lock()
		sess.Rejected = false
		sess.Mu.Unlock()
		sess.Advance <- struct{}{}
		// Then send continue
		sess.Proceed <- struct{}{}
	}()

	a.Run(context.Background(), "test.go", "package main\n", "add a function")

	sess.Mu.Lock()
	opID := sess.CurrentOpID
	sess.Mu.Unlock()

	if opID != "step-1" {
		t.Fatalf("expected current op step-1, got %q", opID)
	}
}

func TestAgentRunRejection(t *testing.T) {
	provider := &mockProvider{
		tokens: []string{
			"Adding code.\n",
			"```op\n",
			`{"id":"step-1","kind":"insert","line":1,"col":1,"text":"x\n","reason":"test"}` + "\n",
			"```\n",
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
}
