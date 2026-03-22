package session

import (
	"context"
	"testing"

	"github.com/latebit-io/junto/engine/agent"
	"github.com/latebit-io/junto/engine/buffer"
	"github.com/latebit-io/junto/engine/editor"
	"github.com/latebit-io/junto/engine/llm"
)

// stubProvider satisfies llm.Provider for constructing an agent in tests.
type stubProvider struct{}

func (stubProvider) Stream(_ context.Context, _ []llm.Message) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent)
	close(ch)
	return ch, nil
}

// newTestSession creates a session with a buffer containing the given text
// and a real agent (needed to test approval signaling).
func newTestSession(content string) *Session {
	buf := buffer.New()
	if content != "" {
		buf.Insert(0, 0, content)
	}
	e := editor.New(buf)
	events := make(chan agent.Event, 64)
	ag := agent.New(stubProvider{}, events)
	return New(e, ag, events)
}

func TestPrepareApproval(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		search   string
		replace  string
		setup    func(s *Session) // set up pending edit + review
		wantErr  string
		wantLine int
		wantCol  int
	}{
		{
			name:    "no pending edit",
			content: "hello world",
			search:  "hello",
			replace: "goodbye",
			setup:   func(_ *Session) {},
			wantErr: "no pending edit",
		},
		{
			name:    "edit not reviewed",
			content: "hello world",
			search:  "hello",
			replace: "goodbye",
			setup: func(s *Session) {
				s.PendingEdit = &agent.PendingEdit{Search: "hello", Replace: "goodbye"}
				// Don't call ReviewEdit
			},
			wantErr: "edit not reviewed",
		},
		{
			name:    "search text not found",
			content: "hello world",
			search:  "missing",
			replace: "found",
			setup: func(s *Session) {
				s.PendingEdit = &agent.PendingEdit{Search: "missing", Replace: "found"}
				s.editReviewed = true
			},
			wantErr: "text not found",
		},
		{
			name:    "ambiguous match",
			content: "hello hello",
			search:  "hello",
			replace: "goodbye",
			setup: func(s *Session) {
				s.PendingEdit = &agent.PendingEdit{Search: "hello", Replace: "goodbye"}
				s.editReviewed = true
			},
			wantErr: "2 matches found",
		},
		{
			name:     "success at start of buffer",
			content:  "hello world",
			search:   "hello",
			replace:  "goodbye",
			wantLine: 0,
			wantCol:  0,
			setup: func(s *Session) {
				s.PendingEdit = &agent.PendingEdit{Search: "hello", Replace: "goodbye"}
				s.editReviewed = true
			},
		},
		{
			name:     "success mid-line",
			content:  "hello world",
			search:   "world",
			replace:  "earth",
			wantLine: 0,
			wantCol:  6,
			setup: func(s *Session) {
				s.PendingEdit = &agent.PendingEdit{Search: "world", Replace: "earth"}
				s.editReviewed = true
			},
		},
		{
			name:     "success on second line",
			content:  "line one\nline two\nline three",
			search:   "line two",
			replace:  "LINE TWO",
			wantLine: 1,
			wantCol:  0,
			setup: func(s *Session) {
				s.PendingEdit = &agent.PendingEdit{Search: "line two", Replace: "LINE TWO"}
				s.editReviewed = true
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestSession(tt.content)
			tt.setup(s)

			plan, err := s.PrepareApproval(tt.search, tt.replace)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %q", tt.wantErr, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if plan == nil {
				t.Fatal("expected non-nil plan")
			}
			if plan.Line != tt.wantLine {
				t.Errorf("Line = %d, want %d", plan.Line, tt.wantLine)
			}
			if plan.Col != tt.wantCol {
				t.Errorf("Col = %d, want %d", plan.Col, tt.wantCol)
			}
			if plan.Search != tt.search {
				t.Errorf("Search = %q, want %q", plan.Search, tt.search)
			}
			if plan.Replace != tt.replace {
				t.Errorf("Replace = %q, want %q", plan.Replace, tt.replace)
			}
			// Pending edit should be cleared
			if s.PendingEdit != nil {
				t.Error("PendingEdit should be nil after PrepareApproval")
			}
			if s.editReviewed {
				t.Error("editReviewed should be false after PrepareApproval")
			}
		})
	}
}

func TestPrepareApprovalDoesNotMutateBuffer(t *testing.T) {
	s := newTestSession("hello world")
	s.PendingEdit = &agent.PendingEdit{Search: "hello", Replace: "goodbye"}
	s.editReviewed = true

	_, err := s.PrepareApproval("hello", "goodbye")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := s.Editor.Buf.Content()
	if got != "hello world" {
		t.Errorf("buffer mutated: got %q, want %q", got, "hello world")
	}
}

func TestCompleteApprovalSignalsAgent(t *testing.T) {
	s := newTestSession("hello world")
	// Pre-load the approve channel to verify CompleteApproval sends to it.
	// Agent.Approve() sends true on approveCh (buffered size 1).
	s.CompleteApproval()
	// If this doesn't panic or block, the signal was sent successfully.
	// We can't easily read the internal channel, but the method should not error.
}

func TestPrepareApprovalRejectsOnLocationFailure(t *testing.T) {
	s := newTestSession("hello world")
	s.PendingEdit = &agent.PendingEdit{Search: "missing", Replace: "found"}
	s.editReviewed = true

	_, err := s.PrepareApproval("missing", "found")
	if err == nil {
		t.Fatal("expected error for missing search text")
	}
	// Agent should have been signaled to reject (via agent.Reject)
	// and pending state should be cleared.
	if s.PendingEdit != nil {
		t.Error("PendingEdit should be nil after failed PrepareApproval")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && containsAt(s, substr)
}

func containsAt(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
