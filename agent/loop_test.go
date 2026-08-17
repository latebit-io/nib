package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/nib/agent/event"
	"github.com/latebit-io/nib/ai/llm"
)

// scriptedProvider serves up a queue of pre-built stream responses,
// one per Stream call. Tests construct the queue via [scripted] turns
// so each turn is a sequence of StreamEvents terminated by a Done
// event. Drains the queue in FIFO order; an empty queue produces an
// empty stream that closes immediately, exercising the
// errProviderClosedEarly path when needed.
type scriptedProvider struct {
	mu    sync.Mutex
	turns [][]llm.StreamEvent
	calls int
}

func newScriptedProvider(turns ...[]llm.StreamEvent) *scriptedProvider {
	return &scriptedProvider{turns: turns}
}

func (p *scriptedProvider) Stream(_ context.Context, _ []llm.Message, _ []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	p.mu.Lock()
	p.calls++
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

// errorProvider returns the configured error from Stream. Used to
// verify the Stream-error path emits [event.Error] and ends the run.
type errorProvider struct{ err error }

func (p errorProvider) Stream(_ context.Context, _ []llm.Message, _ []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	return nil, p.err
}

// recordingTool captures every call dispatched to it and returns a
// configurable result. Tests inspect Calls() to assert the agent
// dispatched the expected calls; the result lets a single test
// substitute success/failure.
type recordingTool struct {
	def    llm.ToolDef
	result ToolResult

	mu    sync.Mutex
	calls []llm.ToolCall
}

func (t *recordingTool) Definition() llm.ToolDef { return t.def }
func (t *recordingTool) Execute(_ context.Context, c llm.ToolCall) ToolResult {
	t.mu.Lock()
	t.calls = append(t.calls, c)
	t.mu.Unlock()
	return t.result
}
func (t *recordingTool) Calls() []llm.ToolCall {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]llm.ToolCall, len(t.calls))
	copy(out, t.calls)
	return out
}

// streamDone is shorthand for a single-event "no content, no tool
// calls, just done" stream — what an LLM would emit when it has
// nothing more to say and the loop should park.
func streamDone() []llm.StreamEvent {
	return []llm.StreamEvent{{Done: true}}
}

// streamText returns a stream that emits content chunks then a clean
// terminal Done event with usage. Helper keeps test data declarative.
func streamText(chunks []string, usage *llm.Usage) []llm.StreamEvent {
	out := make([]llm.StreamEvent, 0, len(chunks)+1)
	for _, c := range chunks {
		out = append(out, llm.StreamEvent{Token: c})
	}
	out = append(out, llm.StreamEvent{Done: true, Usage: usage})
	return out
}

// streamWithToolCalls returns a stream whose Done event carries tool
// calls. The caller-supplied ToolCalls slice is attached to the
// terminal event verbatim.
func streamWithToolCalls(calls []llm.ToolCall) []llm.StreamEvent {
	return []llm.StreamEvent{{Done: true, ToolCalls: calls}}
}

// drainEvents reads at most max events with a 1s deadline. Returns
// the slice consumed plus a flag indicating whether the deadline
// expired. Tests use this when the agent has already emitted
// AgentEnd; without a deadline a missing event would hang the test.
func drainEvents(ch <-chan event.Event, want int) []event.Event {
	out := make([]event.Event, 0, want)
	deadline := time.After(2 * time.Second)
	for len(out) < want {
		select {
		case ev := <-ch:
			out = append(out, ev)
		case <-deadline:
			return out
		}
	}
	return out
}

// drainUntilEnd reads events until [event.AgentEnd] is observed (or a
// 2s deadline expires). Returns every event drained, including the
// terminal AgentEnd. Tests use this for happy-path runs where the
// final transcript is the assertion target.
func drainUntilEnd(ch <-chan event.Event) []event.Event {
	out := make([]event.Event, 0, 16)
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
			if _, ok := ev.(event.AgentEnd); ok {
				return out
			}
		case <-deadline:
			return out
		}
	}
}

// findEvent returns the first event of type T from evs. The boolean
// return distinguishes "missing" from "zero value" — important for
// types like AgentEnd whose zero value is a valid (empty transcript)
// observation.
func findEvent[T event.Event](evs []event.Event) (T, bool) {
	var zero T
	for _, ev := range evs {
		if t, ok := ev.(T); ok {
			return t, true
		}
	}
	return zero, false
}

// countEvents returns how many events of type T appear in evs.
func countEvents[T event.Event](evs []event.Event) int {
	var n int
	for _, ev := range evs {
		if _, ok := ev.(T); ok {
			n++
		}
	}
	return n
}

// TestPrompt_TerminatesOnNoToolCalls covers the simplest happy path:
// a single turn with no tool calls. The agent emits AgentStart,
// MessageStart/Update/End, TurnStart/End, parks awaiting reply, and
// closes out via Abort. WaitForIdle returns once AgentEnd has fired.
func TestPrompt_TerminatesOnNoToolCalls(t *testing.T) {
	t.Parallel()

	provider := newScriptedProvider(streamText([]string{"Hello", " world"}, &llm.Usage{
		PromptTokens:     10,
		CompletionTokens: 2,
	}))
	events := make(chan event.Event, 32)

	a, err := New(Options{Provider: provider, Events: events})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := a.Prompt(context.Background(), "hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// Wait until the loop has consumed the turn and is parked. The
	// MessageEnd marks the end of streaming for the turn.
	collected := make([]event.Event, 0, 8)
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-events:
			collected = append(collected, ev)
			if _, ok := ev.(event.TurnEnd); ok {
				goto Parked
			}
		case <-deadline:
			t.Fatalf("timed out before TurnEnd: got %d events", len(collected))
		}
	}
