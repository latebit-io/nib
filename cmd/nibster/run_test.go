package main

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/event"
	"github.com/latebit-io/nib/kit/tools/bash"
	memorytools "github.com/latebit-io/nib/kit/tools/memory"
)

// scriptedProvider serves up a queue of pre-built stream responses, one
// per Stream call. Mirrors the kit_test.go helper — copied here so the
// test stays self-contained without exporting a kit testutil package.
type scriptedProvider struct {
	mu    sync.Mutex
	turns [][]llm.StreamEvent
}

func (p *scriptedProvider) Stream(_ context.Context, _ []llm.Message, _ []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	p.mu.Lock()
	var events []llm.StreamEvent
	if len(p.turns) > 0 {
		events = p.turns[0]
		p.turns = p.turns[1:]
	}
	p.mu.Unlock()

	ch := make(chan llm.StreamEvent, len(events)+1)
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

// TestKitWiring_ToolCallAndPark exercises the kit primitives nibster
// relies on: bash + memory tools registered through [kit.New], driven
// by the same drain loop runAgent uses, against a mock provider that
// issues a memory_publish tool call then a final text response. The
// run should park naturally (no more tool calls), the session page
// should land in the store, and the summary should contain the final
// assistant text.
//
// This is the kit-boundary smoke nibster was designed to surface — if
// any tool-plumbing or event-ordering invariant breaks, the test fails
// before nibster ever boots a real LLM.
func TestKitWiring_ToolCallAndPark(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	root := t.TempDir()
	sessionID := newSessionID(time.Date(2026, 5, 4, 14, 30, 45, 0, time.UTC), "kit-wiring-smoke")
	sessionPath := sessionsDir + "/" + sessionID + ".md"
	body := "# Findings\n\nRoot exists.\n"

	publishArgs := `{"path":"` + sessionPath + `","body":"` + strings.ReplaceAll(body, "\n", `\n`) + `","expected_version":0}`

	provider := &scriptedProvider{
		turns: [][]llm.StreamEvent{
			// Turn 1: tool call to write the session page.
			{{
				Done: true,
				ToolCalls: []llm.ToolCall{{
					ID:       "tc-1",
					Function: llm.FunctionCall{Name: "memory_publish", Arguments: publishArgs},
				}},
			}},
			// Turn 2: final assistant text. No tool calls — the foundation
			// will call GetFollowUpMessages, which our hook uses to
			// latch parked=true and Cancel the run.
			{
				{Token: "wrote findings"},
				{Done: true},
			},
		},
	}

	events := make(chan event.Event, eventBufferSize)
	var parked atomic.Bool
	var agRef *kit.Agent

	ag, err := kit.New(kit.Config{
		Provider:     provider,
		Events:       events,
		SystemPrompt: buildSystemPrompt(sessionID),
		Tools: []kit.Tool{
			bash.New(root),
			memorytools.NewFetchTool(store),
			memorytools.NewPublishTool(store),
			memorytools.NewAppendTool(store),
			memorytools.NewListTool(store),
		},
		Hooks: kit.Hooks{GetFollowUpMessages: parkAndCancel(&parked, &agRef)},
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	agRef = ag
	defer closeAgent(ag, events)

	// Bound the run so a wedged translator/provider cannot hang CI.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	if err := ag.Prompt(ctx, "write findings to memory"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	result := driveAgent(ctx, ag, events, &parked)

	if !result.parkedNaturally {
		t.Fatalf("parkedNaturally = false; agent did not reach park state. errs=%v", result.errs)
	}
	if len(result.errs) != 0 {
		t.Fatalf("unexpected errors: %v", result.errs)
	}
	if !strings.Contains(result.summary, "wrote findings") {
		t.Errorf("summary missing final answer: %q", result.summary)
	}

	doc, ferr := store.Fetch(t.Context(), sessionPath)
	if ferr != nil {
		t.Fatalf("session page not written: %v", ferr)
	}
	if !strings.Contains(doc.Body, "Findings") {
		t.Errorf("session body missing 'Findings': %q", doc.Body)
	}
}

func TestClassifyStatusFromResult(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   runResult
		want sessionStatus
	}{
		{"parked-no-errors", runResult{parkedNaturally: true}, statusSuccess},
		{"parked-with-errors", runResult{parkedNaturally: true, errs: []string{"oops"}}, statusFailed},
		{"context-cancelled", runResult{contextCancelled: true}, statusCancelled},
		{"no-park-no-cancel", runResult{}, statusFailed},
		{"errors-and-cancel-prefer-cancel", runResult{contextCancelled: true, errs: []string{"oops"}}, statusCancelled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyStatusFromResult(tc.in)
			if got != tc.want {
				t.Errorf("classifyStatusFromResult(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestAppendBounded_TruncatesAtCap(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	// Pre-fill close to the cap.
	b.WriteString(strings.Repeat("a", maxSummaryBytes-10))
	appendBounded(&b, strings.Repeat("b", 100))
	if !strings.Contains(b.String(), "[summary truncated]") {
		t.Errorf("missing truncation marker; len=%d", b.Len())
	}

	// Subsequent appends are no-ops.
	before := b.Len()
	appendBounded(&b, "more")
	if b.Len() != before {
		t.Errorf("appendBounded after truncation grew the buffer: %d → %d", before, b.Len())
	}
}
