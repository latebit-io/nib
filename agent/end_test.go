package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/latebit-io/nib/agent/event"
	"github.com/latebit-io/nib/ai/llm"
)

var echoCall = llm.ToolCall{ID: "c1", Type: "function", Function: llm.FunctionCall{Name: "echo", Arguments: "{}"}}

// hangingProvider opens a stream that never yields, so a run stays
// mid-turn until its ctx ends.
type hangingProvider struct{}

func (hangingProvider) Stream(context.Context, []llm.Message, []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	return make(chan llm.StreamEvent), nil
}

// reaction lets a test act on a run from its event stream.
type reaction func(ev event.Event, a *Agent, cancel context.CancelFunc)

// on returns a reaction that runs do when an event of type T arrives.
func on[T event.Event](do func(a *Agent, cancel context.CancelFunc)) reaction {
	return func(ev event.Event, a *Agent, cancel context.CancelFunc) {
		if _, ok := ev.(T); ok {
			do(a, cancel)
		}
	}
}

// runToEnd starts a run from seed on an agent built from opts (an echo
// tool is added), feeds every event to react and returns the AgentEnd
// together with everything that preceded it.
func runToEnd(t *testing.T, opts Options, seed []llm.Message, react reaction) (event.AgentEnd, []event.Event) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	events := make(chan event.Event, 64)
	opts.Events = events
	opts.Tools = []Tool{namedTool{name: "echo"}}
	a, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.PromptWithMessages(ctx, seed); err != nil {
		t.Fatalf("PromptWithMessages: %v", err)
	}

	var all []event.Event
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-events:
			if react != nil {
				react(ev, a, cancel)
			}
			if end, ok := ev.(event.AgentEnd); ok {
				a.WaitForIdle()
				return end, all
			}
			all = append(all, ev)
		case <-deadline:
			t.Fatalf("no AgentEnd before the deadline; got %d events", len(all))
		}
	}
}

var question = []llm.Message{{Role: "user", Content: "go"}}

// TestAgentEnd_Reason drives one run down each exit path of the loop and
// checks the reason AgentEnd reports, and whether an Error event came with it.
func TestAgentEnd_Reason(t *testing.T) {
	t.Parallel()

	failing := errors.New("boom")
	abort := func(a *Agent, _ context.CancelFunc) { a.Abort() }
	cancelCaller := func(_ *Agent, cancel context.CancelFunc) { cancel() }
	tests := []struct {
		name string
		opts Options
		// react ends a run that would not end on its own.
		react     reaction
		want      event.EndReason
		wantError bool
	}{
		{
			name: "one-shot run finishes",
			opts: Options{Provider: newScriptedProvider(streamText([]string{"done"}, nil)), OneShot: true},
			want: event.EndCompleted,
		},
		{
			name:  "Abort while parked",
			opts:  Options{Provider: newScriptedProvider(streamDone())},
			react: on[event.AgentParked](abort),
			want:  event.EndAborted,
		},
		{
			name:  "caller ctx cancelled while parked",
			opts:  Options{Provider: newScriptedProvider(streamDone())},
			react: on[event.AgentParked](cancelCaller),
			want:  event.EndAborted,
		},
		{
			name:  "Abort mid-turn",
			opts:  Options{Provider: hangingProvider{}},
			react: on[event.MessageStart](abort),
			want:  event.EndAborted,
		},
		{
			// The hook's own error is a consequence of the abort.
			name: "hook fails after Abort",
			opts: Options{
				Provider: newScriptedProvider(streamDone()),
				Hooks: Hooks{TransformContext: func(ctx context.Context, _ []llm.Message) ([]llm.Message, error) {
					<-ctx.Done()
					return nil, failing
				}},
			},
			react: on[event.AgentStart](abort),
			want:  event.EndAborted,
		},
		{
			name:      "provider fails",
			opts:      Options{Provider: errorProvider{err: failing}},
			want:      event.EndError,
			wantError: true,
		},
		{
			// A timeout of the provider's own is a failure, not an abort:
			// the run ctx is still live.
			name:      "provider times out on its own",
			opts:      Options{Provider: errorProvider{err: context.DeadlineExceeded}},
			want:      event.EndError,
			wantError: true,
		},
		{
			name: "hook fails",
			opts: Options{
				Provider: newScriptedProvider(streamDone()),
				Hooks: Hooks{TransformContext: func(context.Context, []llm.Message) ([]llm.Message, error) {
					return nil, failing
				}},
			},
			want:      event.EndError,
			wantError: true,
		},
		{
			name: "turn cap reached",
			opts: Options{Provider: newScriptedProvider(streamWithToolCalls([]llm.ToolCall{echoCall})), MaxTurns: 1},
			want: event.EndMaxTurns,
		},
		{
			name: "tool batch terminates",
			opts: Options{
				Provider: newScriptedProvider(streamWithToolCalls([]llm.ToolCall{echoCall})),
				Hooks: Hooks{AfterToolCall: func(context.Context, AfterToolCallInput) (AfterToolCallResult, error) {
					return AfterToolCallResult{Terminate: true}, nil
				}},
			},
			want: event.EndTerminated,
		},
		{
			name:      "truncated without a hook",
			opts:      Options{Provider: newScriptedProvider([]llm.StreamEvent{{Done: true, Truncated: true}})},
			want:      event.EndTruncated,
			wantError: true,
		},
		{
			name: "truncated, hook declines retry",
			opts: Options{
				Provider: newScriptedProvider([]llm.StreamEvent{{Done: true, Truncated: true}}),
				Hooks: Hooks{OnTruncated: func(context.Context, TruncationInput) (TruncationResult, error) {
					return TruncationResult{}, nil
				}},
			},
			want: event.EndTruncated,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			end, before := runToEnd(t, tc.opts, question, tc.react)
			if end.Reason != tc.want {
				t.Errorf("AgentEnd.Reason = %q; want %q", end.Reason, tc.want)
			}
			if got := countEvents[event.Error](before) > 0; got != tc.wantError {
				t.Errorf("Error event emitted = %v; want %v", got, tc.wantError)
			}
		})
	}
}

