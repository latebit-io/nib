package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/truncation"
	"github.com/latebit-io/nib/engine/event"
)

// multiTurnProvider serves pre-canned stream events for each successive
// Stream call. Each element of turns is the full list of events returned
// from one call to Stream. Used to drive the agent through a scripted
// conversation in tests.
type multiTurnProvider struct {
	mu         sync.Mutex
	turns      [][]llm.StreamEvent
	call       int
	toolInputs []llm.Message // records the tool-role messages passed back in
}

func (m *multiTurnProvider) Stream(_ context.Context, messages []llm.Message, _ []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, msg := range messages {
		if msg.Role == "tool" {
			m.toolInputs = append(m.toolInputs, msg)
		}
	}
	if m.call >= len(m.turns) {
		panic(fmt.Sprintf("multiTurnProvider: no more turns scripted (call %d, have %d)", m.call, len(m.turns)))
	}
	events := m.turns[m.call]
	m.call++
	ch := make(chan llm.StreamEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

// drainUntil reads from ch until it receives the first event matching the
// predicate or the timeout fires. Returns the matching event or nil on timeout.
// All other events are discarded so the agent does not block on a full channel.
// FlushBuffers events are auto-resolved with an empty result — in production
// the frontend handles these; in tests we unblock the agent immediately.
func drainUntil(t *testing.T, ch <-chan event.Event, timeout time.Duration, match func(event.Event) bool) event.Event {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-ch:
			if fb, ok := ev.(event.FlushBuffers); ok {
				fb.Result <- event.FlushResult{}
				continue
			}
			if match(ev) {
				return ev
			}
		case <-deadline:
			return nil
		}
	}
}

// TestAgent_RequestInputUnregisteredAcrossModes verifies the
// request_input tool is NOT registered in either Headless or
// Interactive mode, in either the tools map or the LLM-facing
// definitions slice. The tool was deliberately removed because the
// LLM was using it as a workaround for tool-surface friction
// ("which strategy should I pick?") rather than for genuine goal
// ambiguity. If a legitimate use case re-emerges, re-register
// behind a feature flag — never as an always-on default.
func TestAgent_RequestInputUnregisteredAcrossModes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		mode InteractionMode
	}{
		{"headless", Headless},
		{"interactive", Interactive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := make(chan event.Event, 8)
			ag := New(&multiTurnProvider{}, stubWorkspace{}, events,
				&NewOptions{Interaction: tc.mode})

			if _, ok := ag.tools["request_input"]; ok {
				t.Errorf("request_input registered in %s mode; want absent", tc.name)
			}
			for _, def := range ag.toolDefs {
				if def.Function.Name == "request_input" {
					t.Errorf("request_input advertised in %s tool defs; want absent", tc.name)
				}
			}
		})
	}
}

// escalatingProvider wraps multiTurnProvider and also implements
// truncation.Escalator, so the agent-loop escalation path fires during the
// truncation test. Embedding preserves the scripted-stream Stream() method.
type escalatingProvider struct {
	*multiTurnProvider
	maxTokens int
}

func (p *escalatingProvider) MaxTokens() int     { return p.maxTokens }
func (p *escalatingProvider) SetMaxTokens(v int) { p.maxTokens = v }

func TestAgent_TruncatedOutput_EscalatesMaxTokens(t *testing.T) {
	inner := &multiTurnProvider{
		turns: [][]llm.StreamEvent{
			// Turn 1: truncated tool call — should trigger escalation.
			{
				{
					ToolCalls: []llm.ToolCall{{
						ID:       "call-trunc",
						Type:     "function",
						Function: llm.FunctionCall{Name: "edit_file", Arguments: `{"path":"x"`},
					}},
					Done:      true,
					Truncated: true,
				},
			},
			// Turn 2: plain answer, turn ends.
			{
				{Token: "ok retrying smaller"},
				{Done: true},
			},
		},
	}
	provider := &escalatingProvider{multiTurnProvider: inner}

	events := make(chan event.Event, 64)
	ag := New(provider, stubWorkspace{}, events, nil)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ag.RunWithMode(ctx, "main.go", "", "go", nil, ModeExecution)

	if drainUntil(t, events, 2*time.Second, func(ev event.Event) bool {
		_, ok := ev.(event.AgentWaiting)
		return ok
	}) == nil {
		t.Fatal("timeout waiting for AgentWaiting after truncated turn")
	}

	// The provider's max_tokens should have been bumped to the initial
	// escalation value (started at 0 → unset → jumps to the floor).
	if provider.MaxTokens() != truncation.InitialEscalation {
		t.Errorf("MaxTokens after escalation = %d, want %d",
			provider.MaxTokens(), truncation.InitialEscalation)
	}
}

