package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/nib/ai/llmtest"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/event"
	"github.com/latebit-io/nib/kit/headless"
	"github.com/latebit-io/nib/kit/tools/bash"
	memorytools "github.com/latebit-io/nib/kit/tools/memory"
)

// TestKitWiring_ToolCallAndPark exercises the kit primitives nib
// relies on: bash + memory tools registered through [kit.New], driven
// by [headless.Runner] in single-shot mode against a mock provider that
// issues a memory_publish tool call then a final text response. The
// run should park naturally (foundation emits AgentParked → kit emits
// AgentWaiting → Runner cancels and exits Success=true), the session
// page should land in the store, and the summary should contain the
// final assistant text.
//
// This is the kit-boundary smoke nib was designed to surface — if
// any tool-plumbing or event-ordering invariant breaks, the test fails
// before nib ever boots a real LLM.
func TestKitWiring_ToolCallAndPark(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	root := t.TempDir()
	sessionID := newSessionID(time.Date(2026, 5, 4, 14, 30, 45, 0, time.UTC), "kit-wiring-smoke")
	sessionPath := sessionsDir + "/" + sessionID + ".md"
	body := "# Findings\n\nRoot exists.\n"

	publishArgs := `{"path":"` + sessionPath + `","body":"` + strings.ReplaceAll(body, "\n", `\n`) + `","expected_version":0}`

	provider := llmtest.NewScripted(
		// Turn 1: tool call to write the session page.
		llmtest.ToolCalls(llmtest.Call("tc-1", "memory_publish", publishArgs)),
		// Turn 2: final text. With no tool calls the agent waits, and the
		// Runner cancels and exits with Success=true.
		llmtest.Text("wrote findings"),
	)

	ag, err := kit.New(kit.Config{
		Provider:     provider,
		SystemPrompt: buildSystemPrompt(sessionID),
		Toolset: kit.Toolset{
			Tools: []kit.Tool{
				bash.New(root),
				memorytools.NewFetchTool(store),
				memorytools.NewPublishTool(store),
				memorytools.NewAppendTool(store),
				memorytools.NewListTool(store),
			},
		},
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	sub, err := ag.Subscribe(kit.SubscribeOptions{BufferSize: eventBufferSize})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	events := sub.Events()
	defer closeAgent(ag, events)

	// Bound the run so a wedged translator/provider cannot hang CI.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	runner := headless.New(ag, events)
	result := runner.Run(ctx, "write findings to memory")

	if !result.Success {
		t.Fatalf("Success=false; runner did not park cleanly. errors=%v", result.Errors)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	if !strings.Contains(result.Summary, "wrote findings") {
		t.Errorf("summary missing final answer: %q", result.Summary)
	}

	doc, ferr := store.Fetch(t.Context(), sessionPath)
	if ferr != nil {
		t.Fatalf("session page not written: %v", ferr)
	}
	if !strings.Contains(doc.Body, "Findings") {
		t.Errorf("session body missing 'Findings': %q", doc.Body)
	}
}

// TestKitWiring_AgentTokenBeforeAgentWaiting is the regression test
// for the AgentParked race: with the old hook-based AgentWaiting send,
// final-turn tokens could arrive on the consumer channel AFTER
// AgentWaiting because two writers raced (translator + hook). Now
// AgentWaiting flows through the translator alongside AgentToken,
// guaranteeing stream order. This test asserts the contract directly
// by inspecting raw event arrival order, not Runner-derived state.
func TestKitWiring_AgentTokenBeforeAgentWaiting(t *testing.T) {
	t.Parallel()

	provider := llmtest.NewScripted(llmtest.Text("first", " second", " third"))

	ag, err := kit.New(kit.Config{
		Provider:     provider,
		SystemPrompt: "test",
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	sub, err := ag.Subscribe(kit.SubscribeOptions{BufferSize: eventBufferSize})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	events := sub.Events()
	defer closeAgent(ag, events)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	if err := ag.Prompt(ctx, "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	var sawTokens []string
	deadline := time.After(5 * time.Second)
drain:
	for {
		select {
		case ev := <-events:
			switch e := ev.(type) {
			case event.AgentToken:
				sawTokens = append(sawTokens, e.Text)
			case event.AgentWaiting:
				if len(sawTokens) != 3 {
					t.Errorf("AgentWaiting arrived after %d tokens, want 3 (race regression)", len(sawTokens))
				}
				ag.Cancel()
				break drain
			}
		case <-deadline:
			t.Fatalf("timeout waiting for AgentWaiting; saw tokens=%v", sawTokens)
		}
	}
}

func TestClassifyStatus(t *testing.T) {
	t.Parallel()

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	live := context.Background()

	cases := []struct {
		name string
		ctx  context.Context
		res  *headless.Result
		want sessionStatus
	}{
		{"success", live, &headless.Result{Success: true}, statusSuccess},
		{"failed-no-cancel", live, &headless.Result{Success: false}, statusFailed},
		{"cancelled", cancelled, &headless.Result{Success: false}, statusCancelled},
		{"success-trumps-cancel", cancelled, &headless.Result{Success: true}, statusSuccess},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyStatus(tc.ctx, tc.res)
			if got != tc.want {
				t.Errorf("classifyStatus = %q, want %q", got, tc.want)
			}
		})
	}
}