Parked:

	// Loop should be parked awaiting reply: not streaming, transcript
	// has system+user+assistant.
	state := a.State()
	if state.Streaming {
		t.Errorf("State.Streaming = true; want false after MessageEnd")
	}
	if got := len(state.Messages); got != 2 {
		t.Errorf("State.Messages length = %d; want 2 (user, assistant)", got)
	}

	a.Abort()
	a.WaitForIdle()

	// Drain any trailing events (should include AgentEnd).
	rest := drainEvents(events, 4)
	all := append(collected, rest...)

	if _, ok := findEvent[event.AgentStart](all); !ok {
		t.Errorf("missing AgentStart")
	}
	if _, ok := findEvent[event.MessageStart](all); !ok {
		t.Errorf("missing MessageStart")
	}
	updates := countEvents[event.MessageUpdate](all)
	if updates != 2 {
		t.Errorf("MessageUpdate count = %d; want 2", updates)
	}
	end, ok := findEvent[event.MessageEnd](all)
	if !ok {
		t.Fatalf("missing MessageEnd")
	}
	if end.Message.Content != "Hello world" {
		t.Errorf("MessageEnd.Content = %q; want %q", end.Message.Content, "Hello world")
	}
	usage, ok := findEvent[event.TurnUsage](all)
	if !ok {
		t.Fatalf("missing TurnUsage")
	}
	if usage.PromptTokens != 10 || usage.CompletionTokens != 2 {
		t.Errorf("TurnUsage = %+v; want PromptTokens=10 CompletionTokens=2", usage)
	}
	if _, ok := findEvent[event.AgentEnd](all); !ok {
		t.Errorf("missing AgentEnd")
	}
}

// TestPrompt_CarriesReasoningTrace verifies the foundation copies a Done
// event's Reasoning onto the assembled assistant message AND into the
// persisted transcript, so a provider's reasoning trace round-trips
// through history (required for extended-thinking replay).
func TestPrompt_CarriesReasoningTrace(t *testing.T) {
	t.Parallel()

	trace := &llm.ReasoningTrace{Blocks: []llm.ReasoningBlock{
		{Type: "thinking", Text: "let me think", Signature: "sig-abc"},
	}}
	provider := newScriptedProvider([]llm.StreamEvent{
		{Token: "answer"},
		{Done: true, Reasoning: trace, Usage: &llm.Usage{PromptTokens: 5, CompletionTokens: 1}},
	})
	events := make(chan event.Event, 32)

	a, err := New(Options{Provider: provider, Events: events})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Prompt(context.Background(), "hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	collected := make([]event.Event, 0, 8)
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-events:
			collected = append(collected, ev)
			if _, ok := ev.(event.TurnEnd); ok {
				goto Parked
			}
		case <-deadline:
			t.Fatalf("timed out before TurnEnd")
		}
	}
Parked:
	a.Abort()
	a.WaitForIdle()
	all := append(collected, drainEvents(events, 4)...)

	end, ok := findEvent[event.MessageEnd](all)
	if !ok {
		t.Fatal("missing MessageEnd")
	}
	if end.Message.Reasoning != trace {
		t.Errorf("MessageEnd.Message.Reasoning = %v; want the verbatim trace pointer", end.Message.Reasoning)
	}

	// The trace must live on the persisted assistant turn so it survives
	// into the next request's history.
	msgs := a.State().Messages
	last := msgs[len(msgs)-1]
	if last.Role != "assistant" || last.Reasoning == nil || len(last.Reasoning.Blocks) != 1 {
		t.Fatalf("persisted assistant message missing reasoning: %+v", last)
	}
	if last.Reasoning.Blocks[0].Signature != "sig-abc" {
		t.Errorf("reasoning signature not preserved in history: %q", last.Reasoning.Blocks[0].Signature)
	}
}

// TestPrompt_RejectsConcurrentRun verifies Prompt fails fast when a
// run is already active.
func TestPrompt_RejectsConcurrentRun(t *testing.T) {
	t.Parallel()

	provider := newScriptedProvider(streamDone())
	events := make(chan event.Event, 32)

	a, err := New(Options{Provider: provider, Events: events})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := a.Prompt(context.Background(), "first"); err != nil {
		t.Fatalf("first Prompt: %v", err)
	}
	defer func() {
		a.Abort()
		a.WaitForIdle()
		// Drain remaining events.
		for {
			select {
			case <-events:
			default:
				return
			}
		}
	}()

	err = a.Prompt(context.Background(), "second")
	if !errors.Is(err, ErrRunInProgress) {
		t.Errorf("second Prompt err = %v; want ErrRunInProgress", err)
	}
}