func TestAgent_TruncatedOutput_AbortsAfterRetryLimit(t *testing.T) {
	// A misbehaving model (or one already at the output-token ceiling) that
	// keeps returning Truncated=true must not loop forever — the agent
	// abandons the turn after truncation.MaxRetries consecutive truncations.
	truncatedTurn := []llm.StreamEvent{
		{
			ToolCalls: []llm.ToolCall{{
				ID:       "call-trunc",
				Type:     "function",
				Function: llm.FunctionCall{Name: "edit_file", Arguments: `{"path":"x"`},
			}},
			Done:      true,
			Truncated: true,
		},
	}
	turns := make([][]llm.StreamEvent, 0, truncation.MaxRetries+2)
	for i := 0; i < truncation.MaxRetries+2; i++ {
		turns = append(turns, truncatedTurn)
	}
	provider := &multiTurnProvider{turns: turns}

	events := make(chan event.Event, 64)
	ag := New(provider, stubWorkspace{}, events, nil)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ag.RunWithMode(ctx, "main.go", "", "go", nil, ModeExecution)

	// After retry exhaustion the OnTruncated hook returns Retry=false
	// and the foundation ends the run via AgentEnd. The translator
	// emits AgentDone(success=false) directly — no intervening
	// AgentWaiting park (the inline-loop "park on error" path is gone
	// post-cutover; errors unwind cleanly through AgentDone).
	done := drainUntil(t, events, 2*time.Second, func(ev event.Event) bool {
		_, ok := ev.(event.AgentDone)
		return ok
	})
	if done == nil {
		t.Fatal("timeout waiting for AgentDone after truncation retry exhaustion")
	}
	if done.(event.AgentDone).Success {
		t.Errorf("AgentDone.Success = true on truncation abort, want false")
	}

	// The provider should have been called exactly truncation.MaxRetries+1
	// times — retries capped, no infinite loop.
	provider.mu.Lock()
	wantCalls := truncation.MaxRetries + 1
	if provider.call != wantCalls {
		t.Errorf("provider calls = %d, want %d", provider.call, wantCalls)
	}
	provider.mu.Unlock()

	// Every assistant message with ToolCalls must have a matching tool-role
	// reply in the saved transcript — including the final abort attempt.
	// Without this, Resume from savedMessages would send a malformed
	// request (dangling tool_calls) that the provider rejects on validation.
	ag.mu.Lock()
	saved := ag.savedMessages
	ag.mu.Unlock()

	toolCallsEmitted := 0
	toolRepliesSeen := 0
	for _, msg := range saved {
		if msg.Role == "assistant" {
			toolCallsEmitted += len(msg.ToolCalls)
		}
		if msg.Role == "tool" && msg.ToolCallID == "call-trunc" {
			toolRepliesSeen++
		}
	}
	if toolCallsEmitted != toolRepliesSeen {
		t.Errorf("dangling tool_calls in saved transcript: %d assistant ToolCalls, %d tool-role replies",
			toolCallsEmitted, toolRepliesSeen)
	}
}

func TestAgent_StreamClosedBeforeDone_SurfacesError(t *testing.T) {
	// When the provider closes its stream without ever emitting Done (e.g.
	// mid-stream connection drop, SSE parse error), the turn must end in a
	// visible error — NOT a silent AgentWaiting with partial content.
	provider := &multiTurnProvider{
		turns: [][]llm.StreamEvent{
			// Turn 1: tokens flow, then the channel closes without Done.
			{
				{Token: "partial "},
				{Token: "response"},
			},
		},
	}

	events := make(chan event.Event, 64)
	ag := New(provider, stubWorkspace{}, events, nil)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ag.RunWithMode(ctx, "main.go", "", "go", nil, ModeExecution)

	// The agent must emit an AgentError describing the stream failure —
	// never reach AgentWaiting treating the partial tokens as a clean turn.
	errEv := drainUntil(t, events, 2*time.Second, func(ev event.Event) bool {
		_, ok := ev.(event.AgentError)
		return ok
	})
	if errEv == nil {
		t.Fatal("expected AgentError for stream closed before Done")
	}
	msg := errEv.(event.AgentError).Err
	if !strings.Contains(msg, "stream") {
		t.Errorf("AgentError = %q, want substring 'stream'", msg)
	}
}

func TestAgent_TruncatedOutput_RejectsToolCalls(t *testing.T) {
	// When the provider reports Truncated=true on the final stream event,
	// the agent must not execute the accumulated tool calls — their
	// arguments may have been cut off mid-generation and silently applying
	// them would corrupt files. It should instead reply to each tool call
	// with an error and loop so the LLM can retry smaller.
	provider := &multiTurnProvider{
		turns: [][]llm.StreamEvent{
			// Turn 1: LLM emits a tool call AND the stream is flagged
			// truncated (hit max output tokens mid-generation).
			{
				{
					ToolCalls: []llm.ToolCall{{
						ID:   "call-trunc",
						Type: "function",
						Function: llm.FunctionCall{
							Name:      "edit_file",
							Arguments: `{"path":"x.lua","search":"foo\n","replace":"bar mid-stream cutoff with no newline",`,
						},
					}},
					Done:      true,
					Truncated: true,
				},
			},
			// Turn 2: LLM recovers with a plain text answer, no tool calls.
			{
				{Token: "sorry, retrying smaller."},
				{Done: true},
			},
		},
	}

	events := make(chan event.Event, 64)
	ag := New(provider, stubWorkspace{}, events, nil)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ag.RunWithMode(ctx, "main.go", "", "go", nil, ModeExecution)

	// Wait for the turn to end.
	if drainUntil(t, events, 2*time.Second, func(ev event.Event) bool {
		_, ok := ev.(event.AgentWaiting)
		return ok
	}) == nil {
		t.Fatal("timeout waiting for AgentWaiting after truncated turn")
	}

	// Verify turn 2 saw the rejection message (not a real edit_file result).
	provider.mu.Lock()
	defer provider.mu.Unlock()
	var found bool
	for _, msg := range provider.toolInputs {
		if msg.ToolCallID == "call-trunc" && strings.Contains(msg.Content, "truncated") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("truncation rejection not delivered to LLM for call-trunc; got %+v", provider.toolInputs)
	}
}
