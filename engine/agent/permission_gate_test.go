package agent

import (
	"testing"

	"github.com/latebit-io/junto/engine/llm"
)

// TestShouldNudgePermissionQuestion locks in the conditions under which
// the autonomous-mode gate fires: a yielded turn whose last assistant
// message has no tool calls and ends in `?`. Every other shape (tool
// calls present, no trailing `?`, no assistant message at all) is a
// non-fire so the gate does not interrupt healthy flow.
func TestShouldNudgePermissionQuestion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msgs []llm.Message
		want bool
	}{
		{
			name: "trailing question without tool calls fires",
			msgs: []llm.Message{
				{Role: "user", Content: "do the thing"},
				{Role: "assistant", Content: "Should I proceed with option A or option B?"},
			},
			want: true,
		},
		{
			name: "trailing whitespace tolerated",
			msgs: []llm.Message{
				{Role: "assistant", Content: "Want me to continue?\n\n"},
			},
			want: true,
		},
		{
			name: "tool calls present suppresses fire",
			msgs: []llm.Message{
				{Role: "assistant", Content: "Reading the file?", ToolCalls: []llm.ToolCall{{ID: "1"}}},
			},
			want: false,
		},
		{
			name: "no trailing question",
			msgs: []llm.Message{
				{Role: "assistant", Content: "Done. The file compiles."},
			},
			want: false,
		},
		{
			name: "empty assistant content",
			msgs: []llm.Message{
				{Role: "assistant", Content: ""},
			},
			want: false,
		},
		{
			name: "no assistant message at all",
			msgs: []llm.Message{
				{Role: "user", Content: "do the thing"},
			},
			want: false,
		},
		{
			name: "later non-assistant message ignored, walks back to last assistant",
			msgs: []llm.Message{
				{Role: "assistant", Content: "Want me to continue?"},
				{Role: "tool", Content: "result"},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldNudgePermissionQuestion(tt.msgs); got != tt.want {
				t.Errorf("shouldNudgePermissionQuestion = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestLastAssistantMessage covers the helper that the permission gate
// uses to inspect both Content and ToolCalls (lastAssistantContent only
// returns Content). Single-table test because the helper is trivial; the
// edge cases just verify "walks back from end, returns nil if absent."
func TestLastAssistantMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		msgs    []llm.Message
		wantNil bool
		want    string
	}{
		{
			name:    "empty slice returns nil",
			msgs:    nil,
			wantNil: true,
		},
		{
			name:    "no assistant returns nil",
			msgs:    []llm.Message{{Role: "user", Content: "x"}},
			wantNil: true,
		},
		{
			name: "returns latest assistant when multiple exist",
			msgs: []llm.Message{
				{Role: "assistant", Content: "first"},
				{Role: "user", Content: "reply"},
				{Role: "assistant", Content: "second"},
			},
			want: "second",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := lastAssistantMessage(tt.msgs)
			if tt.wantNil {
				if got != nil {
					t.Errorf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("got nil, want non-nil")
			}
			if got.Content != tt.want {
				t.Errorf("Content = %q, want %q", got.Content, tt.want)
			}
		})
	}
}