// TestToolDispatch_RoutesCallsAndAppendsResult drives a turn whose
// terminal event includes a tool call. The agent must route to the
// recordingTool, append the result as a tool-role message, and emit
// ToolStart + ToolEnd.
func TestToolDispatch_RoutesCallsAndAppendsResult(t *testing.T) {
	t.Parallel()

	tool := &recordingTool{
		def: llm.ToolDef{
			Type:     "function",
			Function: llm.FunctionDef{Name: "echo"},
		},
		result: ToolResult{Content: "echoed"},
	}

	turn1 := streamWithToolCalls([]llm.ToolCall{{
		ID:   "call_1",
		Type: "function",
		Function: llm.FunctionCall{
			Name:      "echo",
			Arguments: `{"text":"hi"}`,
		},
	}})
	turn2 := streamDone() // post-tool turn, no more calls

	provider := newScriptedProvider(turn1, turn2)
	events := make(chan event.Event, 64)

	a, err := New(Options{
		Provider: provider,
		Events:   events,
		Tools:    []Tool{tool},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// Wait until the second turn parks (TurnEnd seen twice).
	turnEnds := 0
	collected := make([]event.Event, 0, 16)
	deadline := time.After(2 * time.Second)
	for turnEnds < 2 {
		select {
		case ev := <-events:
			collected = append(collected, ev)
			if _, ok := ev.(event.TurnEnd); ok {
				turnEnds++
			}
		case <-deadline:
			t.Fatalf("timed out: turnEnds=%d events=%d", turnEnds, len(collected))
		}
	}

	a.Abort()
	a.WaitForIdle()

	if calls := tool.Calls(); len(calls) != 1 {
		t.Fatalf("tool.Calls() len = %d; want 1", len(calls))
	} else if calls[0].ID != "call_1" {
		t.Errorf("tool.Calls()[0].ID = %q; want call_1", calls[0].ID)
	}

	if start, ok := findEvent[event.ToolStart](collected); !ok {
		t.Errorf("missing ToolStart")
	} else if start.Name != "echo" || start.CallID != "call_1" {
		t.Errorf("ToolStart = %+v; want Name=echo CallID=call_1", start)
	}
	if end, ok := findEvent[event.ToolEnd](collected); !ok {
		t.Errorf("missing ToolEnd")
	} else if end.Result != "echoed" || end.IsError {
		t.Errorf("ToolEnd = %+v; want Result=echoed IsError=false", end)
	}

	state := a.State()
	// system absent (no SystemPrompt), then user, assistant(toolcall),
	// tool, assistant(empty)  → 4 messages.
	if got := len(state.Messages); got != 4 {
		t.Errorf("State.Messages length = %d; want 4", got)
	}
	if state.Messages[2].Role != "tool" || state.Messages[2].Content != "echoed" {
		t.Errorf("messages[2] = %+v; want role=tool content=echoed", state.Messages[2])
	}
}

// TestBeforeToolCall_BlocksExecution verifies the BeforeToolCall hook
// can short-circuit a tool call: Execute is NEVER invoked, and the
// synthesized error result feeds back to the LLM in place.
func TestBeforeToolCall_BlocksExecution(t *testing.T) {
	t.Parallel()

	tool := &recordingTool{
		def: llm.ToolDef{
			Type:     "function",
			Function: llm.FunctionDef{Name: "edit_file"},
		},
		result: ToolResult{Content: "should not appear"},
	}

	turn1 := streamWithToolCalls([]llm.ToolCall{{
		ID:       "call_1",
		Function: llm.FunctionCall{Name: "edit_file", Arguments: "{}"},
	}})
	turn2 := streamDone()

	provider := newScriptedProvider(turn1, turn2)
	events := make(chan event.Event, 64)

	hookCalls := 0
	hooks := Hooks{
		BeforeToolCall: func(_ context.Context, c BeforeToolCallInput) (BeforeToolCallResult, error) {
			hookCalls++
			if c.Name != "edit_file" {
				return BeforeToolCallResult{}, nil
			}
			return BeforeToolCallResult{Block: true, Reason: "no edits in test"}, nil
		},
	}

	a, err := New(Options{Provider: provider, Events: events, Tools: []Tool{tool}, Hooks: hooks})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	turnEnds := 0
	collected := make([]event.Event, 0, 16)
	deadline := time.After(2 * time.Second)
	for turnEnds < 2 {
		select {
		case ev := <-events:
			collected = append(collected, ev)
			if _, ok := ev.(event.TurnEnd); ok {
				turnEnds++
			}
		case <-deadline:
			t.Fatalf("timed out: turnEnds=%d", turnEnds)
		}
	}

	a.Abort()
	a.WaitForIdle()

	if hookCalls != 1 {
		t.Errorf("hookCalls = %d; want 1", hookCalls)
	}
	if calls := tool.Calls(); len(calls) != 0 {
		t.Errorf("tool.Calls() = %d; want 0 (block should skip Execute)", len(calls))
	}
	end, ok := findEvent[event.ToolEnd](collected)
	if !ok {
		t.Fatalf("missing ToolEnd")
	}
	if !end.IsError || end.Result != "no edits in test" {
		t.Errorf("ToolEnd = %+v; want IsError=true Result=%q", end, "no edits in test")
	}
}

// TestAfterToolCall_OverridesAndTerminates verifies AfterToolCall
// overrides Content/IsError and the all-terminate case ends the run
// after the batch.
func TestAfterToolCall_OverridesAndTerminates(t *testing.T) {
	t.Parallel()

	tool := &recordingTool{
		def: llm.ToolDef{
			Type:     "function",
			Function: llm.FunctionDef{Name: "noop"},
		},
		result: ToolResult{Content: "raw"},
	}

	turn1 := streamWithToolCalls([]llm.ToolCall{{
		ID:       "c1",
		Function: llm.FunctionCall{Name: "noop", Arguments: "{}"},
	}})

	provider := newScriptedProvider(turn1)
	events := make(chan event.Event, 64)

	overridden := "overridden"
	isErr := true
	hooks := Hooks{
		AfterToolCall: func(_ context.Context, _ AfterToolCallInput) (AfterToolCallResult, error) {
			return AfterToolCallResult{
				Content:   &overridden,
				IsError:   &isErr,
				Terminate: true,
			}, nil
		},
	}

	a, err := New(Options{Provider: provider, Events: events, Tools: []Tool{tool}, Hooks: hooks})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	all := drainUntilEnd(events)
	a.WaitForIdle()

	end, ok := findEvent[event.ToolEnd](all)
	if !ok {
		t.Fatalf("missing ToolEnd")
	}
	if end.Result != "overridden" || !end.IsError {
		t.Errorf("ToolEnd = %+v; want Result=overridden IsError=true", end)
	}
	if _, ok := findEvent[event.AgentEnd](all); !ok {
		t.Errorf("missing AgentEnd (Terminate should end the run)")
	}
	// Provider must have been called exactly once — terminate after
	// the first batch means no second turn.
	if provider.calls != 1 {
		t.Errorf("provider.calls = %d; want 1", provider.calls)
	}
}

// TestTransformContext_RewritesMessages confirms the hook sees a copy
// of the transcript and can return a rewritten slice that is passed to
// the provider.
func TestTransformContext_RewritesMessages(t *testing.T) {
	t.Parallel()

	provider := newScriptedProvider(streamDone())
	events := make(chan event.Event, 32)

	var sawMessages []llm.Message
	hooks := Hooks{
		TransformContext: func(_ context.Context, msgs []llm.Message) ([]llm.Message, error) {
			sawMessages = msgs
			out := append([]llm.Message{}, msgs...)
			out = append(out, llm.Message{Role: "user", Content: "injected"})
			return out, nil
		},
	}

	a, err := New(Options{Provider: provider, Events: events, Hooks: hooks})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Prompt(context.Background(), "hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// Wait for first TurnEnd so we know TransformContext fired.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-events:
			if _, ok := ev.(event.TurnEnd); ok {
				goto Done
			}
		case <-deadline:
			t.Fatal("timed out waiting for TurnEnd")
		}
	}
Done:

	a.Abort()
	a.WaitForIdle()

	if len(sawMessages) != 1 || sawMessages[0].Content != "hi" {
		t.Errorf("TransformContext saw %+v; want [{user hi}]", sawMessages)
	}
	// Mutating the snapshot must not affect the live transcript.
	state := a.State()
	if len(state.Messages) != 2 {
		// 1 user (initial prompt) + 1 assistant (empty done turn) —
		// the injected message goes to the provider but is NOT
		// persisted into the live transcript (TransformContext is
		// per-call, not a transcript edit).
		t.Errorf("State.Messages length = %d; want 2", len(state.Messages))
	}
}

// TestSteeringMessages_ReentersLoop verifies that a non-empty
// steering response on a no-tool-calls turn causes the loop to invoke
// the provider a second time without parking on Reply.
func TestSteeringMessages_ReentersLoop(t *testing.T) {
	t.Parallel()

	provider := newScriptedProvider(streamDone(), streamDone())
	events := make(chan event.Event, 64)

	calls := 0
	hooks := Hooks{
		SteeringMessages: func(_ context.Context) ([]llm.Message, error) {
			calls++
			if calls == 1 {
				return []llm.Message{{Role: "user", Content: "steer"}}, nil
			}
			return nil, nil
		},
	}

	a, err := New(Options{Provider: provider, Events: events, Hooks: hooks})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// Wait for two TurnEnds — confirms the steering re-entry fired.
	turnEnds := 0
	deadline := time.After(2 * time.Second)
	for turnEnds < 2 {
		select {
		case ev := <-events:
			if _, ok := ev.(event.TurnEnd); ok {
				turnEnds++
			}
		case <-deadline:
			t.Fatalf("timed out: turnEnds=%d", turnEnds)
		}
	}

	a.Abort()
	a.WaitForIdle()

	if provider.calls != 2 {
		t.Errorf("provider.calls = %d; want 2 (steering should re-enter loop)", provider.calls)
	}
	if calls != 2 {
		t.Errorf("SteeringMessages calls = %d; want 2 (one to inject, one to drain)", calls)
	}
}

// TestBeforePark_EmitsAgentParkedInStreamOrder verifies that the
// foundation calls BeforePark immediately before parking and emits the
// returned [event.AgentParked] through the same event pipeline as
// every other foundation event. The event must arrive after the
// turn's MessageEnd / TurnEnd (proves it goes through the queue, not
// a side channel) and must carry the hook's Finished bit.
func TestBeforePark_EmitsAgentParkedInStreamOrder(t *testing.T) {
	t.Parallel()

	provider := newScriptedProvider(streamText([]string{"hello"}, nil))
	events := make(chan event.Event, 64)

	hookCalls := 0
	hooks := Hooks{
		BeforePark: func(_ context.Context) (event.AgentParked, error) {
			hookCalls++
			return event.AgentParked{Finished: true}, nil
		},
	}

	a, err := New(Options{Provider: provider, Events: events, Hooks: hooks})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	var (
		sawTurnEnd  bool
		sawParked   bool
		parkedAfter bool
	)
	deadline := time.After(2 * time.Second)
	for !sawParked {
		select {
		case ev := <-events:
			switch e := ev.(type) {
			case event.TurnEnd:
				sawTurnEnd = true
			case event.AgentParked:
				sawParked = true
				parkedAfter = sawTurnEnd
				if !e.Finished {
					t.Errorf("AgentParked.Finished = false; want true (hook returned true)")
				}
			}
		case <-deadline:
			t.Fatalf("timed out before AgentParked: sawTurnEnd=%v hookCalls=%d", sawTurnEnd, hookCalls)
		}
	}

	a.Abort()
	a.WaitForIdle()

	if !parkedAfter {
		t.Errorf("AgentParked arrived before TurnEnd; ordering violated")
	}
	if hookCalls != 1 {
		t.Errorf("BeforePark called %d times; want exactly 1", hookCalls)
	}
}

// TestBeforePark_NilHookEmitsZeroValue verifies that an unset hook
// still produces an AgentParked event (zero value); kit consumers can
// rely on AgentWaiting firing for every park, hook or not.
func TestBeforePark_NilHookEmitsZeroValue(t *testing.T) {
	t.Parallel()

	provider := newScriptedProvider(streamDone())
	events := make(chan event.Event, 64)

	a, err := New(Options{Provider: provider, Events: events})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-events:
			if p, ok := ev.(event.AgentParked); ok {
				if p.Finished {
					t.Errorf("AgentParked.Finished = true with no hook; want zero value")
				}
				a.Abort()
				a.WaitForIdle()
				return
			}
		case <-deadline:
			t.Fatalf("timed out without seeing AgentParked")
		}
	}
}

