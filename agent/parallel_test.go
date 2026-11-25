package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/nib/agent/event"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/ai/llmtest"
)

// funcTool is a tool whose Execute is a function.
type funcTool struct {
	namedTool
	fn func(ctx context.Context, call llm.ToolCall) ToolResult
}

func (t funcTool) Execute(ctx context.Context, call llm.ToolCall) ToolResult { return t.fn(ctx, call) }

// boom is a tool body that panics.
func boom(context.Context, llm.ToolCall) ToolResult { panic("tool bug") }

// callsOf returns n calls to tool, with ids c0, c1, …
func callsOf(tool string, n int) []llm.ToolCall {
	calls := make([]llm.ToolCall, n)
	for i := range calls {
		calls[i] = llmtest.Call(fmt.Sprintf("c%d", i), tool, "{}")
	}
	return calls
}

// admit is a ParallelTools predicate for the given tool names.
func admit(names ...string) func(string) bool {
	return func(name string) bool { return slices.Contains(names, name) }
}

// toolTrace returns the run's ToolStart/ToolEnd events as "start c0", "end c0", …
func toolTrace(events []event.Event) []string {
	var trace []string
	for _, ev := range events {
		switch e := ev.(type) {
		case event.ToolStart:
			trace = append(trace, "start "+e.CallID)
		case event.ToolEnd:
			trace = append(trace, "end "+e.CallID)
		}
	}
	return trace
}

// TestParallelTools_RunTogetherResultsInOrder: each call can only return
// once every call of the batch is running, so the batch finishes only if
// it is concurrent. The last call returns first; results stay in call order.
func TestParallelTools_RunTogetherResultsInOrder(t *testing.T) {
	t.Parallel()

	const n = 3
	var running sync.WaitGroup
	running.Add(n)
	release := make([]chan struct{}, n+1) // call i returns after call i+1
	for i := range release {
		release[i] = make(chan struct{})
	}
	close(release[n])
	calls := callsOf("echo", n)
	tool := funcTool{namedTool{name: "echo"}, func(ctx context.Context, call llm.ToolCall) ToolResult {
		i := slices.IndexFunc(calls, func(c llm.ToolCall) bool { return c.ID == call.ID })
		running.Done()
		running.Wait()
		select {
		case <-release[i+1]:
		case <-ctx.Done():
			return ToolResult{Content: "cancelled"}
		}
		close(release[i])
		return ToolResult{Content: "ok"}
	}}

	end, before := runToEnd(t, Options{
		Provider:      llmtest.NewScripted(llmtest.ToolCalls(calls...), llmtest.Done()),
		Tools:         []Tool{tool},
		ParallelTools: admit("echo"),
		OneShot:       true,
	}, question, nil)

	wantTrace := []string{"start c0", "start c1", "start c2", "end c0", "end c1", "end c2"}
	if trace := toolTrace(before); !slices.Equal(trace, wantTrace) {
		t.Errorf("tool events = %v; want %v", trace, wantTrace)
	}
	if got, want := toolResults(end.Messages), []string{"c0=ok", "c1=ok", "c2=ok"}; !slices.Equal(got, want) {
		t.Errorf("tool results = %v; want %v", got, want)
	}
	if end.Reason != event.EndCompleted {
		t.Errorf("Reason = %q; want completed", end.Reason)
	}
}

