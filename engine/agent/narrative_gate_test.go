package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/llm"
)

// taskTreeWorkspace is a stubWorkspace that also satisfies TaskReader.
// Defaults reflect a loaded tree with no active task — flip loaded or
// active in a test to exercise the unloaded / mid-task branches.
type taskTreeWorkspace struct {
	stubWorkspace
	next     string
	active   string
	unloaded bool
}

func (t taskTreeWorkspace) ActiveTaskPath() string  { return t.active }
func (t taskTreeWorkspace) WorkTreeLoaded() bool    { return !t.unloaded }
func (t taskTreeWorkspace) NextPendingTask() string { return t.next }

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
		{"plain not", "the function returns true if not idle", false},
		{"plain need", "we need this commit message to be precise", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := containsOutstandingWorkMarker(tc.in); got != tc.want {
				t.Errorf("containsOutstandingWorkMarker(%q) = %v, want %v", tc.in, got, tc.want)
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
	if got := lastAssistantContent(messages); got != "final" {
		t.Errorf("lastAssistantContent = %q, want %q", got, "final")
	}
	if got := lastAssistantContent(nil); got != "" {
		t.Errorf("lastAssistantContent(nil) = %q, want empty", got)
	}
}

// TestAgent_NarrativeGate_FiresWhenTreeEmptyAndOutstandingLanguage
// drives the agent through one turn that yields with text containing an
// outstanding-work marker while the task tree is empty. The gate should
// inject the nudge and re-loop instead of yielding to AgentWaiting.
func TestAgent_NarrativeGate_FiresWhenTreeEmptyAndOutstandingLanguage(t *testing.T) {
	provider := &multiTurnProvider{
		turns: [][]llm.StreamEvent{
			// Turn 1: model yields with a verbose summary that mentions
			// outstanding work — gate should fire.
			{
				{Token: "All tasks complete. Maze rendering still needs implementation."},
				{Done: true},
			},
			// Turn 2: model corrects course (or just yields cleanly).
			{
				{Token: "Acknowledged."},
				{Done: true},
			},
		},
	}

	events := make(chan event.Event, 64)
	ag := New(provider, taskTreeWorkspace{next: ""}, events, nil)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ag.RunWithMode(ctx, "main.go", "", "go", nil, ModeExecution)

	// Wait for AgentWaiting — the gate should fire once, then yield on
	// the second turn after the nudge is delivered.
	if drainUntil(t, events, 2*time.Second, func(ev event.Event) bool {
		_, ok := ev.(event.AgentWaiting)
		return ok
	}) == nil {
		t.Fatal("timeout waiting for AgentWaiting after narrative gate")
	}

	// The provider should have been called exactly twice: original turn
	// plus one re-loop after the nudge.
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.call != 2 {
		t.Errorf("provider call count = %d, want 2 (original + nudge re-loop)", provider.call)
	}
}

// TestAgent_NarrativeGate_DoesNotFireWhenTreeHasPendingWork verifies the
// gate is a NO-OP when the task tree still has pending work — outstanding
// language in the narrative is irrelevant in that case because the model
// is mid-flow, not declaring completion.
func TestAgent_NarrativeGate_DoesNotFireWhenTreeHasPendingWork(t *testing.T) {
	provider := &multiTurnProvider{
		turns: [][]llm.StreamEvent{
			{
				{Token: "Mid-flow note: collisions still need wiring up."},
				{Done: true},
			},
		},
	}

	events := make(chan event.Event, 64)
	ag := New(provider, taskTreeWorkspace{next: "Implement collisions"}, events, nil)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ag.RunWithMode(ctx, "main.go", "", "go", nil, ModeExecution)

	if drainUntil(t, events, 2*time.Second, func(ev event.Event) bool {
		_, ok := ev.(event.AgentWaiting)
		return ok
	}) == nil {
		t.Fatal("timeout waiting for AgentWaiting")
	}

	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.call != 1 {
		t.Errorf("provider call count = %d, want 1 (gate must not re-loop)", provider.call)
	}
}

// TestAgent_NarrativeGate_FiresOncePerDeveloperTurn verifies that the
// gate's once-per-developer-input budget holds: even if the model emits
// outstanding language a second time within the same input cycle, the
// gate must NOT re-fire (otherwise it could loop forever on a model that
// refuses to comply).
func TestAgent_NarrativeGate_FiresOncePerDeveloperTurn(t *testing.T) {
	provider := &multiTurnProvider{
		turns: [][]llm.StreamEvent{
			// Turn 1: outstanding language — gate fires.
			{
				{Token: "Done — but ghost AI still needs implementation."},
				{Done: true},
			},
			// Turn 2: model still refuses to add the task — outstanding
			// language again. Gate should NOT fire (budget exhausted).
			{
				{Token: "Right, ghost AI still needs work. Yielding anyway."},
				{Done: true},
			},
		},
	}

	events := make(chan event.Event, 64)
	ag := New(provider, taskTreeWorkspace{next: ""}, events, nil)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ag.RunWithMode(ctx, "main.go", "", "go", nil, ModeExecution)

	if drainUntil(t, events, 2*time.Second, func(ev event.Event) bool {
		_, ok := ev.(event.AgentWaiting)
		return ok
	}) == nil {
		t.Fatal("timeout waiting for AgentWaiting")
	}

	provider.mu.Lock()
	defer provider.mu.Unlock()
	// Two calls total: original + one nudge re-loop. NOT three (would
	// indicate the gate fired twice).
	if provider.call != 2 {
		t.Errorf("provider call count = %d, want 2 (gate fires at most once per dev turn)", provider.call)
	}
}

// TestAgent_AgentWaiting_FinishedWhenTreeEmpty verifies that AgentWaiting
// carries Finished=true when the task tree is empty at yield time. This
// is the signal the TUI uses to render the DONE chip instead of REPLY.
func TestAgent_AgentWaiting_FinishedWhenTreeEmpty(t *testing.T) {
	provider := &multiTurnProvider{
		turns: [][]llm.StreamEvent{
			{
				{Token: "All wrapped up cleanly."},
				{Done: true},
			},
		},
	}

	events := make(chan event.Event, 64)
	ag := New(provider, taskTreeWorkspace{next: ""}, events, nil)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ag.RunWithMode(ctx, "main.go", "", "go", nil, ModeExecution)

	ev := drainUntil(t, events, 2*time.Second, func(ev event.Event) bool {
		_, ok := ev.(event.AgentWaiting)
		return ok
	})
	if ev == nil {
		t.Fatal("timeout waiting for AgentWaiting")
	}
	w := ev.(event.AgentWaiting)
	if !w.Finished {
		t.Error("AgentWaiting.Finished = false, want true (task tree empty)")
	}
}

// TestAgent_AgentWaiting_NotFinishedWhenTreeHasPendingWork verifies the
// inverse: AgentWaiting carries Finished=false when there is still
// pending work. The DONE chip must NOT appear in this case.
func TestAgent_AgentWaiting_NotFinishedWhenTreeHasPendingWork(t *testing.T) {
	provider := &multiTurnProvider{
		turns: [][]llm.StreamEvent{
			{
				{Token: "Pausing for now."},
				{Done: true},
			},
		},
	}

	events := make(chan event.Event, 64)
	ag := New(provider, taskTreeWorkspace{next: "Implement HUD"}, events, nil)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ag.RunWithMode(ctx, "main.go", "", "go", nil, ModeExecution)

	ev := drainUntil(t, events, 2*time.Second, func(ev event.Event) bool {
		_, ok := ev.(event.AgentWaiting)
		return ok
	})
	if ev == nil {
		t.Fatal("timeout waiting for AgentWaiting")
	}
	w := ev.(event.AgentWaiting)
	if w.Finished {
		t.Error("AgentWaiting.Finished = true, want false (pending task remains)")
	}
}

// TestAgent_NarrativeGate_UnloadedTree_NoFire verifies that the gate is
// silent when the work tree has not loaded yet. An unloaded tree reads
// as "no pending tasks" via the nil-tree fallback, but that's "unknown"
// not "complete" — firing Finished or the nudge here would be wrong on
// every cold start before /project.md fetches.
func TestAgent_NarrativeGate_UnloadedTree_NoFire(t *testing.T) {
	provider := &multiTurnProvider{
		turns: [][]llm.StreamEvent{
			{
				{Token: "Setup not yet complete."},
				{Done: true},
			},
		},
	}

	events := make(chan event.Event, 64)
	ag := New(provider, taskTreeWorkspace{unloaded: true}, events, nil)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ag.RunWithMode(ctx, "main.go", "", "go", nil, ModeExecution)

	ev := drainUntil(t, events, 2*time.Second, func(ev event.Event) bool {
		_, ok := ev.(event.AgentWaiting)
		return ok
	})
	if ev == nil {
		t.Fatal("timeout waiting for AgentWaiting")
	}
	w := ev.(event.AgentWaiting)
	if w.Finished {
		t.Error("AgentWaiting.Finished = true on unloaded tree, want false (unknown != done)")
	}

	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.call != 1 {
		t.Errorf("provider call count = %d, want 1 (gate must not fire on unloaded tree)", provider.call)
	}
}

// TestAgent_NarrativeGate_ActiveTaskInProgress_NoFire verifies that an
// active [>] task suppresses both the Finished signal and the narrative
// gate. FindNextPendingTask only walks [ ] tasks, so a tree with one
// active leaf and zero pending leaves reads as empty pending — without
// the active-task check the agent would declare completion mid-work.
func TestAgent_NarrativeGate_ActiveTaskInProgress_NoFire(t *testing.T) {
	provider := &multiTurnProvider{
		turns: [][]llm.StreamEvent{
			{
				{Token: "Mid-flow note: collisions still need wiring up."},
				{Done: true},
			},
		},
	}

	events := make(chan event.Event, 64)
	ag := New(provider, taskTreeWorkspace{active: "Implement collisions"}, events, nil)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ag.RunWithMode(ctx, "main.go", "", "go", nil, ModeExecution)

	ev := drainUntil(t, events, 2*time.Second, func(ev event.Event) bool {
		_, ok := ev.(event.AgentWaiting)
		return ok
	})
	if ev == nil {
		t.Fatal("timeout waiting for AgentWaiting")
	}
	w := ev.(event.AgentWaiting)
	if w.Finished {
		t.Error("AgentWaiting.Finished = true while a task is active, want false")
	}

	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.call != 1 {
		t.Errorf("provider call count = %d, want 1 (gate must not fire while task active)", provider.call)
	}
}

// TestAgent_NarrativeGate_NoTaskReader_NoFire verifies that without a
// TaskReader workspace (no project plan) the gate is silent — there is
// nothing to compare the narrative against, so we cannot tell whether
// "still need" language is wrong.
func TestAgent_NarrativeGate_NoTaskReader_NoFire(t *testing.T) {
	provider := &multiTurnProvider{
		turns: [][]llm.StreamEvent{
			{
				{Token: "Note: the renderer still needs work."},
				{Done: true},
			},
		},
	}

	events := make(chan event.Event, 64)
	// stubWorkspace is NOT a TaskReader.
	ag := New(provider, stubWorkspace{}, events, nil)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ag.RunWithMode(ctx, "main.go", "", "go", nil, ModeExecution)

	ev := drainUntil(t, events, 2*time.Second, func(ev event.Event) bool {
		_, ok := ev.(event.AgentWaiting)
		return ok
	})
	if ev == nil {
		t.Fatal("timeout waiting for AgentWaiting")
	}
	w := ev.(event.AgentWaiting)
	if w.Finished {
		t.Error("AgentWaiting.Finished = true, want false (no TaskReader = unknown)")
	}

	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.call != 1 {
		t.Errorf("provider call count = %d, want 1 (gate must not fire without TaskReader)", provider.call)
	}

	// Sanity: the nudge banner text should NOT appear in the event stream.
	tokens := drainTokens(events)
	if strings.Contains(tokens, "Nudge:") {
		t.Errorf("nudge banner emitted without TaskReader: %q", tokens)
	}
}
