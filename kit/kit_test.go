package kit_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	agentevent "github.com/latebit-io/nib/agent/event"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/ai/llmtest"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/event"
)

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
		Provider: llmtest.Failing(errors.New("x")),
		Toolset:  kit.Toolset{Tools: []kit.Tool{brokenTool{}}},
	})
	if !errors.Is(err, kit.ErrInvalidOptions) {
		t.Fatalf("want ErrInvalidOptions, got %v", err)
	}
}

func TestNew_RejectsNegativeMaxTurns(t *testing.T) {
	t.Parallel()
	_, err := kit.New(kit.Config{Provider: llmtest.NewScripted(), MaxTurns: -1})
	if !errors.Is(err, kit.ErrInvalidOptions) {
		t.Fatalf("want ErrInvalidOptions, got %v", err)
	}
}

func TestMaxTurns_PublishesAgentMaxTurnsThenSuccessfulDone(t *testing.T) {
	t.Parallel()

	// Three tool-call turns scripted; the cap must stop the run after two.
	provider := llmtest.NewScripted(
		llmtest.ToolCalls(llmtest.Call("c1", "nop", "{}")),
		llmtest.ToolCalls(llmtest.Call("c2", "nop", "{}")),
		llmtest.ToolCalls(llmtest.Call("c3", "nop", "{}")),
	)
	a, err := kit.New(kit.Config{
		Provider: provider,
		Toolset:  kit.Toolset{Tools: []kit.Tool{nopTool{name: "nop"}}},
		MaxTurns: 2,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	events := subscribeEvents(t, a)

	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	got := drainUntil(events, untilDone)

	// A turn-cap stop is a policy outcome, not a failure: Success stays true.
	want := []event.Event{event.AgentMaxTurns{Turns: 2}, event.AgentDone{
		Success: true,
		Reason:  event.EndMaxTurns,
		Usage:   event.RunUsage{Turns: 2, ToolCalls: 2},
	}}
	if n := len(got); n < 2 || !slices.Equal(got[n-2:], want) {
		t.Fatalf("events = %v; want tail %v", eventTypeSeq(got), want)
	}
}

func TestPrompt_StreamsTokensAndEndsSuccess(t *testing.T) {
	t.Parallel()

	provider := llmtest.NewScripted(llmtest.TextUsage(&llm.Usage{
		PromptTokens:     10,
		CompletionTokens: 2,
	}, "Hello", " world"))
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

// TestPark_TranslatesAgentParkedToAgentWaitingInStreamOrder verifies
// the kit translator emits [event.AgentWaiting] in stream order with
// [event.AgentToken] when the foundation emits [agentevent.AgentParked].
// This is the architectural fix for the historical race where the
// AgentWaiting send originated from a coding hook on the foundation
// goroutine and could overtake AgentTokens still queued in the
// translator's foundationEvents buffer.
func TestPark_TranslatesAgentParkedToAgentWaitingInStreamOrder(t *testing.T) {
	t.Parallel()

	provider := llmtest.NewScripted(llmtest.Text("a", "b", "c"))

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

	provider := llmtest.Failing(errors.New("boom"))
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

	provider := llmtest.Failing(errors.New("boom"))

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

func TestErrRunInProgress(t *testing.T) {
	t.Parallel()

	provider := llmtest.Blocking()
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

	provider := llmtest.NewScripted(llmtest.Text("hi"))
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
	provider := llmtest.NewScripted(llmtest.TextUsage(usage, "hi"))
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

	provider := llmtest.NewScripted(llmtest.Text("hi"))
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
			event.AgentToolCall, event.AgentToolResult, event.AgentWaiting, event.AgentStatus,
			event.AgentMaxTurns:
			// allowed — every kit/event type a Phase 2 facade can produce
			// or that an application hook may have emitted directly.
		default:
			t.Fatalf("unexpected event type %T leaked from foundation: %+v", ev, ev)
		}
	}
}

func TestClose_StopsTranslatorGoroutine(t *testing.T) {
	t.Parallel()

	provider := llmtest.NewScripted(llmtest.Text("hi"))
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
	// (before Bus.Close finished) the channel would still be open
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
		t.Fatal("subscription inbox not closed after Close — Bus.Close did not run or Close returned early")
	}
}

func TestClose_Idempotent(t *testing.T) {
	t.Parallel()

	provider := llmtest.NewScripted(llmtest.Text("hi"))
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

	provider := llmtest.Blocking()
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
	// Close calls Cancel, which aborts the run.
	if d.Success {
		t.Fatalf("AgentDone.Success=true after Close, want false (Close cancels)")
	}
}

func TestPrompt_AfterCloseReturnsErrClosed(t *testing.T) {
	t.Parallel()

	provider := llmtest.NewScripted(llmtest.Text("hi"))
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
	if _, err := a.Run(context.Background(), []llm.Message{{Role: "user", Content: "x"}}); !errors.Is(err, kit.ErrClosed) {
		t.Fatalf("Run after Close: want ErrClosed, got %v", err)
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

// TestSubscribeProbeObservesAlongsideConsumer (P9): a probe subscriber
// sees every event in publish order next to the consumer, and neither
// blocks the other. A terminating tool hook ends the run without a Cancel.
func TestSubscribeProbeObservesAlongsideConsumer(t *testing.T) {
	t.Parallel()

	provider := llmtest.NewScripted(
		llmtest.ToolCalls(llmtest.Call("call-1", "echo", `{"text":"hi"}`)),
	)
	hooks := kit.Hooks{
		AfterToolCall: func(_ context.Context, _ kit.AfterToolCallInput) (kit.AfterToolCallResult, error) {
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

	// Real consumer: the headless-style drain path.
	consumer := subscribeEvents(t, a)
	// Probe: observes the same stream without claiming the consumer
	// position. A future audit logger / metrics emitter / test probe
	// is the same shape.
	probe := subscribeEvents(t, a)

	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	consumerSeq := drainUntil(consumer, untilDone)
	probeSeq := drainUntil(probe, untilDone)

	// Both subscribers must see a terminal AgentDone — proof both
	// received the full stream. Tool-call runs do not emit AgentToken
	// (the foundation's MessageUpdate stream carries the tool-call
	// id/args, not free text), so token concatenation is not part of
	// the assertion; AgentToolCall + AgentDone are the visible signals.
	_, consumerDone := pickTokenAndDone(consumerSeq)
	_, probeDone := pickTokenAndDone(probeSeq)
	if !consumerDone || !probeDone {
		t.Fatalf("missing AgentDone — consumer=%v probe=%v", consumerDone, probeDone)
	}
	// Both subscribers see the same event-type sequence in the same
	// order. Comparing the event-type slices is the structural P2
	// (per-subscriber order) check at the kit level.
	if got, want := eventTypeSeq(consumerSeq), eventTypeSeq(probeSeq); !slices.Equal(got, want) {
		t.Errorf("subscriber sequences diverged\n  consumer: %v\n     probe: %v", got, want)
	}
}

// eventTypeSeq returns the string type name of each event in evs, in
// order. Used by [TestSubscribeProbeObservesAlongsideConsumer] to
// compare what two subscribers saw.
func eventTypeSeq(evs []event.Event) []string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = fmt.Sprintf("%T", ev)
	}
	return out
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