// TestParallelTools_Batches covers which calls of a batch run together
// and what hooks do to them. Whatever happens, every call is answered.
func TestParallelTools_Batches(t *testing.T) {
	t.Parallel()

	ok := func(context.Context, llm.ToolCall) ToolResult { return ToolResult{Content: "ok"} }
	blockC1 := func(_ context.Context, c BeforeToolCallInput) (BeforeToolCallResult, error) {
		return BeforeToolCallResult{Block: c.CallID == "c1", Reason: "no"}, nil
	}
	failC1Before := func(_ context.Context, c BeforeToolCallInput) (BeforeToolCallResult, error) {
		if c.CallID == "c1" {
			return BeforeToolCallResult{}, errors.New("boom")
		}
		return BeforeToolCallResult{}, nil
	}
	failC1After := func(_ context.Context, c AfterToolCallInput) (AfterToolCallResult, error) {
		if c.CallID == "c1" {
			return AfterToolCallResult{}, errors.New("boom")
		}
		return AfterToolCallResult{}, nil
	}
	failC0After := func(_ context.Context, c AfterToolCallInput) (AfterToolCallResult, error) {
		if c.CallID == "c0" {
			return AfterToolCallResult{}, errors.New("boom")
		}
		return AfterToolCallResult{}, nil
	}
	terminate := func(context.Context, AfterToolCallInput) (AfterToolCallResult, error) {
		return AfterToolCallResult{Terminate: true}, nil
	}
	// call builds one call; the driver registers tools "echo" and "other".
	call := func(id, tool string) llm.ToolCall { return llmtest.Call(id, tool, "{}") }
	oneByOne := []string{"start c0", "end c0", "start c1", "end c1"}
	together := []string{"start c0", "start c1", "end c0", "end c1"}

	tests := []struct {
		name        string
		calls       []llm.ToolCall
		fn          func(context.Context, llm.ToolCall) ToolResult
		admitted    func(string) bool
		hooks       Hooks
		want        event.EndReason
		wantTrace   []string
		wantResults []string
	}{
		{
			name: "no predicate: one by one", calls: callsOf("echo", 2), fn: ok,
			want: event.EndCompleted, wantTrace: oneByOne, wantResults: []string{"c0=ok", "c1=ok"},
		},
		{
			name: "tool not admitted: one by one", calls: callsOf("echo", 2), fn: ok, admitted: admit("other"),
			want: event.EndCompleted, wantTrace: oneByOne, wantResults: []string{"c0=ok", "c1=ok"},
		},
		{
			name: "a tool the model made up is never admitted", fn: ok, admitted: admit("echo", "ghost"),
			calls: []llm.ToolCall{call("c0", "echo"), call("c1", "ghost")},
			want:  event.EndCompleted, wantTrace: oneByOne,
			wantResults: []string{"c0=ok", `c1=Error: unknown tool "ghost"`},
		},
		{
			name: "admitted calls in a row run together, the rest alone", fn: ok, admitted: admit("echo"),
			calls: []llm.ToolCall{call("c0", "echo"), call("c1", "echo"), call("c2", "other"), call("c3", "echo")},
			want:  event.EndCompleted,
			wantTrace: []string{
				"start c0", "start c1", "end c0", "end c1",
				"start c2", "end c2",
				"start c3", "end c3",
			},
			wantResults: []string{"c0=ok", "c1=ok", "c2=ok", "c3=ok"},
		},
		{
			name: "blocked call is not run", calls: callsOf("echo", 2), fn: ok, admitted: admit("echo"),
			hooks: Hooks{BeforeToolCall: blockC1},
			want:  event.EndCompleted, wantTrace: together, wantResults: []string{"c0=ok", "c1=no"},
		},
		{
			name: "panicking tools are error results", calls: callsOf("echo", 2), fn: boom, admitted: admit("echo"),
			want: event.EndCompleted, wantTrace: together,
			wantResults: []string{"c0=Error: agent: panic in tool echo: tool bug", "c1=Error: agent: panic in tool echo: tool bug"},
		},
		{
			name: "BeforeToolCall fails: nothing runs", calls: callsOf("echo", 2), fn: ok, admitted: admit("echo"),
			hooks: Hooks{BeforeToolCall: failC1Before},
			want:  event.EndError, wantTrace: []string{"start c0", "start c1"},
			wantResults: []string{"c0=" + unfinishedToolResult, "c1=" + unfinishedToolResult},
		},
		{
			name: "AfterToolCall fails: earlier results stand", calls: callsOf("echo", 2), fn: ok, admitted: admit("echo"),
			hooks: Hooks{AfterToolCall: failC1After},
			want:  event.EndError, wantTrace: []string{"start c0", "start c1", "end c0"},
			wantResults: []string{"c0=ok", "c1=" + unfinishedToolResult},
		},
		{
			// c1 ran as well: its result is kept, only c0's is withheld.
			name: "AfterToolCall fails: later results stand too", calls: callsOf("echo", 2), fn: ok, admitted: admit("echo"),
			hooks: Hooks{AfterToolCall: failC0After},
			want:  event.EndError, wantTrace: []string{"start c0", "start c1", "end c1"},
			wantResults: []string{"c1=ok", "c0=" + unfinishedToolResult},
		},
		{
			name: "every result terminates", calls: callsOf("echo", 2), fn: ok, admitted: admit("echo"),
			hooks: Hooks{AfterToolCall: terminate},
			want:  event.EndTerminated, wantTrace: together, wantResults: []string{"c0=ok", "c1=ok"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			end, before := runToEnd(t, Options{
				Provider: llmtest.NewScripted(llmtest.ToolCalls(tc.calls...), llmtest.Done()),
				Tools: []Tool{
					funcTool{namedTool{name: "echo"}, tc.fn},
					funcTool{namedTool{name: "other"}, tc.fn},
				},
				ParallelTools: tc.admitted,
				Hooks:         tc.hooks,
				OneShot:       true,
			}, question, nil)

			if end.Reason != tc.want {
				t.Errorf("Reason = %q; want %q", end.Reason, tc.want)
			}
			if trace := toolTrace(before); !slices.Equal(trace, tc.wantTrace) {
				t.Errorf("tool events = %v; want %v", trace, tc.wantTrace)
			}
			if results := toolResults(end.Messages); !slices.Equal(results, tc.wantResults) {
				t.Errorf("tool results = %v; want %v", results, tc.wantResults)
			}
		})
	}
}