// TestReply_DeliversBetweenTurns parks the loop on awaitReply, sends
// a Reply, and confirms the loop consumes it as a user message in the
// next turn.
func TestReply_DeliversBetweenTurns(t *testing.T) {
	t.Parallel()

	provider := newScriptedProvider(streamDone(), streamDone())
	events := make(chan event.Event, 64)

	a, err := New(Options{Provider: provider, Events: events})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Prompt(context.Background(), "first"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// Wait for the first TurnEnd so we know the loop is parked.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-events:
			if _, ok := ev.(event.TurnEnd); ok {
				goto Parked
			}
		case <-deadline:
			t.Fatal("timed out waiting for first TurnEnd")
		}
	}
Parked:

	// Reply may need a brief moment to land if the loop is between
	// the TurnEnd send and awaitReply read. Loop with a short
	// timeout to avoid a flake.
	deadline = time.After(time.Second)
	for !a.Reply(context.Background(), "second") {
		select {
		case <-time.After(5 * time.Millisecond):
		case <-deadline:
			t.Fatal("Reply never accepted")
		}
	}

	// Wait for second TurnEnd.
	deadline = time.After(2 * time.Second)
	for {
		select {
		case ev := <-events:
			if _, ok := ev.(event.TurnEnd); ok {
				goto SecondDone
			}
		case <-deadline:
			t.Fatal("timed out waiting for second TurnEnd")
		}
	}
