package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/llm"
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

func TestAgent_RequestInput_RoundTrip(t *testing.T) {
	provider := &multiTurnProvider{
		turns: [][]llm.StreamEvent{
			// Turn 1: LLM asks via request_input.
			{
				{
					ToolCalls: []llm.ToolCall{{
						ID:   "call-ri",
						Type: "function",
						Function: llm.FunctionCall{
							Name: "request_input",
							Arguments: `{
								"prompt": "Fix all or only new?",
								"options": [
									{"id":"fix-all","label":"Fix every violation"},
									{"id":"fix-new","label":"Only fix new ones"}
								]
							}`,
						},
					}},
					Done: true,
				},
			},
			// Turn 2: LLM receives the answer, emits no tool calls, turn ends.
			{
				{Token: "ok, fixing all."},
				{Done: true},
			},
		},
	}

	events := make(chan event.Event, 64)
	ag := New(provider, stubWorkspace{}, events, nil)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ag.RunWithMode(ctx, "main.go", "", "go", nil, ModeExecution)

	// Drain events until we see AgentAwaitingInput.
	awaitEv := drainUntil(t, events, 2*time.Second, func(ev event.Event) bool {
		_, ok := ev.(event.AgentAwaitingInput)
		return ok
	})
	if awaitEv == nil {
		t.Fatal("timeout waiting for AgentAwaitingInput")
	}
	ae := awaitEv.(event.AgentAwaitingInput)
	if ae.Prompt != "Fix all or only new?" {
		t.Errorf("Prompt = %q", ae.Prompt)
	}
	if ae.CallID != "call-ri" {
		t.Errorf("CallID = %q, want call-ri", ae.CallID)
	}
	if len(ae.Options) != 2 || ae.Options[0].ID != "fix-all" {
		t.Errorf("Options = %+v", ae.Options)
	}

	// Developer answers with the first option.
	ag.AnswerInput("fix-all")

	// Drain until turn 2 ends with AgentWaiting (turn-end signal).
	waitEv := drainUntil(t, events, 2*time.Second, func(ev event.Event) bool {
		_, ok := ev.(event.AgentWaiting)
		return ok
	})
	if waitEv == nil {
		t.Fatal("timeout waiting for AgentWaiting after answer")
	}

	// Verify the answer was delivered to the LLM as a tool-role message
	// tied to the request_input call ID.
	provider.mu.Lock()
	defer provider.mu.Unlock()
	var found bool
	for _, msg := range provider.toolInputs {
		// The agent appends an intent reminder to tool results, so the
		// content starts with the raw answer but isn't byte-equal.
		if msg.ToolCallID == "call-ri" && strings.HasPrefix(msg.Content, "fix-all") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("answer not delivered as tool message for call-ri; got %+v", provider.toolInputs)
	}
}