// TestParallelTools_Bounded: a large batch never has more than
// maxParallelTools calls executing at once.
func TestParallelTools_Bounded(t *testing.T) {
	t.Parallel()

	var (
		mu        sync.Mutex
		now, peak int
	)
	tool := funcTool{namedTool{name: "echo"}, func(context.Context, llm.ToolCall) ToolResult {
		mu.Lock()
		now++
		peak = max(peak, now)
		mu.Unlock()
		time.Sleep(5 * time.Millisecond) // long enough for the batch to pile up
		mu.Lock()
		now--
		mu.Unlock()
		return ToolResult{Content: "ok"}
	}}

	end, _ := runToEnd(t, Options{
		Provider:      llmtest.NewScripted(llmtest.ToolCalls(callsOf("echo", 4*maxParallelTools)...), llmtest.Done()),
		Tools:         []Tool{tool},
		ParallelTools: admit("echo"),
		OneShot:       true,
	}, question, nil)

	if end.Reason != event.EndCompleted {
		t.Fatalf("Reason = %q; want completed", end.Reason)
	}
	if peak < 2 || peak > maxParallelTools {
		t.Errorf("peak concurrency = %d; want between 2 and %d", peak, maxParallelTools)
	}
}

// TestParallelTools_CancelWhileSlotsAreFull: the run is cancelled once
// every slot is taken. No waiting call may start, the calls that ran keep
// their results, and the rest are answered as unfinished.
func TestParallelTools_CancelWhileSlotsAreFull(t *testing.T) {
	t.Parallel()

	const n = 4 * maxParallelTools
	var started atomic.Int32
	full := make(chan struct{})
	tool := funcTool{namedTool{name: "echo"}, func(ctx context.Context, _ llm.ToolCall) ToolResult {
		if started.Add(1) == maxParallelTools {
			close(full)
		}
		<-ctx.Done()
		return ToolResult{Content: "late"}
	}}

	end, _ := runToEnd(t, Options{
		Provider:      llmtest.NewScripted(llmtest.ToolCalls(callsOf("echo", n)...)),
		Tools:         []Tool{tool},
		ParallelTools: admit("echo"),
	}, question, on[event.AgentStart](func(a *Agent, _ context.CancelFunc) {
		go func() {
			<-full
			a.Abort()
		}()
	}))

	if end.Reason != event.EndAborted {
		t.Errorf("Reason = %q; want aborted", end.Reason)
	}
	if s := started.Load(); s != maxParallelTools {
		t.Errorf("%d tools started; want exactly %d", s, maxParallelTools)
	}
	late, unfinished := 0, 0
	for _, r := range toolResults(end.Messages) {
		switch _, content, _ := strings.Cut(r, "="); content {
		case "late":
			late++
		case unfinishedToolResult:
			unfinished++
		}
	}
	if late != maxParallelTools || unfinished != n-maxParallelTools {
		t.Errorf("results: %d kept, %d unfinished; want %d and %d", late, unfinished, maxParallelTools, n-maxParallelTools)
	}
}

// TestTools_NotStartedAfterCancel: a cancel that lands while BeforeToolCall
// runs must keep the tool from starting, on the one-call path too.
func TestTools_NotStartedAfterCancel(t *testing.T) {
	t.Parallel()

	var started atomic.Bool
	tool := funcTool{namedTool{name: "echo"}, func(context.Context, llm.ToolCall) ToolResult {
		started.Store(true)
		return ToolResult{Content: "ok"}
	}}
	hooks := Hooks{BeforeToolCall: func(ctx context.Context, _ BeforeToolCallInput) (BeforeToolCallResult, error) {
		<-ctx.Done()
		return BeforeToolCallResult{}, nil
	}}

	end, _ := runToEnd(t, Options{
		Provider: llmtest.NewScripted(llmtest.ToolCalls(echoCall)),
		Tools:    []Tool{tool},
		Hooks:    hooks,
	}, question, on[event.ToolStart](func(a *Agent, _ context.CancelFunc) { a.Abort() }))

	if started.Load() {
		t.Error("the tool ran after the run was cancelled")
	}
	if got, want := toolResults(end.Messages), []string{"c1=" + unfinishedToolResult}; !slices.Equal(got, want) || end.Reason != event.EndAborted {
		t.Errorf("Reason = %q, results = %v; want aborted, %v", end.Reason, got, want)
	}
}