SecondDone:

	a.Abort()
	a.WaitForIdle()

	if provider.calls != 2 {
		t.Errorf("provider.calls = %d; want 2", provider.calls)
	}
	state := a.State()
	// user(first) + assistant(empty) + user(second) + assistant(empty) → 4
	if got := len(state.Messages); got != 4 {
		t.Errorf("State.Messages length = %d; want 4", got)
	}
	if state.Messages[2].Role != "user" || state.Messages[2].Content != "second" {
		t.Errorf("messages[2] = %+v; want user/second", state.Messages[2])
	}
}

// TestReply_RejectsWhenNotRunning ensures Reply on an idle agent
// returns false rather than blocking.
func TestReply_RejectsWhenNotRunning(t *testing.T) {
	t.Parallel()

	provider := newScriptedProvider()
	events := make(chan event.Event, 1)
	a, err := New(Options{Provider: provider, Events: events})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.Reply(context.Background(), "noop") {
		t.Error("Reply on idle agent returned true; want false")
	}
}

// TestAbort_UnwindsRun confirms Abort cancels the run ctx and that
// WaitForIdle returns once the goroutine exits.
func TestAbort_UnwindsRun(t *testing.T) {
	t.Parallel()

	provider := newScriptedProvider(streamDone())
	events := make(chan event.Event, 32)

	a, err := New(Options{Provider: provider, Events: events})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	a.Abort()

	doneCh := make(chan struct{})
	go func() {
		a.WaitForIdle()
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForIdle did not return after Abort")
	}

	// Drain events to confirm AgentEnd fired.
	all := drainEvents(events, 16)
	if _, ok := findEvent[event.AgentEnd](all); !ok {
		t.Errorf("missing AgentEnd after Abort")
	}
}

// TestStreamError_EmitsErrorEvent verifies a Stream-level error from
// the provider is surfaced through [event.Error] and ends the run.
func TestStreamError_EmitsErrorEvent(t *testing.T) {
	t.Parallel()

	provider := errorProvider{err: errors.New("boom")}
	events := make(chan event.Event, 32)

	a, err := New(Options{Provider: provider, Events: events})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	all := drainUntilEnd(events)
	a.WaitForIdle()

	errEv, ok := findEvent[event.Error](all)
	if !ok {
		t.Fatalf("missing Error event")
	}
	if errEv.Err == "" {
		t.Errorf("Error.Err is empty; want a message containing 'boom'")
	}
	state := a.State()
	if state.LastError == "" {
		t.Errorf("State.LastError is empty after stream error")
	}
}

// TestProviderClosedEarly_EmitsErrorEvent covers the path where the
// provider closes its stream channel without a Done event. The
// foundation must surface this as an error rather than silently
// completing the turn with partial content.
func TestProviderClosedEarly_EmitsErrorEvent(t *testing.T) {
	t.Parallel()

	provider := newScriptedProvider([]llm.StreamEvent{{Token: "partial"}}) // no Done
	events := make(chan event.Event, 32)

	a, err := New(Options{Provider: provider, Events: events})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	all := drainUntilEnd(events)
	a.WaitForIdle()

	if _, ok := findEvent[event.Error](all); !ok {
		t.Errorf("missing Error event for early-close path")
	}
	if _, ok := findEvent[event.MessageEnd](all); ok {
		t.Errorf("MessageEnd should NOT fire on early close")
	}
}

// TestTruncated_EndsRunWithError confirms a Truncated terminal event
// is treated as an error — the foundation does not retry.
func TestTruncated_EndsRunWithError(t *testing.T) {
	t.Parallel()

	provider := newScriptedProvider([]llm.StreamEvent{
		{Token: "partial"},
		{Done: true, Truncated: true},
	})
	events := make(chan event.Event, 32)

	a, err := New(Options{Provider: provider, Events: events})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	all := drainUntilEnd(events)
	a.WaitForIdle()

	if _, ok := findEvent[event.Error](all); !ok {
		t.Errorf("missing Error event for truncated stream")
	}
}