// TestAgentEnd_Usage: the run total sums every turn's provider usage.
func TestAgentEnd_Usage(t *testing.T) {
	t.Parallel()

	provider := newScriptedProvider(
		[]llm.StreamEvent{{Done: true, ToolCalls: []llm.ToolCall{echoCall}, Usage: &llm.Usage{
			PromptTokens: 10, CompletionTokens: 2, CachedTokens: 1, CacheWriteTokens: 4,
		}}},
		streamDone(), // a turn whose provider reports no usage still counts
		streamText([]string{"done"}, &llm.Usage{PromptTokens: 20, CompletionTokens: 3}),
	)
	// Steering keeps the run going past the usage-less second turn.
	steered := false
	hooks := Hooks{SteeringMessages: func(context.Context) ([]llm.Message, error) {
		if steered {
			return nil, nil
		}
		steered = true
		return []llm.Message{{Role: "user", Content: "continue"}}, nil
	}}

	end, _ := runToEnd(t, Options{Provider: provider, Hooks: hooks, OneShot: true}, question, nil)

	want := event.RunUsage{
		Turns: 3, ToolCalls: 1,
		PromptTokens: 30, CompletionTokens: 5, CachedTokens: 1, CacheWriteTokens: 4,
	}
	if end.Usage != want {
		t.Errorf("AgentEnd.Usage = %+v; want %+v", end.Usage, want)
	}
}

// TestAgentEnd_FinalText: the final text is what the model wrote in this
// run since the last reply, never text that was already in the transcript.
func TestAgentEnd_FinalText(t *testing.T) {
	t.Parallel()

	narrate := []llm.StreamEvent{{Token: "Looking."}, {Done: true, ToolCalls: []llm.ToolCall{echoCall}}}
	tests := []struct {
		name  string
		opts  Options
		seed  []llm.Message
		react reaction
		want  string
	}{
		{
			name: "answer supersedes narration",
			opts: Options{Provider: newScriptedProvider(narrate, streamText([]string{"The ", "answer."}, nil)), OneShot: true},
			seed: question,
			want: "The answer.",
		},
		{
			name: "empty last turn keeps the narration",
			opts: Options{Provider: newScriptedProvider(narrate, streamDone()), OneShot: true},
			seed: question,
			want: "Looking.",
		},
		{
			name: "seeded history is not this run's text",
			opts: Options{Provider: errorProvider{err: errors.New("boom")}},
			seed: []llm.Message{{Role: "user", Content: "q"}, {Role: "assistant", Content: "old answer"}},
			want: "",
		},
		{
			// The scripted provider has no second turn: the run fails
			// right after the reply, before any new text.
			name: "a reply starts a new exchange",
			opts: Options{Provider: newScriptedProvider(streamText([]string{"first"}, nil))},
			seed: question,
			react: on[event.AgentParked](func(a *Agent, _ context.CancelFunc) {
				a.Reply(context.Background(), "more")
			}),
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			end, _ := runToEnd(t, tc.opts, tc.seed, tc.react)
			if end.FinalText != tc.want {
				t.Errorf("AgentEnd.FinalText = %q; want %q", end.FinalText, tc.want)
			}
		})
	}
}
