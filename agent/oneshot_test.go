package agent

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/latebit-io/nib/agent/event"
	"github.com/latebit-io/nib/ai/llm"
)

// TestOneShot_EndsRunInsteadOfParking covers every path that reaches the
// park point: the run must end there, after steering and follow-up have
// had their chance to continue it.
func TestOneShot_EndsRunInsteadOfParking(t *testing.T) {
	t.Parallel()

	call := llm.ToolCall{ID: "c1", Type: "function", Function: llm.FunctionCall{Name: "echo", Arguments: "{}"}}
	// once returns a hook that yields one user message on its first call.
	once := func(text string) func(context.Context) ([]llm.Message, error) {
		fired := false
		return func(context.Context) ([]llm.Message, error) {
			if fired {
				return nil, nil
			}
			fired = true
			return []llm.Message{{Role: "user", Content: text}}, nil
		}
	}

	tests := []struct {
		name      string
		turns     [][]llm.StreamEvent
		hooks     Hooks
		wantRoles []string
	}{
		{
			name:      "text turn",
			turns:     [][]llm.StreamEvent{streamText([]string{"done"}, nil)},
			wantRoles: []string{"user", "assistant"},
		},
		{
			name:      "tool call then text",
			turns:     [][]llm.StreamEvent{streamWithToolCalls([]llm.ToolCall{call}), streamText([]string{"done"}, nil)},
			wantRoles: []string{"user", "assistant", "tool", "assistant"},
		},
		{
			name:      "steering continues the run",
			turns:     [][]llm.StreamEvent{streamDone(), streamDone()},
			hooks:     Hooks{SteeringMessages: once("steer")},
			wantRoles: []string{"user", "assistant", "user", "assistant"},
		},
		{
			name:      "follow-up continues the run",
			turns:     [][]llm.StreamEvent{streamDone(), streamDone()},
			hooks:     Hooks{FollowUpMessages: once("more")},
			wantRoles: []string{"user", "assistant", "user", "assistant"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			events := make(chan event.Event, 64)
			a, err := New(Options{
				Provider: newScriptedProvider(tc.turns...),
				Events:   events,
				Tools:    []Tool{namedTool{name: "echo"}},
				Hooks:    tc.hooks,
				OneShot:  true,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := a.Prompt(context.Background(), "go"); err != nil {
				t.Fatalf("Prompt: %v", err)
			}

			all := drainUntilEnd(events)
			a.WaitForIdle()

			end, ok := findEvent[event.AgentEnd](all)
			if !ok {
				t.Fatal("missing AgentEnd: the run did not end on its own")
			}
			roles := make([]string, 0, len(end.Messages))
			for _, m := range end.Messages {
				roles = append(roles, m.Role)
			}
			if !slices.Equal(roles, tc.wantRoles) {
				t.Errorf("transcript roles = %v; want %v", roles, tc.wantRoles)
			}
			if n := countEvents[event.AgentParked](all); n != 0 {
				t.Errorf("AgentParked count = %d; want 0", n)
			}
			if n := countEvents[event.Error](all); n != 0 {
				t.Errorf("Error count = %d; want 0", n)
			}
		})
	}
}

// TestOneShot_ReplyRefusedAndAgentReusable: a reply to a one-shot run
// would never be read, so Reply must refuse it even mid-run. The agent
// must accept a new Prompt once the run has ended.
func TestOneShot_ReplyRefusedAndAgentReusable(t *testing.T) {
	t.Parallel()

	// The steering hook holds the run open at the park point until released.
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	hooks := Hooks{SteeringMessages: func(context.Context) ([]llm.Message, error) {
		started <- struct{}{}
		<-release
		return nil, nil
	}}
	events := make(chan event.Event, 64)
	a, err := New(Options{
		Provider: newScriptedProvider(streamDone(), streamDone()),
		Events:   events,
		Hooks:    hooks,
		OneShot:  true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := a.Prompt(context.Background(), "first"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	<-started
	if a.Reply(context.Background(), "ignored") {
		t.Error("Reply on a running one-shot agent returned true; want false")
	}
	close(release)
	if _, ok := findEvent[event.AgentEnd](drainUntilEnd(events)); !ok {
		t.Fatal("first run: missing AgentEnd")
	}
	a.WaitForIdle()

	if err := a.Prompt(context.Background(), "second"); err != nil {
		t.Fatalf("second Prompt: %v", err)
	}
	if _, ok := findEvent[event.AgentEnd](drainUntilEnd(events)); !ok {
		t.Fatal("second run: missing AgentEnd")
	}
	a.WaitForIdle()
}

// TestRunEnd_CancelsRunContext: a run that ends on its own must release
// its ctx, or every finished run stays registered with the caller's ctx.
func TestRunEnd_CancelsRunContext(t *testing.T) {
	t.Parallel()

	var runCtx context.Context
	hooks := Hooks{TransformContext: func(ctx context.Context, _ []llm.Message) ([]llm.Message, error) {
		runCtx = ctx
		return nil, nil
	}}
	events := make(chan event.Event, 64)
	a, err := New(Options{Provider: newScriptedProvider(streamDone()), Events: events, Hooks: hooks, OneShot: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	drainUntilEnd(events)
	a.WaitForIdle()

	if !errors.Is(runCtx.Err(), context.Canceled) {
		t.Errorf("run ctx Err after run end = %v; want context.Canceled", runCtx.Err())
	}
}