// escalatingProvider wraps multiTurnProvider and also implements
// maxTokensEscalator, so the agent-loop escalation path fires during the
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
	if provider.MaxTokens() != maxTokensInitialEscalation {
		t.Errorf("MaxTokens after escalation = %d, want %d",
			provider.MaxTokens(), maxTokensInitialEscalation)
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

func TestAgent_RequestInput_CancelDuringAwait(t *testing.T) {
	provider := &multiTurnProvider{
		turns: [][]llm.StreamEvent{
			{
				{
					ToolCalls: []llm.ToolCall{{
						ID:   "call-ri",
						Type: "function",
						Function: llm.FunctionCall{
							Name:      "request_input",
							Arguments: `{"prompt":"pick"}`,
						},
					}},
					Done: true,
				},
			},
		},
	}

	events := make(chan event.Event, 64)
	ag := New(provider, stubWorkspace{}, events, nil)

	ctx, cancel := context.WithCancel(context.Background())
	ag.RunWithMode(ctx, "main.go", "", "go", nil, ModeExecution)

	// Wait for the prompt to arrive, then cancel.
	awaitEv := drainUntil(t, events, 2*time.Second, func(ev event.Event) bool {
		_, ok := ev.(event.AgentAwaitingInput)
		return ok
	})
	if awaitEv == nil {
		t.Fatal("timeout waiting for AgentAwaitingInput")
	}
	cancel()

	// The agent run must end (AgentDone with Success=false).
	doneEv := drainUntil(t, events, 2*time.Second, func(ev event.Event) bool {
		_, ok := ev.(event.AgentDone)
		return ok
	})
	if doneEv == nil {
		t.Fatal("timeout waiting for AgentDone after cancel")
	}
	if doneEv.(event.AgentDone).Success {
		t.Error("AgentDone.Success should be false after cancel")
	}
}

func TestAgent_AnswerInput_NoPendingIsNoOp(t *testing.T) {
	// Same pattern as Approve/Reject/Continue: calling AnswerInput with
	// no pending prompt must not block or panic — the answer is dropped.
	events := make(chan event.Event, 8)
	ag := New(&multiTurnProvider{}, stubWorkspace{}, events, nil)

	done := make(chan struct{})
	go func() {
		ag.AnswerInput("anything")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("AnswerInput blocked with no pending prompt")
	}
}

func TestAgent_HeadlessMode_RequestInputNotRegistered(t *testing.T) {
	events := make(chan event.Event, 8)
	ag := New(&multiTurnProvider{}, stubWorkspace{}, events, &NewOptions{Interaction: Headless})

	// Tool registry must not contain request_input in headless mode.
	if _, ok := ag.tools["request_input"]; ok {
		t.Error("request_input should not be registered in headless mode")
	}
	// Tool definitions advertised to the LLM must not include it either.
	for _, def := range ag.toolDefs {
		if def.Function.Name == "request_input" {
			t.Error("request_input should not be advertised in headless tool defs")
		}
	}
}

func TestAgent_InteractiveMode_RequestInputIsRegistered(t *testing.T) {
	events := make(chan event.Event, 8)
	ag := New(&multiTurnProvider{}, stubWorkspace{}, events, &NewOptions{Interaction: Interactive})

	if _, ok := ag.tools["request_input"]; !ok {
		t.Error("request_input must be registered in interactive mode")
	}
	var advertised bool
	for _, def := range ag.toolDefs {
		if def.Function.Name == "request_input" {
			advertised = true
			break
		}
	}
	if !advertised {
		t.Error("request_input must be advertised in interactive tool defs")
	}
}

func TestAgent_RequestInput_AnswerDrainedAcrossRuns(t *testing.T) {
	// Stale answers from a previous run must not leak into a fresh run's
	// awaitingInputCh. RunWithMode drains the channel at start.
	provider := &multiTurnProvider{
		turns: [][]llm.StreamEvent{
			{
				{
					ToolCalls: []llm.ToolCall{{
						ID:       "call-ri",
						Type:     "function",
						Function: llm.FunctionCall{Name: "request_input", Arguments: `{"prompt":"pick"}`},
					}},
					Done: true,
				},
			},
			{{Done: true}}, // turn 2 after answer
		},
	}
	events := make(chan event.Event, 64)
	ag := New(provider, stubWorkspace{}, events, nil)

	// Prime the channel with a stale answer, then start a run. If the
	// drain didn't happen, the stale answer would satisfy the new prompt
	// and the test would observe a non-empty tool input for the new run.
	ag.awaitingInputCh <- "STALE"

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ag.RunWithMode(ctx, "main.go", "", "go", nil, ModeExecution)
	awaitEv := drainUntil(t, events, 2*time.Second, func(ev event.Event) bool {
		_, ok := ev.(event.AgentAwaitingInput)
		return ok
	})
	if awaitEv == nil {
		t.Fatal("timeout waiting for AgentAwaitingInput in fresh run")
	}
	// The stale STALE was drained; provide a fresh answer.
	ag.AnswerInput("fresh")
	waitEv := drainUntil(t, events, 2*time.Second, func(ev event.Event) bool {
		_, ok := ev.(event.AgentWaiting)
		return ok
	})
	if waitEv == nil {
		t.Fatal("timeout waiting for AgentWaiting")
	}

	provider.mu.Lock()
	defer provider.mu.Unlock()
	for _, msg := range provider.toolInputs {
		if strings.HasPrefix(msg.Content, "STALE") {
			t.Fatal("stale answer leaked into new run — drain failed")
		}
	}
}
