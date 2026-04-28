package nudges

import (
	"testing"

	"github.com/latebit-io/junto/ai/llm"
)

func TestContainsOutstandingWorkMarker(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"clean wrap-up", "All done. Tests pass.", false},
		{"still need", "exact maze verification still need implementation", true},
		{"still needs caps", "Frightened ghost behavior STILL NEEDS work.", true},
		{"not yet", "ghost AI not yet wired to update loop", true},
		{"need implementation", "fruit spawn rules need implementation", true},
		{"yet to be", "level transitions yet to be implemented", true},
		{"todo prefix", "TODO: hook collisions", true},
		{"outstanding work phrase", "outstanding work on the AI module", true},
		{"outstanding items phrase", "two outstanding items remain in the queue", true},
		{"plain not", "the function returns true if not idle", false},
		{"plain need", "we need this commit message to be precise", false},
		// "outstanding" as a bare adjective ("Outstanding!", "Outstanding
		// result.") must NOT fire — that was the bug behind narrowing to
		// phrase-only matches. The phrase variants ("outstanding work",
		// "outstanding items") still match on praise like "outstanding
		// work — well done", but that's an accepted trade-off: a false
		// positive costs one nudge round-trip (the gate fires at most
		// once per developer turn), while a false negative lets the
		// original "all done + still-need-X" bug through.
		{"bare praise outstanding", "Outstanding!", false},
		{"bare praise outstanding result", "Outstanding result on this run.", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ContainsOutstandingWorkMarker(tc.in); got != tc.want {
				t.Errorf("ContainsOutstandingWorkMarker(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestLastAssistantContent(t *testing.T) {
	t.Parallel()
	messages := []llm.Message{
		{Role: "system", Content: "system prompt"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "first"},
		{Role: "tool", Content: "tool result"},
		{Role: "assistant", Content: "final"},
	}
	if got := LastAssistantContent(messages); got != "final" {
		t.Errorf("LastAssistantContent = %q, want %q", got, "final")
	}
	if got := LastAssistantContent(nil); got != "" {
		t.Errorf("LastAssistantContent(nil) = %q, want empty", got)
	}
}

// permissionGateCase describes one ShouldNudgePermissionQuestion
// scenario. Defined here so the structural-shape tests and the
// phrase-pattern tests share a single row type and a single runner.
type permissionGateCase struct {
	name string
	msgs []llm.Message
	want bool
}

// runPermissionGateCases is the shared table-driven body for the two
// ShouldNudgePermissionQuestion test groups. Split into two callers so
// each function stays under the funlen cap; the runner ensures both
// suites assert on identical semantics.
func runPermissionGateCases(t *testing.T, cases []permissionGateCase) {
	t.Helper()
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ShouldNudgePermissionQuestion(tt.msgs); got != tt.want {
				t.Errorf("ShouldNudgePermissionQuestion = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestShouldNudgePermissionQuestion_Structure locks in the structural
// conditions: tool-call presence short-circuits, no-assistant returns
// false, walks back from the latest message, and the existing
// trailing-`?` signal still fires. Phrase patterns are covered
// separately in [TestShouldNudgePermissionQuestion_PhraseForms].
func TestShouldNudgePermissionQuestion_Structure(t *testing.T) {
	t.Parallel()
	runPermissionGateCases(t, []permissionGateCase{
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
			msgs: []llm.Message{{Role: "assistant", Content: "Want me to continue?\n\n"}},
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
			msgs: []llm.Message{{Role: "assistant", Content: "Done. The file compiles."}},
			want: false,
		},
		{
			name: "empty assistant content",
			msgs: []llm.Message{{Role: "assistant", Content: ""}},
			want: false,
		},
		{
			name: "no assistant message at all",
			msgs: []llm.Message{{Role: "user", Content: "do the thing"}},
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
	})
}

// TestShouldNudgePermissionQuestion_PhraseForms covers the phrase-
// based signal added 2026-04-27: permission-seeking *statements* (no
// trailing `?`) that the original predicate missed. Each row is a
// phrase observed in real autonomous-mode failure transcripts.
func TestShouldNudgePermissionQuestion_PhraseForms(t *testing.T) {
	t.Parallel()
	runPermissionGateCases(t, []permissionGateCase{
		{
			name: "let me know offer",
			msgs: []llm.Message{{Role: "assistant", Content: "Done with the first edit. Let me know if you want me to continue with the next phase."}},
			want: true,
		},
		{
			name: "say keep going offer",
			msgs: []llm.Message{{Role: "assistant", Content: "I'll pause here. Say keep going and I'll continue Phase 3/4."}},
			want: true,
		},
		{
			name: "want me to proceed",
			msgs: []llm.Message{{Role: "assistant", Content: "Want me to proceed with the cleanup."}},
			want: true,
		},
		{
			name: "if you want statement",
			msgs: []llm.Message{{Role: "assistant", Content: "Phase 2 complete. If you want, next turn I'll start Phase 3."}},
			want: true,
		},
		{
			name: "i can continue",
			msgs: []llm.Message{{Role: "assistant", Content: "Stopping here. I can continue when you give the green light."}},
			want: true,
		},
		{
			name: "ready when you are",
			msgs: []llm.Message{{Role: "assistant", Content: "Ready when you are."}},
			want: true,
		},
		{
			name: "case insensitive",
			msgs: []llm.Message{{Role: "assistant", Content: "LET ME KNOW IF YOU WANT TO PROCEED."}},
			want: true,
		},
		{
			name: "non-permission text without ? does not fire",
			msgs: []llm.Message{{Role: "assistant", Content: "Done. The build passes and tests are green."}},
			want: false,
		},
	})
}

// TestLastAssistantMessage covers the helper that the permission gate
// uses to inspect both Content and ToolCalls (LastAssistantContent
// only returns Content). Single-table test because the helper is
// trivial; the edge cases just verify "walks back from end, returns
// nil if absent."
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
			got := LastAssistantMessage(tt.msgs)
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