// TestOnTruncated_RetrySplicesAndContinues verifies the OnTruncated
// hook drives a recovery turn. Provider scripts a truncated turn 1
// that emits a tool call with partial args, then a clean turn 2. The
// hook returns Retry=true with rejection messages; the loop must
// append them, NOT execute the truncated tool calls, run turn 2, and
// finish with a parked AgentWaiting/TurnEnd. The truncated assistant
// message itself ends up in the transcript so the rejection messages
// pair with their tool_call IDs (chat-completion format).
func TestOnTruncated_RetrySplicesAndContinues(t *testing.T) {
	t.Parallel()

	tc := llm.ToolCall{
		ID:       "trunc-1",
		Type:     "function",
		Function: llm.FunctionCall{Name: "edit_file", Arguments: `{"path":"x"`},
	}
	provider := newScriptedProvider(
		[]llm.StreamEvent{
			{Done: true, Truncated: true, ToolCalls: []llm.ToolCall{tc}},
		},
		streamText([]string{"recovered"}, nil),
	)
	tool := &recordingTool{def: llm.ToolDef{Function: llm.FunctionDef{Name: "edit_file"}}}

	events := make(chan event.Event, 64)

	var hookCalls int
	hook := func(_ context.Context, c TruncationInput) (TruncationResult, error) {
		hookCalls++
		if len(c.ToolCalls) != 1 || c.ToolCalls[0].ID != "trunc-1" {
			t.Errorf("hook ToolCalls = %+v; want trunc-1 only", c.ToolCalls)
		}
		return TruncationResult{
			Retry: true,
			Messages: []llm.Message{
				{Role: "tool", ToolCallID: "trunc-1", Content: "rejected: truncated"},
			},
		}, nil
	}

	a, err := New(Options{
		Provider: provider,
		Events:   events,
		Tools:    []Tool{tool},
		Hooks:    Hooks{OnTruncated: hook},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	deadline := time.After(2 * time.Second)
	turnEnds := 0
	for turnEnds < 2 {
		select {
		case ev := <-events:
			if _, ok := ev.(event.TurnEnd); ok {
				turnEnds++
			}
		case <-deadline:
			t.Fatalf("timed out waiting for both TurnEnd events; got %d", turnEnds)
		}
	}

	a.Abort()
	a.WaitForIdle()

	if hookCalls != 1 {
		t.Errorf("OnTruncated calls = %d; want 1", hookCalls)
	}
	if got := len(tool.Calls()); got != 0 {
		t.Errorf("tool dispatched %d times; want 0 (truncated tool calls must NEVER execute)", got)
	}

	state := a.State()
	if len(state.Messages) < 4 {
		t.Fatalf("State.Messages length = %d; want >=4 (user, truncated assistant, rejection, recovered assistant)", len(state.Messages))
	}
	// Order: user, truncated assistant, rejection tool, recovered assistant
	if state.Messages[1].Role != "assistant" || len(state.Messages[1].ToolCalls) != 1 {
		t.Errorf("messages[1] = %+v; want truncated assistant with 1 tool call", state.Messages[1])
	}
	if state.Messages[2].Role != "tool" || state.Messages[2].ToolCallID != "trunc-1" {
		t.Errorf("messages[2] = %+v; want tool rejection for trunc-1", state.Messages[2])
	}
	if state.Messages[3].Role != "assistant" || state.Messages[3].Content != "recovered" {
		t.Errorf("messages[3] = %+v; want clean recovered assistant", state.Messages[3])
	}
}

// TestOnTruncated_NoRetryEndsRunSilently locks the contract that a
// hook returning Retry=false ends the run WITHOUT the foundation
// emitting its own [event.Error]. The hook owns user-facing error
// emission; the foundation's emit-on-no-hook fallback would otherwise
// duplicate the application's already-emitted message.
func TestOnTruncated_NoRetryEndsRunSilently(t *testing.T) {
	t.Parallel()

	provider := newScriptedProvider([]llm.StreamEvent{
		{Done: true, Truncated: true},
	})
	events := make(chan event.Event, 32)

	hook := func(_ context.Context, _ TruncationInput) (TruncationResult, error) {
		return TruncationResult{
			Retry: false,
			Messages: []llm.Message{
				{Role: "user", Content: "abandoned: out of retries"},
			},
		}, nil
	}

	a, err := New(Options{
		Provider: provider,
		Events:   events,
		Hooks:    Hooks{OnTruncated: hook},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	all := drainUntilEnd(events)
	a.WaitForIdle()

	if _, ok := findEvent[event.Error](all); ok {
		t.Errorf("foundation emitted Error event; hook owns error emission on Retry=false")
	}
	if _, ok := findEvent[event.AgentEnd](all); !ok {
		t.Errorf("missing AgentEnd; run did not unwind on Retry=false")
	}

	state := a.State()
	// The hook's splice message should land in the final transcript so
	// AgentEnd snapshots include it.
	var found bool
	for _, msg := range state.Messages {
		if msg.Role == "user" && msg.Content == "abandoned: out of retries" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("hook splice not appended to transcript; messages=%+v", state.Messages)
	}
}

// TestOnTruncated_HookErrorIsTerminal verifies a hook returning a
// non-nil error short-circuits the run with [event.Error] (consistent
// with how every other hook handles error returns).
func TestOnTruncated_HookErrorIsTerminal(t *testing.T) {
	t.Parallel()

	provider := newScriptedProvider([]llm.StreamEvent{
		{Done: true, Truncated: true},
	})
	events := make(chan event.Event, 32)

	hookErr := errors.New("recovery rejected the proposal")
	hook := func(_ context.Context, _ TruncationInput) (TruncationResult, error) {
		return TruncationResult{}, hookErr
	}

	a, err := New(Options{
		Provider: provider,
		Events:   events,
		Hooks:    Hooks{OnTruncated: hook},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	all := drainUntilEnd(events)
	a.WaitForIdle()

	gotErr, ok := findEvent[event.Error](all)
	if !ok {
		t.Fatalf("missing Error event for hook failure")
	}
	if !contains(gotErr.Err, "OnTruncated") || !contains(gotErr.Err, hookErr.Error()) {
		t.Errorf("Error.Err = %q; want substrings 'OnTruncated' and %q", gotErr.Err, hookErr.Error())
	}
}

// contains is a tiny helper so the test does not pull strings just for
// substring assertions in error messages.
func contains(s, sub string) bool {
	if sub == "" {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestSystemPrompt_PrependedToTranscript confirms that a non-empty
// SystemPrompt is included as the first message of the run.
func TestSystemPrompt_PrependedToTranscript(t *testing.T) {
	t.Parallel()

	provider := newScriptedProvider(streamDone())
	events := make(chan event.Event, 32)

	a, err := New(Options{
		Provider:     provider,
		Events:       events,
		SystemPrompt: "you are a tester",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-events:
			if _, ok := ev.(event.TurnEnd); ok {
				goto Done
			}
		case <-deadline:
			t.Fatal("timed out waiting for TurnEnd")
		}
	}
Done:

	state := a.State()
	if len(state.Messages) < 2 {
		t.Fatalf("State.Messages length = %d; want >= 2", len(state.Messages))
	}
	if state.Messages[0].Role != "system" || state.Messages[0].Content != "you are a tester" {
		t.Errorf("messages[0] = %+v; want role=system content='you are a tester'", state.Messages[0])
	}

	a.Abort()
	a.WaitForIdle()
}

// TestPromptWithMessages_UsesCallerSuppliedTranscript verifies the new
// entry point bypasses the [system?, user] build and runs with the
// transcript the caller hands in. The caller-side use case is an
// application that needs file content, context files, memory summaries,
// or other prefill in the initial transcript — beyond what
// [Options.SystemPrompt] + a single content string can express.
func TestPromptWithMessages_UsesCallerSuppliedTranscript(t *testing.T) {
	t.Parallel()

	provider := newScriptedProvider(streamDone())
	events := make(chan event.Event, 32)

	a, err := New(Options{Provider: provider, Events: events})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	initial := []llm.Message{
		{Role: "system", Content: "shaped by the application"},
		{Role: "user", Content: "with prefilled context"},
		{Role: "assistant", Content: "and a prior turn"},
		{Role: "user", Content: "do the thing"},
	}
	if err := a.PromptWithMessages(context.Background(), initial); err != nil {
		t.Fatalf("PromptWithMessages: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-events:
			if _, ok := ev.(event.TurnEnd); ok {
				goto Done
			}
		case <-deadline:
			t.Fatal("timed out waiting for TurnEnd")
		}
	}
Done:

	state := a.State()
	// 4 supplied + 1 assistant turn appended by the loop.
	if got := len(state.Messages); got != 5 {
		t.Fatalf("State.Messages length = %d; want 5", got)
	}
	for i, want := range initial {
		if state.Messages[i].Role != want.Role || state.Messages[i].Content != want.Content {
			t.Errorf("messages[%d] = %+v; want %+v", i, state.Messages[i], want)
		}
	}

	a.Abort()
	a.WaitForIdle()
}

// TestPromptWithMessages_DefensiveCopy verifies the agent does not
// share storage with the caller's slice. A caller mutation after the
// call must not leak into the live transcript.
func TestPromptWithMessages_DefensiveCopy(t *testing.T) {
	t.Parallel()

	provider := newScriptedProvider(streamDone())
	events := make(chan event.Event, 32)

	a, err := New(Options{Provider: provider, Events: events})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	initial := []llm.Message{
		{Role: "system", Content: "original system"},
		{Role: "user", Content: "original user"},
	}
	if err := a.PromptWithMessages(context.Background(), initial); err != nil {
		t.Fatalf("PromptWithMessages: %v", err)
	}

	// Mutate the caller's slice while the loop is running. The agent's
	// copy must remain untouched.
	initial[0].Content = "MUTATED"
	initial[1].Content = "MUTATED"

	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-events:
			if _, ok := ev.(event.TurnEnd); ok {
				goto Done
			}
		case <-deadline:
			t.Fatal("timed out waiting for TurnEnd")
		}
	}
Done:

	state := a.State()
	if state.Messages[0].Content != "original system" {
		t.Errorf("messages[0].Content = %q; want %q (caller mutation leaked)", state.Messages[0].Content, "original system")
	}
	if state.Messages[1].Content != "original user" {
		t.Errorf("messages[1].Content = %q; want %q (caller mutation leaked)", state.Messages[1].Content, "original user")
	}

	a.Abort()
	a.WaitForIdle()
}

// TestPromptWithMessages_RejectsEmpty verifies the empty-slice guard.
// A run with zero messages would have nothing to send to the provider
// on the first turn, so the entry point must refuse rather than start
// a useless run that would error on the first Stream call.
func TestPromptWithMessages_RejectsEmpty(t *testing.T) {
	t.Parallel()

	provider := newScriptedProvider(streamDone())
	events := make(chan event.Event, 1)

	a, err := New(Options{Provider: provider, Events: events})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = a.PromptWithMessages(context.Background(), nil)
	if !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("PromptWithMessages(nil) error = %v; want ErrInvalidOptions", err)
	}

	err = a.PromptWithMessages(context.Background(), []llm.Message{})
	if !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("PromptWithMessages(empty) error = %v; want ErrInvalidOptions", err)
	}
}

// TestPromptWithMessages_RejectsConcurrentRun verifies the entry point
// shares the [ErrRunInProgress] guard with [Agent.Prompt].
func TestPromptWithMessages_RejectsConcurrentRun(t *testing.T) {
	t.Parallel()

	provider := newScriptedProvider(streamDone(), streamDone())
	events := make(chan event.Event, 32)

	a, err := New(Options{Provider: provider, Events: events})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := a.PromptWithMessages(context.Background(), []llm.Message{
		{Role: "user", Content: "first"},
	}); err != nil {
		t.Fatalf("first PromptWithMessages: %v", err)
	}
	defer func() {
		a.Abort()
		a.WaitForIdle()
		for {
			select {
			case <-events:
			default:
				return
			}
		}
	}()

	err = a.PromptWithMessages(context.Background(), []llm.Message{
		{Role: "user", Content: "second"},
	})
	if !errors.Is(err, ErrRunInProgress) {
		t.Errorf("second PromptWithMessages error = %v; want ErrRunInProgress", err)
	}
}

// TestMaxTurns_EndsRunWhenCapReached drives a provider that requests a
// tool call on every turn — the ping-pong MaxTurns exists to stop. With
// MaxTurns=2 the loop takes exactly two turns (each with its tool result
// appended), emits MaxTurnsReached{Turns: 2} instead of a third LLM call,
// and ends the run cleanly: no event.Error, no LastError, AgentEnd fires.
func TestMaxTurns_EndsRunWhenCapReached(t *testing.T) {
	t.Parallel()

	call := func(id string) []llm.ToolCall {
		return []llm.ToolCall{{
			ID:       id,
			Type:     "function",
			Function: llm.FunctionCall{Name: "lookup", Arguments: "{}"},
		}}
	}
	// Three scripted tool-call turns queued; the cap must stop the loop
	// after two, leaving the third unconsumed.
	provider := newScriptedProvider(
		streamWithToolCalls(call("c1")),
		streamWithToolCalls(call("c2")),
		streamWithToolCalls(call("c3")),
	)
	tool := &recordingTool{
		def:    llm.ToolDef{Type: "function", Function: llm.FunctionDef{Name: "lookup"}},
		result: ToolResult{Content: "ok"},
	}
	events := make(chan event.Event, 64)

	a, err := New(Options{Provider: provider, Events: events, Tools: []Tool{tool}, MaxTurns: 2})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Prompt(context.Background(), "loop forever"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	a.WaitForIdle()

	all := drainUntilEnd(events)
	reached, ok := findEvent[event.MaxTurnsReached](all)
	if !ok {
		t.Fatalf("missing MaxTurnsReached; events: %#v", all)
	}
	if reached.Turns != 2 {
		t.Errorf("MaxTurnsReached.Turns = %d; want 2", reached.Turns)
	}
	if turns := countEvents[event.TurnStart](all); turns != 2 {
		t.Errorf("TurnStart count = %d; want 2", turns)
	}
	if _, ok := findEvent[event.Error](all); ok {
		t.Errorf("unexpected event.Error — cap stop must not read as failure")
	}
	if _, ok := findEvent[event.AgentEnd](all); !ok {
		t.Errorf("missing AgentEnd")
	}
	if provider.calls != 2 {
		t.Errorf("provider.calls = %d; want 2", provider.calls)
	}
	if got := len(tool.Calls()); got != 2 {
		t.Errorf("tool call count = %d; want 2", got)
	}

	state := a.State()
	if state.LastError != "" {
		t.Errorf("State.LastError = %q; want empty", state.LastError)
	}
	// user + 2×(assistant + tool result) — the transcript is well-formed:
	// every dispatched tool call has its matching tool-role result.
	if got := len(state.Messages); got != 5 {
		t.Errorf("State.Messages length = %d; want 5", got)
	}
	if last := state.Messages[len(state.Messages)-1]; last.Role != "tool" || last.ToolCallID != "c2" {
		t.Errorf("final message = %+v; want tool result for c2", last)
	}
}

// TestMaxTurns_ResetsOnReply verifies a delivered user reply re-arms the
// cap: with MaxTurns=1, the first turn parks, a Reply arrives, and the
// second turn still runs — the counter bounds autonomous turns, not the
// run's total. No MaxTurnsReached is emitted.
func TestMaxTurns_ResetsOnReply(t *testing.T) {
	t.Parallel()

	provider := newScriptedProvider(streamDone(), streamDone())
	events := make(chan event.Event, 64)

	a, err := New(Options{Provider: provider, Events: events, MaxTurns: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Prompt(context.Background(), "first"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	collected := make([]event.Event, 0, 16)
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-events:
			collected = append(collected, ev)
			if _, ok := ev.(event.TurnEnd); ok {
				goto Parked
			}
		case <-deadline:
			t.Fatal("timed out waiting for first TurnEnd")
		}
	}
Parked:

	// Reply may need a brief moment to land if the loop is between the
	// TurnEnd send and awaitReply read (same choreography as
	// TestReply_DeliversBetweenTurns).
	deadline = time.After(time.Second)
	for !a.Reply(context.Background(), "second") {
		select {
		case <-time.After(5 * time.Millisecond):
		case <-deadline:
			t.Fatal("Reply never accepted")
		}
	}

	deadline = time.After(2 * time.Second)
	for {
		select {
		case ev := <-events:
			collected = append(collected, ev)
			if _, ok := ev.(event.TurnEnd); ok {
				goto SecondDone
			}
		case <-deadline:
			t.Fatal("timed out waiting for second TurnEnd")
		}
	}
SecondDone:

	a.Abort()
	a.WaitForIdle()
	all := append(collected, drainEvents(events, 8)...)

	if provider.calls != 2 {
		t.Errorf("provider.calls = %d; want 2 — reply must re-arm the cap", provider.calls)
	}
	if _, ok := findEvent[event.MaxTurnsReached](all); ok {
		t.Errorf("unexpected MaxTurnsReached — cap resets on delivered reply")
	}
}

// TestHookErrorMidBatch_EveryToolUseGetsResult verifies that when a
// hook fails on the second of three tool calls, the run ends with an
// Error event AND the transcript still carries a tool-role result for
// every tool_use in the batch — the failed call and the never-run one
// included. Anthropic rejects resumes with orphaned tool_use blocks.
func TestHookErrorMidBatch_EveryToolUseGetsResult(t *testing.T) {
	t.Parallel()

	tool := &recordingTool{
		def:    llm.ToolDef{Type: "function", Function: llm.FunctionDef{Name: "echo"}},
		result: ToolResult{Content: "ok"},
	}
	calls := []llm.ToolCall{
		{ID: "call_1", Type: "function", Function: llm.FunctionCall{Name: "echo"}},
		{ID: "call_2", Type: "function", Function: llm.FunctionCall{Name: "echo"}},
		{ID: "call_3", Type: "function", Function: llm.FunctionCall{Name: "echo"}},
	}
	provider := newScriptedProvider(streamWithToolCalls(calls))
	events := make(chan event.Event, 64)

	hookErr := errors.New("gate exploded")
	before := func(_ context.Context, in BeforeToolCallInput) (BeforeToolCallResult, error) {
		if in.CallID == "call_2" {
			return BeforeToolCallResult{}, hookErr
		}
		return BeforeToolCallResult{}, nil
	}

	a, err := New(Options{
		Provider: provider,
		Events:   events,
		Tools:    []Tool{tool},
		Hooks:    Hooks{BeforeToolCall: before},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	all := drainUntilEnd(events)
	a.WaitForIdle()

	gotErr, ok := findEvent[event.Error](all)
	if !ok {
		t.Fatalf("missing Error event for hook failure")
	}
	if !contains(gotErr.Err, "BeforeToolCall") || !contains(gotErr.Err, hookErr.Error()) {
		t.Errorf("Error.Err = %q; want substrings 'BeforeToolCall' and %q", gotErr.Err, hookErr.Error())
	}
	if got := tool.Calls(); len(got) != 1 || got[0].ID != "call_1" {
		t.Errorf("tool.Calls() = %+v; want only call_1 executed", got)
	}

	end, ok := findEvent[event.AgentEnd](all)
	if !ok {
		t.Fatalf("missing AgentEnd")
	}
	results := make(map[string]llm.Message)
	for _, m := range end.Messages {
		if m.Role == "tool" {
			results[m.ToolCallID] = m
		}
	}
	for _, c := range calls {
		if _, ok := results[c.ID]; !ok {
			t.Errorf("tool_use %s has no tool_result in final transcript", c.ID)
		}
	}
	if len(results) != len(calls) {
		t.Errorf("tool results = %d; want %d", len(results), len(calls))
	}
	if r := results["call_1"]; r.Content != "ok" {
		t.Errorf("call_1 result = %q; want executed result 'ok'", r.Content)
	}
	for _, id := range []string{"call_2", "call_3"} {
		if r := results[id]; !contains(r.Content, "aborted") {
			t.Errorf("%s result = %q; want synthesized aborted result", id, r.Content)
		}
	}
}
