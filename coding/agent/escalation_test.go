package agent

import (
	"context"
	"testing"

	"github.com/latebit-io/junto/ai/llm"
)

func TestEscalateMaxTokens(t *testing.T) {
	tests := []struct {
		name    string
		current int
		want    int
	}{
		{"unset starts at initial escalation", 0, maxTokensInitialEscalation},
		{"small value jumps to initial", 1024, maxTokensInitialEscalation},
		{"below-initial doubles below initial → floor to initial", 8000, maxTokensInitialEscalation},
		{"at initial doubles", maxTokensInitialEscalation, maxTokensInitialEscalation * 2},
		{"caps at ceiling", maxTokensCeiling / 2, maxTokensCeiling},
		{"beyond ceiling stays at ceiling", maxTokensCeiling + 10_000, maxTokensCeiling},
		{"at ceiling does not move", maxTokensCeiling, maxTokensCeiling},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := escalateMaxTokens(tc.current); got != tc.want {
				t.Errorf("escalateMaxTokens(%d) = %d, want %d", tc.current, got, tc.want)
			}
		})
	}
}

// stubEscalator is a minimal provider that implements both llm.Provider
// (via a noop Stream) and maxTokensEscalator so we can verify the
// escalation path without spinning up real HTTP.
type stubEscalator struct {
	max int
}

func (s *stubEscalator) Stream(_ context.Context, _ []llm.Message, _ []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent)
	close(ch)
	return ch, nil
}

func (s *stubEscalator) MaxTokens() int     { return s.max }
func (s *stubEscalator) SetMaxTokens(v int) { s.max = v }

func TestAgent_EscalateProviderMaxTokens(t *testing.T) {
	t.Run("doubles unset provider to initial", func(t *testing.T) {
		esc := &stubEscalator{max: 0}
		from, to, ok := (&Agent{provider: esc}).escalateProviderMaxTokens()
		if !ok {
			t.Fatal("expected escalation to succeed")
		}
		if from != 0 {
			t.Errorf("from = %d, want 0", from)
		}
		if to != maxTokensInitialEscalation {
			t.Errorf("to = %d, want %d", to, maxTokensInitialEscalation)
		}
		if esc.max != maxTokensInitialEscalation {
			t.Errorf("provider max = %d, want %d", esc.max, maxTokensInitialEscalation)
		}
	})

	t.Run("does not move past ceiling", func(t *testing.T) {
		esc := &stubEscalator{max: maxTokensCeiling}
		from, to, ok := (&Agent{provider: esc}).escalateProviderMaxTokens()
		if ok {
			t.Fatal("expected no escalation at ceiling")
		}
		if from != maxTokensCeiling || to != maxTokensCeiling {
			t.Errorf("from=%d to=%d, want both %d", from, to, maxTokensCeiling)
		}
		if esc.max != maxTokensCeiling {
			t.Errorf("provider max changed unexpectedly: %d", esc.max)
		}
	})
}
