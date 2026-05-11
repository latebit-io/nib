package kit_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	agentevent "github.com/latebit-io/nib/agent/event"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/event"
)

// scriptedProvider serves up a queue of pre-built stream responses,
// one per Stream call. Sufficient for facade-level tests; the agent
// foundation has deeper provider-behavior coverage.
type scriptedProvider struct {
	mu    sync.Mutex
	turns [][]llm.StreamEvent
}

func newScriptedProvider(turns ...[]llm.StreamEvent) *scriptedProvider {
	return &scriptedProvider{turns: turns}
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

// errorProvider returns an error from Stream.
type errorProvider struct{ err error }

func (p errorProvider) Stream(_ context.Context, _ []llm.Message, _ []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	return nil, p.err
}

// blockingProvider blocks Stream until ctx cancellation. Used to keep
// a run alive long enough to observe Cancel semantics deterministically.
type blockingProvider struct{}

func (blockingProvider) Stream(ctx context.Context, _ []llm.Message, _ []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

func streamText(chunks []string, usage *llm.Usage) []llm.StreamEvent {
	out := make([]llm.StreamEvent, 0, len(chunks)+1)
	for _, c := range chunks {
		out = append(out, llm.StreamEvent{Token: c})
	}
	out = append(out, llm.StreamEvent{Done: true, Usage: usage})
	return out
}

// streamWithToolCall returns a stream whose terminal Done event carries
// a single tool call with the given id, name, and JSON-arg blob.
func streamWithToolCall(id, name, args string) []llm.StreamEvent {
	return []llm.StreamEvent{{
		Done: true,
		ToolCalls: []llm.ToolCall{{
			ID:       id,
			Function: llm.FunctionCall{Name: name, Arguments: args},
		}},
	}}
}

// drainUntil reads from ch until pred returns true on a received event
// or the 2s deadline expires. Returns every event drained including the
// matching one.
func drainUntil(ch <-chan event.Event, pred func(event.Event) bool) []event.Event {
	out := make([]event.Event, 0, 8)
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
			if pred(ev) {
				return out
			}
		case <-deadline:
			return out
		}
	}
}

func untilDone(ev event.Event) bool {
	_, ok := ev.(event.AgentDone)
	return ok
}

// subscribeEvents subscribes a default-policy [kit.Subscription] to a
// and returns its inbox channel; the subscription is auto-Closed via
// [t.Cleanup]. Replaces the legacy pattern of allocating a channel and
// passing it through `Config.Events`.
//
// Default options preserve the pre-bus drop-streaming-block-control
// behavior: a buffered inbox (64 slots) absorbs streaming bursts;
// control events block the publisher if the test stops draining.
// Override via the variadic SubscribeOptions for tests that need a
// smaller buffer or different policy.
func subscribeEvents(t *testing.T, a *kit.Agent, opts ...kit.SubscribeOptions) <-chan event.Event {
	t.Helper()
	o := kit.SubscribeOptions{}
	if len(opts) > 0 {
		o = opts[0]
	}
	sub, err := a.Subscribe(o)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Close)
	return sub.Events()
}

// nopTool implements [kit.Tool] with a no-op execute. Used only to
// drive the facade — semantic tool tests live in the foundation.
type nopTool struct {
	name string
}

func (t nopTool) Definition() llm.ToolDef {
	return llm.ToolDef{Function: llm.FunctionDef{Name: t.name}}
}

func (nopTool) Execute(_ context.Context, _ llm.ToolCall) kit.ToolResult {
	return kit.ToolResult{Content: "ok"}
}

// brokenTool returns an empty tool name — exercises the foundation's
// validation propagation through kit.New.
type brokenTool struct{}

func (brokenTool) Definition() llm.ToolDef                              { return llm.ToolDef{} }
func (brokenTool) Execute(context.Context, llm.ToolCall) kit.ToolResult { return kit.ToolResult{} }

func TestNew_RejectsNilProvider(t *testing.T) {
	t.Parallel()
	_, err := kit.New(kit.Config{})
	if !errors.Is(err, kit.ErrInvalidOptions) {
		t.Fatalf("want ErrInvalidOptions, got %v", err)
	}
}

func TestNew_PropagatesFoundationValidation(t *testing.T) {
	t.Parallel()
	_, err := kit.New(kit.Config{
		Provider: errorProvider{err: errors.New("x")},
		Toolset:  kit.Toolset{Tools: []kit.Tool{brokenTool{}}},
	})
	if !errors.Is(err, kit.ErrInvalidOptions) {
		t.Fatalf("want ErrInvalidOptions, got %v", err)
	}
}

func TestPrompt_StreamsTokensAndEndsSuccess(t *testing.T) {
	t.Parallel()

	provider := newScriptedProvider(streamText([]string{"Hello", " world"}, &llm.Usage{
		PromptTokens:     10,
		CompletionTokens: 2,
	}))
	a, err := kit.New(kit.Config{Provider: provider})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	events := subscribeEvents(t, a)

	if err := a.Prompt(context.Background(), "hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// Drain everything the run emits up to AgentTurnUsage so we can
	// observe the streaming events. Then Cancel to unwind from the
	// awaitReply park; AgentDone arrives with Success=false.
	got := drainUntil(events, func(ev event.Event) bool {
		_, ok := ev.(event.AgentTurnUsage)
		return ok
	})
	a.Cancel()
	a.WaitForIdle()
	got = append(got, drainUntil(events, untilDone)...)
	if len(got) == 0 {
		t.Fatalf("no events drained")
	}

	// Streaming text surfaced as AgentToken.
	var sawToken bool
	var concat string
	for _, ev := range got {
		if tok, ok := ev.(event.AgentToken); ok {
			sawToken = true
			concat += tok.Text
		}
	}
	if !sawToken {
		t.Fatalf("no AgentToken in %d events", len(got))
	}
	if concat != "Hello world" {
		t.Fatalf("token concat = %q, want %q", concat, "Hello world")
	}

	// AgentTurnUsage carries the provider counts.
	var sawUsage bool
	for _, ev := range got {
		if u, ok := ev.(event.AgentTurnUsage); ok {
			sawUsage = true
			if u.PromptTokens != 10 || u.CompletionTokens != 2 {
				t.Fatalf("usage = %+v, want PromptTokens=10 CompletionTokens=2", u)
			}
		}
	}
	if !sawUsage {
		t.Fatalf("no AgentTurnUsage in events")
	}

	// Cancel flips success=false.
	done, _ := lastDone(got)
	if done.Success {
		t.Fatalf("AgentDone.Success=true after Cancel, want false")
	}
}

// TestPrompt_CleanRunDoneSuccessTrue drives a run that completes
// cleanly via [AfterToolCallResult.Terminate] and asserts the resulting
// [event.AgentDone] reports Success=true. This is the only path that
// produces a clean foundation AgentEnd without external Cancel or
// stream error, so it's the only way to observe the success=true
// branch of the kit translator.
func TestPrompt_CleanRunDoneSuccessTrue(t *testing.T) {
	t.Parallel()

	provider := newScriptedProvider(
		streamWithToolCall("call-1", "echo", `{"text":"hi"}`),
	)
	tool := nopTool{name: "echo"}
	hooks := kit.Hooks{
		AfterToolCall: func(_ context.Context, _ kit.AfterToolCallContext) (kit.AfterToolCallResult, error) {
			return kit.AfterToolCallResult{Terminate: true}, nil
		},
	}
	a, err := kit.New(kit.Config{
		Provider: provider,
		Toolset:  kit.Toolset{Tools: []kit.Tool{tool}, Hooks: hooks},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	events := subscribeEvents(t, a)

	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	a.WaitForIdle()

	got := drainUntil(events, untilDone)
	done, ok := lastDone(got)
	if !ok {
		t.Fatalf("no AgentDone in events")
	}
	if !done.Success {
		t.Fatalf("AgentDone.Success=false on clean run, want true")
	}
}

// TestPark_TranslatesAgentParkedToAgentWaitingInStreamOrder verifies
// the kit translator emits [event.AgentWaiting] in stream order with
// [event.AgentToken] when the foundation emits [agentevent.AgentParked].
// This is the architectural fix for the historical race where the
// AgentWaiting send originated from a coding hook on the foundation
// goroutine and could overtake AgentTokens still queued in the
// translator's foundationEvents buffer.
func TestPark_TranslatesAgentParkedToAgentWaitingInStreamOrder(t *testing.T) {
	t.Parallel()

	provider := newScriptedProvider(streamText([]string{"a", "b", "c"}, nil))

	hooks := kit.Hooks{
		BeforePark: func(_ context.Context) (agentevent.AgentParked, error) {
			return agentevent.AgentParked{Finished: true}, nil
		},
	}

	a, err := kit.New(kit.Config{Provider: provider, Toolset: kit.Toolset{Hooks: hooks}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	events := subscribeEvents(t, a, kit.SubscribeOptions{BufferSize: 64})

	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	var (
		tokens   []string
		sawWait  bool
		waitInfo event.AgentWaiting
	)
	deadline := time.After(2 * time.Second)
	for !sawWait {
		select {
		case ev := <-events:
			switch e := ev.(type) {
			case event.AgentToken:
				tokens = append(tokens, e.Text)
			case event.AgentWaiting:
				sawWait = true
				waitInfo = e
			}
		case <-deadline:
			t.Fatalf("timed out before AgentWaiting; tokens=%v", tokens)
		}
	}

	a.Cancel()

	if !waitInfo.Finished {
		t.Errorf("AgentWaiting.Finished = false; want true (translator must propagate Finished from AgentParked)")
	}
	if len(tokens) != 3 {
		t.Errorf("AgentWaiting overtook AgentTokens: saw %d/3 tokens before AgentWaiting (race regression)", len(tokens))
	}
}

func TestStreamError_EmitsAgentErrorAndUnsuccessfulDone(t *testing.T) {
	t.Parallel()

	provider := errorProvider{err: errors.New("boom")}
	a, err := kit.New(kit.Config{Provider: provider})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	events := subscribeEvents(t, a)

	if err := a.Prompt(context.Background(), "hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	a.WaitForIdle()

	got := drainUntil(events, untilDone)

	var sawErr bool
	for _, ev := range got {
		if e, ok := ev.(event.AgentError); ok {
			sawErr = true
			if e.Err == "" {
				t.Fatalf("AgentError.Err is empty")
			}
		}
	}
	if !sawErr {
		t.Fatalf("no AgentError in events")
	}

	done, ok := lastDone(got)
	if !ok {
		t.Fatalf("no AgentDone in events")
	}
	if done.Success {
		t.Fatalf("AgentDone.Success=true after Stream error, want false")
	}
}

// TestUnsuccessful_ResetBetweenRuns exercises the contract that
// [Agent.Prompt] clears the unsuccess flag set by a prior run. First
// run errors mid-stream → flag set → AgentDone(false). Second run
// terminates cleanly via Terminate hook; without the reset, the
// translator would still emit AgentDone(false) on the second run.
func TestUnsuccessful_ResetBetweenRuns(t *testing.T) {
	t.Parallel()

	provider := &swappableProvider{p: errorProvider{err: errors.New("first")}}
	hooks := kit.Hooks{
		AfterToolCall: func(_ context.Context, _ kit.AfterToolCallContext) (kit.AfterToolCallResult, error) {
			return kit.AfterToolCallResult{Terminate: true}, nil
		},
	}
	a, err := kit.New(kit.Config{
		Provider: provider,
		Toolset:  kit.Toolset{Tools: []kit.Tool{nopTool{name: "echo"}}, Hooks: hooks},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	events := subscribeEvents(t, a)

	if err := a.Prompt(context.Background(), "first"); err != nil {
		t.Fatalf("Prompt(first): %v", err)
	}
	a.WaitForIdle()
	first := drainUntil(events, untilDone)
	if d, _ := lastDone(first); d.Success {
		t.Fatalf("first AgentDone.Success=true, want false")
	}

	provider.swap(newScriptedProvider(
		streamWithToolCall("call-1", "echo", `{"text":"hi"}`),
	))

	if err := a.Prompt(context.Background(), "second"); err != nil {
		t.Fatalf("Prompt(second): %v", err)
	}
	a.WaitForIdle()

	second := drainUntil(events, untilDone)
	d, ok := lastDone(second)
	if !ok {
		t.Fatalf("no AgentDone on second run")
	}
	if !d.Success {
		t.Fatalf("second AgentDone.Success=false, want true (reset failed)")
	}
}

// TestSend_ControlEventsBlockNotDrop guards the contract that
// terminal/control events are guaranteed delivery — never silently
// dropped to a timeout. A slow consumer (here, an unbuffered channel
// drained after a deliberate delay) must still receive AgentError
// and AgentDone. The previous 5-second-timeout-then-drop policy
// would have silently lost both signals under sustained
// backpressure; the new blocking semantics block until the consumer
// catches up.
func TestSend_ControlEventsBlockNotDrop(t *testing.T) {
	t.Parallel()

	provider := errorProvider{err: errors.New("boom")}

	a, err := kit.New(kit.Config{Provider: provider})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	// Tiny buffer so the second control event must back-pressure the
	// publisher under the default Block policy. With BufferSize=1
	// AgentError fits in the slot; AgentDone publish blocks until
	// the consumer drains. If the bus dropped control events on full
	// (the regression this test guards against) AgentDone would
	// silently disappear.
	events := subscribeEvents(t, a, kit.SubscribeOptions{BufferSize: 1})

	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// Do not drain immediately. With the previous 5s drop policy a
	// shorter sleep here would have left the test passing accidentally
	// (events arrive within the timeout); a longer sleep would have
	// dropped them. The new blocking policy delivers regardless of
	// how long this sleep is — pick something well past any
	// reasonable scheduler delay.
	time.Sleep(100 * time.Millisecond)

	var sawErr, sawDone bool
	deadline := time.After(2 * time.Second)
	for !sawDone {
		select {
		case ev := <-events:
			switch ev.(type) {
			case event.AgentError:
				sawErr = true
			case event.AgentDone:
				sawDone = true
			}
		case <-deadline:
			t.Fatalf("timeout waiting for AgentDone (sawErr=%v sawDone=%v)", sawErr, sawDone)
		}
	}
	if !sawErr {
		t.Fatalf("AgentError not delivered to slow consumer")
	}
}

// TestUnsuccessful_NoCrossRunPoisoning exercises the race CodeRabbit
// flagged: WaitForIdle returns when the foundation goroutine exits,
// but the previous run's AgentEnd may still be in the foundation
// events buffer awaiting translation. A fast next Prompt that resets
// shared state on the user's goroutine would race the translator's
// read of that state on the previous run's AgentEnd, mislabeling
// run 1's success as the next run's prelude.
//
// Per-run outcome state in [Agent] eliminates the race: each run's
// failure bit is bound to its own [runOutcome] so the next run's
// allocation cannot poison the previous run's reading.
//
// Test shape: run 1 errors, immediately Prompt run 2 (no drain
// between), then drain both AgentDones. Run 1 must report
// Success=false; run 2 must report Success=true.
func TestUnsuccessful_NoCrossRunPoisoning(t *testing.T) {
	t.Parallel()

	provider := &swappableProvider{p: errorProvider{err: errors.New("first")}}
	hooks := kit.Hooks{
		AfterToolCall: func(_ context.Context, _ kit.AfterToolCallContext) (kit.AfterToolCallResult, error) {
			return kit.AfterToolCallResult{Terminate: true}, nil
		},
	}
	a, err := kit.New(kit.Config{
		Provider: provider,
		Toolset:  kit.Toolset{Tools: []kit.Tool{nopTool{name: "echo"}}, Hooks: hooks},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	events := subscribeEvents(t, a, kit.SubscribeOptions{BufferSize: 64})

	if err := a.Prompt(context.Background(), "first"); err != nil {
		t.Fatalf("Prompt(first): %v", err)
	}
	a.WaitForIdle()

	provider.swap(newScriptedProvider(
		streamWithToolCall("call-1", "echo", `{"text":"hi"}`),
	))

	// Crucial: no drain between runs. Prompt(second) immediately
	// after WaitForIdle of first stresses the per-run binding.
	// foundation.Prompt may return ErrRunInProgress if the
	// foundation hasn't fully released the running flag yet — retry
	// briefly to handle that without serializing through a drain
	// (which would defeat the test's purpose).
	deadline := time.After(2 * time.Second)
	for {
		err := a.Prompt(context.Background(), "second")
		if err == nil {
			break
		}
		if !errors.Is(err, kit.ErrRunInProgress) {
			t.Fatalf("Prompt(second): %v", err)
		}
		select {
		case <-deadline:
			t.Fatalf("Prompt(second) stuck on ErrRunInProgress past deadline")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	a.WaitForIdle()

	// Now drain both AgentDones and verify each run's success bit
	// reflects that run, not the other.
	var dones []event.AgentDone
	drainUntil(events, func(ev event.Event) bool {
		if d, ok := ev.(event.AgentDone); ok {
			dones = append(dones, d)
			return len(dones) == 2
		}
		return false
	})

	if len(dones) < 2 {
		t.Fatalf("got %d AgentDone events, want 2", len(dones))
	}
	if dones[0].Success {
		t.Fatalf("first AgentDone.Success=true, want false (run errored)")
	}
	if !dones[1].Success {
		t.Fatalf("second AgentDone.Success=false, want true (run completed cleanly)")
	}
}

func TestErrRunInProgress(t *testing.T) {
	t.Parallel()

	provider := blockingProvider{}
	a, err := kit.New(kit.Config{Provider: provider})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	events := subscribeEvents(t, a)

	if err := a.Prompt(context.Background(), "first"); err != nil {
		t.Fatalf("Prompt(first): %v", err)
	}
	defer func() {
		a.Cancel()
		a.WaitForIdle()
		drainUntil(events, untilDone)
	}()

	if err := a.Prompt(context.Background(), "second"); !errors.Is(err, kit.ErrRunInProgress) {
		t.Fatalf("want ErrRunInProgress, got %v", err)
	}
}

func TestState_DelegatesToFoundation(t *testing.T) {
	t.Parallel()

	provider := newScriptedProvider(streamText([]string{"hi"}, nil))
	a, err := kit.New(kit.Config{
		Provider:     provider,
		SystemPrompt: "you are a test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	events := subscribeEvents(t, a)

	if err := a.Prompt(context.Background(), "hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	defer func() {
		a.Cancel()
		a.WaitForIdle()
		drainUntil(events, untilDone)
	}()

	// Wait for the loop to start so the transcript is populated.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-events:
			if _, ok := ev.(event.AgentToken); ok {
				goto Started
			}
		case <-deadline:
			t.Fatalf("no AgentToken before deadline")
		}
	}
Started:
	st := a.State()
	if len(st.Messages) < 2 {
		t.Fatalf("State.Messages has %d entries, want >= 2 (system + user)", len(st.Messages))
	}
	if st.Messages[0].Role != "system" || st.Messages[0].Content != "you are a test" {
		t.Fatalf("State.Messages[0] = %+v, want system 'you are a test'", st.Messages[0])
	}
}

func TestEventTranslation_TurnUsageFieldByField(t *testing.T) {
	t.Parallel()

	usage := &llm.Usage{
		PromptTokens:     100,
		CompletionTokens: 50,
		CachedTokens:     20,
	}
	provider := newScriptedProvider(streamText([]string{"hi"}, usage))
	a, err := kit.New(kit.Config{Provider: provider})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	events := subscribeEvents(t, a)

	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	defer func() {
		a.Cancel()
		a.WaitForIdle()
		drainUntil(events, untilDone)
	}()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-events:
			if u, ok := ev.(event.AgentTurnUsage); ok {
				if u.PromptTokens != 100 || u.CompletionTokens != 50 || u.CachedTokens != 20 {
					t.Fatalf("AgentTurnUsage = %+v, want PromptTokens=100 CompletionTokens=50 CachedTokens=20", u)
				}
				return
			}
		case <-deadline:
			t.Fatalf("no AgentTurnUsage before deadline")
		}
	}
}

func TestFoundationObservability_NotInConsumerStream(t *testing.T) {
	t.Parallel()

	provider := newScriptedProvider(streamText([]string{"hi"}, nil))
	a, err := kit.New(kit.Config{Provider: provider})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	events := subscribeEvents(t, a)

	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	a.Cancel()
	a.WaitForIdle()

	got := drainUntil(events, untilDone)

	// Every event must be a kit/event type. The package-level type
	// switch in this assertion documents the kit consumer surface.
	for _, ev := range got {
		switch ev.(type) {
		case event.AgentToken, event.AgentTurnUsage, event.AgentInputEstimate,
			event.AgentCompacted, event.AgentError, event.AgentDone,
			event.AgentToolCall, event.AgentWaiting, event.AgentStatus:
			// allowed — every kit/event type a Phase 2 facade can produce
			// or that an application hook may have emitted directly.
		default:
			t.Fatalf("unexpected event type %T leaked from foundation: %+v", ev, ev)
		}
	}
}

func TestClose_StopsTranslatorGoroutine(t *testing.T) {
	t.Parallel()

	provider := newScriptedProvider(streamText([]string{"hi"}, nil))
	a, err := kit.New(kit.Config{Provider: provider})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	events := subscribeEvents(t, a)

	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// Drain pending events on a background goroutine so Close's
	// translator-drain phase has somewhere to send. The drainer
	// returns once an AgentDone arrives — same as steady-state.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		drainUntil(events, untilDone)
	}()

	a.Close()
	<-drained

	// Observable contract: after Close returns, the translator has
	// fully exited, the bus has closed every subscription's inbox,
	// and reader loops exit naturally. The range below sees a closed
	// channel and returns immediately; if Close had returned early
	// (before bus.close finished) the channel would still be open
	// and the range would block until a 200ms watchdog fired.
	closedSeen := make(chan struct{})
	go func() {
		defer close(closedSeen)
		for range events {
		}
	}()
	select {
	case <-closedSeen:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("subscription inbox not closed after Close — bus.close did not run or Close returned early")
	}
}

func TestClose_Idempotent(t *testing.T) {
	t.Parallel()

	provider := newScriptedProvider(streamText([]string{"hi"}, nil))
	a, err := kit.New(kit.Config{Provider: provider})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	events := subscribeEvents(t, a)

	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// First Close shuts down. Second Close must not panic on
	// double-close of the done channel — sync.Once protects that.
	a.Close()
	a.Close()

	drainUntil(events, untilDone)
}

func TestClose_DeliversFinalAgentDone(t *testing.T) {
	t.Parallel()

	provider := blockingProvider{}
	a, err := kit.New(kit.Config{Provider: provider})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	events := subscribeEvents(t, a)

	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// Close while the run is in flight. The translator's drain path
	// must still surface AgentDone — without the post-Close drain,
	// an early select on done would drop the final AgentEnd from
	// the buffered foundation channel.
	a.Close()

	got := drainUntil(events, untilDone)
	d, ok := lastDone(got)
	if !ok {
		t.Fatalf("no AgentDone after Close")
	}
	// Close calls Cancel, which sets the unsuccess flag.
	if d.Success {
		t.Fatalf("AgentDone.Success=true after Close, want false (Close cancels)")
	}
}

func TestPrompt_AfterCloseReturnsErrClosed(t *testing.T) {
	t.Parallel()

	provider := newScriptedProvider(streamText([]string{"hi"}, nil))
	a, err := kit.New(kit.Config{Provider: provider})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	events := subscribeEvents(t, a)

	a.Close()
	drainUntil(events, untilDone)

	if err := a.Prompt(context.Background(), "after-close"); !errors.Is(err, kit.ErrClosed) {
		t.Fatalf("Prompt after Close: want ErrClosed, got %v", err)
	}
	if err := a.PromptWithMessages(context.Background(), []llm.Message{{Role: "user", Content: "x"}}); !errors.Is(err, kit.ErrClosed) {
		t.Fatalf("PromptWithMessages after Close: want ErrClosed, got %v", err)
	}
}

// lastDone returns the final AgentDone in evs, if any.
func lastDone(evs []event.Event) (event.AgentDone, bool) {
	var (
		d  event.AgentDone
		ok bool
	)
	for _, ev := range evs {
		if x, isDone := ev.(event.AgentDone); isDone {
			d = x
			ok = true
		}
	}
	return d, ok
}

// TestSubscribeProbeObservesAlongsideConsumer (P9) verifies the
// concrete use case the bus migration enables: a kit-level test
// attaches a probe subscriber to observe the agent's event stream
// without owning the consumer position. Both subscribers see every
// event in publish order; neither blocks the other.
//
// Pre-bus this was impossible — there was one Events channel; whoever
// held it was the consumer. After the bus, every subscriber gets an
// independent inbox, and the bus is the sole writer to each.
func TestSubscribeProbeObservesAlongsideConsumer(t *testing.T) {
	t.Parallel()

	provider := newScriptedProvider(streamText([]string{"Hi", " there"}, &llm.Usage{
		PromptTokens:     5,
		CompletionTokens: 2,
	}))

	a, err := kit.New(kit.Config{Provider: provider})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	// Real consumer: the headless-style drain path.
	consumer := subscribeEvents(t, a)
	// Probe: observes the same stream without claiming the consumer
	// position. A future audit logger / metrics emitter / test probe
	// is the same shape.
	probe := subscribeEvents(t, a)

	if err := a.Prompt(context.Background(), "hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	consumerSeq := drainUntil(consumer, untilDone)
	a.Cancel()
	a.WaitForIdle()
	consumerSeq = append(consumerSeq, drainUntil(consumer, untilDone)...)
	probeSeq := drainUntil(probe, untilDone)
	probeSeq = append(probeSeq, drainUntil(probe, untilDone)...)

	// Both subscribers must see at least one AgentToken with text and a
	// terminal AgentDone — proof that both received the full stream.
	consumerTok, consumerDone := pickTokenAndDone(consumerSeq)
	probeTok, probeDone := pickTokenAndDone(probeSeq)

	if consumerTok == "" || probeTok == "" {
		t.Fatalf("missing tokens — consumer=%q probe=%q", consumerTok, probeTok)
	}
	if !consumerDone || !probeDone {
		t.Fatalf("missing AgentDone — consumer=%v probe=%v", consumerDone, probeDone)
	}
	// Both subscribers see the same streamed text in the same order.
	if consumerTok != probeTok {
		t.Errorf("token streams diverged — consumer=%q probe=%q", consumerTok, probeTok)
	}
}

// pickTokenAndDone returns the concatenated AgentToken text and a flag
// indicating whether an AgentDone was observed. Helper for the P9 test.
func pickTokenAndDone(evs []event.Event) (string, bool) {
	var (
		text string
		done bool
	)
	for _, ev := range evs {
		switch e := ev.(type) {
		case event.AgentToken:
			text += e.Text
		case event.AgentDone:
			done = true
		}
	}
	return text, done
}

// swappableProvider lets a test swap the underlying provider mid-test.
// Required for the unsuccess-reset test: first run uses an error
// provider, second run uses a happy-path provider.
type swappableProvider struct {
	mu sync.Mutex
	p  llm.Provider
}

func (s *swappableProvider) swap(p llm.Provider) {
	s.mu.Lock()
	s.p = p
	s.mu.Unlock()
}

func (s *swappableProvider) Stream(ctx context.Context, msgs []llm.Message, defs []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	s.mu.Lock()
	p := s.p
	s.mu.Unlock()
	if p == nil {
		return nil, fmt.Errorf("no provider")
	}
	return p.Stream(ctx, msgs, defs)
}
