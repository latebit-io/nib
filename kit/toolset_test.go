package kit_test

import (
	"context"
	"errors"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/event"
)

// --- Merge: tool concatenation ---

func TestMerge_ConcatenatesToolsInOrder(t *testing.T) {
	t.Parallel()
	a := kit.Toolset{Tools: []kit.Tool{nopTool{name: "a"}}}
	b := kit.Toolset{Tools: []kit.Tool{nopTool{name: "b"}, nopTool{name: "c"}}}
	merged := kit.Merge(a, b)

	want := []string{"a", "b", "c"}
	if len(merged.Tools) != len(want) {
		t.Fatalf("got %d tools, want %d", len(merged.Tools), len(want))
	}
	for i, w := range want {
		got := merged.Tools[i].Definition().Function.Name
		if got != w {
			t.Errorf("tool[%d] = %q, want %q", i, got, w)
		}
	}
}

func TestMerge_ZeroValue(t *testing.T) {
	t.Parallel()
	a := kit.Toolset{Tools: []kit.Tool{nopTool{name: "x"}}}
	merged := kit.Merge(a, kit.Toolset{})
	if len(merged.Tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(merged.Tools))
	}
}

func TestMerge_Empty(t *testing.T) {
	t.Parallel()
	merged := kit.Merge()
	if len(merged.Tools) != 0 {
		t.Fatalf("got %d tools, want 0", len(merged.Tools))
	}
	if merged.Hooks.BeforeToolCall != nil {
		t.Fatal("expected nil BeforeToolCall")
	}
}

func TestMerge_Associative(t *testing.T) {
	t.Parallel()
	a := kit.Toolset{Tools: []kit.Tool{nopTool{name: "a"}}}
	b := kit.Toolset{Tools: []kit.Tool{nopTool{name: "b"}}}
	c := kit.Toolset{Tools: []kit.Tool{nopTool{name: "c"}}}

	left := kit.Merge(a, kit.Merge(b, c))
	right := kit.Merge(kit.Merge(a, b), c)
	flat := kit.Merge(a, b, c)

	for i := range flat.Tools {
		ln := left.Tools[i].Definition().Function.Name
		rn := right.Tools[i].Definition().Function.Name
		fn := flat.Tools[i].Definition().Function.Name
		if ln != fn || rn != fn {
			t.Errorf("tool[%d]: left=%q right=%q flat=%q", i, ln, rn, fn)
		}
	}
}

// --- Hook chaining: BeforeToolCall ---

func TestMerge_BeforeToolCall_ChainsInOrder(t *testing.T) {
	t.Parallel()
	var order []string
	a := kit.Toolset{Hooks: kit.Hooks{
		BeforeToolCall: func(_ context.Context, _ kit.BeforeToolCallContext) (kit.BeforeToolCallResult, error) {
			order = append(order, "a")
			return kit.BeforeToolCallResult{}, nil
		},
	}}
	b := kit.Toolset{Hooks: kit.Hooks{
		BeforeToolCall: func(_ context.Context, _ kit.BeforeToolCallContext) (kit.BeforeToolCallResult, error) {
			order = append(order, "b")
			return kit.BeforeToolCallResult{}, nil
		},
	}}
	merged := kit.Merge(a, b)
	_, err := merged.Hooks.BeforeToolCall(context.Background(), kit.BeforeToolCallContext{})
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "a" || order[1] != "b" {
		t.Fatalf("order = %v, want [a b]", order)
	}
}

func TestMerge_BeforeToolCall_BlockShortCircuits(t *testing.T) {
	t.Parallel()
	var bCalled bool
	a := kit.Toolset{Hooks: kit.Hooks{
		BeforeToolCall: func(_ context.Context, _ kit.BeforeToolCallContext) (kit.BeforeToolCallResult, error) {
			return kit.BeforeToolCallResult{Block: true, Reason: "nope"}, nil
		},
	}}
	b := kit.Toolset{Hooks: kit.Hooks{
		BeforeToolCall: func(_ context.Context, _ kit.BeforeToolCallContext) (kit.BeforeToolCallResult, error) {
			bCalled = true
			return kit.BeforeToolCallResult{}, nil
		},
	}}
	merged := kit.Merge(a, b)
	res, err := merged.Hooks.BeforeToolCall(context.Background(), kit.BeforeToolCallContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Block || res.Reason != "nope" {
		t.Fatalf("expected block with reason 'nope', got %+v", res)
	}
	if bCalled {
		t.Fatal("b should not have been called after block")
	}
}

func TestMerge_BeforeToolCall_ErrorShortCircuits(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	var bCalled bool
	a := kit.Toolset{Hooks: kit.Hooks{
		BeforeToolCall: func(_ context.Context, _ kit.BeforeToolCallContext) (kit.BeforeToolCallResult, error) {
			return kit.BeforeToolCallResult{}, boom
		},
	}}
	b := kit.Toolset{Hooks: kit.Hooks{
		BeforeToolCall: func(_ context.Context, _ kit.BeforeToolCallContext) (kit.BeforeToolCallResult, error) {
			bCalled = true
			return kit.BeforeToolCallResult{}, nil
		},
	}}
	merged := kit.Merge(a, b)
	_, err := merged.Hooks.BeforeToolCall(context.Background(), kit.BeforeToolCallContext{})
	if !errors.Is(err, boom) {
		t.Fatalf("want boom, got %v", err)
	}
	if bCalled {
		t.Fatal("b should not have been called after error")
	}
}

func TestMerge_BeforeToolCall_NilWhenEmpty(t *testing.T) {
	t.Parallel()
	merged := kit.Merge(kit.Toolset{}, kit.Toolset{})
	if merged.Hooks.BeforeToolCall != nil {
		t.Fatal("expected nil BeforeToolCall when no hooks contribute")
	}
}

func TestMerge_BeforeToolCall_SinglePassthrough(t *testing.T) {
	t.Parallel()
	called := false
	a := kit.Toolset{Hooks: kit.Hooks{
		BeforeToolCall: func(_ context.Context, _ kit.BeforeToolCallContext) (kit.BeforeToolCallResult, error) {
			called = true
			return kit.BeforeToolCallResult{Block: true, Reason: "only"}, nil
		},
	}}
	merged := kit.Merge(a)
	res, _ := merged.Hooks.BeforeToolCall(context.Background(), kit.BeforeToolCallContext{})
	if !called || !res.Block {
		t.Fatal("single hook should pass through directly")
	}
}

// --- Hook chaining: AfterToolCall ---

func TestMerge_AfterToolCall_LaterOverrides(t *testing.T) {
	t.Parallel()
	contentA := "from-a"
	contentB := "from-b"
	isErrTrue := true
	a := kit.Toolset{Hooks: kit.Hooks{
		AfterToolCall: func(_ context.Context, _ kit.AfterToolCallContext) (kit.AfterToolCallResult, error) {
			return kit.AfterToolCallResult{Content: &contentA}, nil
		},
	}}
	b := kit.Toolset{Hooks: kit.Hooks{
		AfterToolCall: func(_ context.Context, _ kit.AfterToolCallContext) (kit.AfterToolCallResult, error) {
			return kit.AfterToolCallResult{Content: &contentB, IsError: &isErrTrue}, nil
		},
	}}
	merged := kit.Merge(a, b)
	res, err := merged.Hooks.AfterToolCall(context.Background(), kit.AfterToolCallContext{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content == nil || *res.Content != "from-b" {
		t.Fatalf("Content = %v, want 'from-b'", res.Content)
	}
	if res.IsError == nil || !*res.IsError {
		t.Fatal("IsError should be true from b")
	}
}

func TestMerge_AfterToolCall_TerminateORd(t *testing.T) {
	t.Parallel()
	a := kit.Toolset{Hooks: kit.Hooks{
		AfterToolCall: func(_ context.Context, _ kit.AfterToolCallContext) (kit.AfterToolCallResult, error) {
			return kit.AfterToolCallResult{Terminate: true}, nil
		},
	}}
	b := kit.Toolset{Hooks: kit.Hooks{
		AfterToolCall: func(_ context.Context, _ kit.AfterToolCallContext) (kit.AfterToolCallResult, error) {
			return kit.AfterToolCallResult{Terminate: false}, nil
		},
	}}
	merged := kit.Merge(a, b)
	res, _ := merged.Hooks.AfterToolCall(context.Background(), kit.AfterToolCallContext{})
	if !res.Terminate {
		t.Fatal("Terminate should be OR'd to true")
	}
}

func TestMerge_AfterToolCall_ErrorShortCircuits(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	a := kit.Toolset{Hooks: kit.Hooks{
		AfterToolCall: func(_ context.Context, _ kit.AfterToolCallContext) (kit.AfterToolCallResult, error) {
			return kit.AfterToolCallResult{}, boom
		},
	}}
	b := kit.Toolset{Hooks: kit.Hooks{
		AfterToolCall: func(_ context.Context, _ kit.AfterToolCallContext) (kit.AfterToolCallResult, error) {
			t.Fatal("should not be called")
			return kit.AfterToolCallResult{}, nil
		},
	}}
	merged := kit.Merge(a, b)
	_, err := merged.Hooks.AfterToolCall(context.Background(), kit.AfterToolCallContext{})
	if !errors.Is(err, boom) {
		t.Fatalf("want boom, got %v", err)
	}
}

// --- Hook chaining: TransformContext ---

func TestMerge_TransformContext_Pipeline(t *testing.T) {
	t.Parallel()
	a := kit.Toolset{Hooks: kit.Hooks{
		TransformContext: func(_ context.Context, msgs []llm.Message) ([]llm.Message, error) {
			return append(msgs, llm.Message{Role: "system", Content: "injected-a"}), nil
		},
	}}
	b := kit.Toolset{Hooks: kit.Hooks{
		TransformContext: func(_ context.Context, msgs []llm.Message) ([]llm.Message, error) {
			return append(msgs, llm.Message{Role: "system", Content: "injected-b"}), nil
		},
	}}
	merged := kit.Merge(a, b)
	input := []llm.Message{{Role: "user", Content: "hi"}}
	out, err := merged.Hooks.TransformContext(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("got %d messages, want 3", len(out))
	}
	if out[1].Content != "injected-a" || out[2].Content != "injected-b" {
		t.Fatalf("pipeline order wrong: %+v", out)
	}
}

func TestMerge_TransformContext_NilPassthrough(t *testing.T) {
	t.Parallel()
	a := kit.Toolset{Hooks: kit.Hooks{
		TransformContext: func(_ context.Context, _ []llm.Message) ([]llm.Message, error) {
			return nil, nil
		},
	}}
	b := kit.Toolset{Hooks: kit.Hooks{
		TransformContext: func(_ context.Context, msgs []llm.Message) ([]llm.Message, error) {
			return msgs, nil
		},
	}}
	merged := kit.Merge(a, b)
	input := []llm.Message{{Role: "user", Content: "hi"}}
	out, err := merged.Hooks.TransformContext(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Content != "hi" {
		t.Fatalf("nil return should pass input through: %+v", out)
	}
}

func TestMerge_TransformContext_ErrorShortCircuits(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	a := kit.Toolset{Hooks: kit.Hooks{
		TransformContext: func(_ context.Context, _ []llm.Message) ([]llm.Message, error) {
			return nil, boom
		},
	}}
	b := kit.Toolset{Hooks: kit.Hooks{
		TransformContext: func(_ context.Context, _ []llm.Message) ([]llm.Message, error) {
			t.Fatal("should not be called")
			return nil, nil
		},
	}}
	merged := kit.Merge(a, b)
	_, err := merged.Hooks.TransformContext(context.Background(), nil)
	if !errors.Is(err, boom) {
		t.Fatalf("want boom, got %v", err)
	}
}

// --- Hook chaining: GetSteeringMessages / GetFollowUpMessages ---

func TestMerge_GetSteeringMessages_Concatenates(t *testing.T) {
	t.Parallel()
	a := kit.Toolset{Hooks: kit.Hooks{
		GetSteeringMessages: func(_ context.Context) ([]llm.Message, error) {
			return []llm.Message{{Role: "user", Content: "steer-a"}}, nil
		},
	}}
	b := kit.Toolset{Hooks: kit.Hooks{
		GetSteeringMessages: func(_ context.Context) ([]llm.Message, error) {
			return []llm.Message{{Role: "user", Content: "steer-b"}}, nil
		},
	}}
	merged := kit.Merge(a, b)
	msgs, err := merged.Hooks.GetSteeringMessages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Content != "steer-a" || msgs[1].Content != "steer-b" {
		t.Fatalf("got %+v, want [steer-a steer-b]", msgs)
	}
}

func TestMerge_GetFollowUpMessages_Concatenates(t *testing.T) {
	t.Parallel()
	a := kit.Toolset{Hooks: kit.Hooks{
		GetFollowUpMessages: func(_ context.Context) ([]llm.Message, error) {
			return []llm.Message{{Role: "user", Content: "follow-a"}}, nil
		},
	}}
	b := kit.Toolset{Hooks: kit.Hooks{
		GetFollowUpMessages: func(_ context.Context) ([]llm.Message, error) {
			return []llm.Message{{Role: "user", Content: "follow-b"}}, nil
		},
	}}
	merged := kit.Merge(a, b)
	msgs, err := merged.Hooks.GetFollowUpMessages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Content != "follow-a" || msgs[1].Content != "follow-b" {
		t.Fatalf("got %+v, want [follow-a follow-b]", msgs)
	}
}

func TestMerge_GetMessages_ErrorShortCircuits(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	a := kit.Toolset{Hooks: kit.Hooks{
		GetSteeringMessages: func(_ context.Context) ([]llm.Message, error) {
			return nil, boom
		},
	}}
	b := kit.Toolset{Hooks: kit.Hooks{
		GetSteeringMessages: func(_ context.Context) ([]llm.Message, error) {
			t.Fatal("should not be called")
			return nil, nil
		},
	}}
	merged := kit.Merge(a, b)
	_, err := merged.Hooks.GetSteeringMessages(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("want boom, got %v", err)
	}
}

// --- Hook chaining: OnTruncated ---

func TestMerge_OnTruncated_LastNonZeroWins(t *testing.T) {
	t.Parallel()
	a := kit.Toolset{Hooks: kit.Hooks{
		OnTruncated: func(_ context.Context, _ kit.TruncationContext) (kit.TruncationResult, error) {
			return kit.TruncationResult{Retry: true, Messages: []llm.Message{{Content: "a"}}}, nil
		},
	}}
	b := kit.Toolset{Hooks: kit.Hooks{
		OnTruncated: func(_ context.Context, _ kit.TruncationContext) (kit.TruncationResult, error) {
			return kit.TruncationResult{Retry: false, Messages: []llm.Message{{Content: "b"}}}, nil
		},
	}}
	merged := kit.Merge(a, b)
	res, err := merged.Hooks.OnTruncated(context.Background(), kit.TruncationContext{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Retry {
		t.Fatal("last non-zero should win: Retry=false")
	}
	if len(res.Messages) != 1 || res.Messages[0].Content != "b" {
		t.Fatalf("last non-zero should win: got %+v", res)
	}
}

func TestMerge_OnTruncated_ZeroSkipped(t *testing.T) {
	t.Parallel()
	a := kit.Toolset{Hooks: kit.Hooks{
		OnTruncated: func(_ context.Context, _ kit.TruncationContext) (kit.TruncationResult, error) {
			return kit.TruncationResult{Retry: true, Messages: []llm.Message{{Content: "keep"}}}, nil
		},
	}}
	b := kit.Toolset{Hooks: kit.Hooks{
		OnTruncated: func(_ context.Context, _ kit.TruncationContext) (kit.TruncationResult, error) {
			return kit.TruncationResult{}, nil
		},
	}}
	merged := kit.Merge(a, b)
	res, _ := merged.Hooks.OnTruncated(context.Background(), kit.TruncationContext{})
	if !res.Retry || len(res.Messages) != 1 || res.Messages[0].Content != "keep" {
		t.Fatalf("zero result should not override: got %+v", res)
	}
}

func TestMerge_OnTruncated_ErrorShortCircuits(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	a := kit.Toolset{Hooks: kit.Hooks{
		OnTruncated: func(_ context.Context, _ kit.TruncationContext) (kit.TruncationResult, error) {
			return kit.TruncationResult{}, boom
		},
	}}
	b := kit.Toolset{Hooks: kit.Hooks{
		OnTruncated: func(_ context.Context, _ kit.TruncationContext) (kit.TruncationResult, error) {
			t.Fatal("should not be called")
			return kit.TruncationResult{}, nil
		},
	}}
	merged := kit.Merge(a, b)
	_, err := merged.Hooks.OnTruncated(context.Background(), kit.TruncationContext{})
	if !errors.Is(err, boom) {
		t.Fatalf("want boom, got %v", err)
	}
}

// --- Tool dedup via New() ---

func TestNew_DeduplicatesToolsFirstWins(t *testing.T) {
	t.Parallel()
	provider := newScriptedProvider(streamText([]string{"hi"}, nil))
	events := make(chan event.Event, 32)

	a, err := kit.New(kit.Config{
		Provider: provider,
		Events:   events,
		Tools:    []kit.Tool{nopTool{name: "echo"}},
		Toolsets: []kit.Toolset{
			{Tools: []kit.Tool{nopTool{name: "echo"}, nopTool{name: "search"}}},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	// The foundation registered tools: "echo" from Tools (first wins),
	// "search" from Toolset. We verify indirectly: if dedup failed,
	// foundation.New would have rejected the duplicate tool name.
}

func TestNew_ToolsetsHooksChainWithDirectHooks(t *testing.T) {
	t.Parallel()

	var order []string
	provider := newScriptedProvider(
		streamWithToolCall("call-1", "echo", `{}`),
	)
	hooks := kit.Hooks{
		BeforeToolCall: func(_ context.Context, _ kit.BeforeToolCallContext) (kit.BeforeToolCallResult, error) {
			order = append(order, "direct")
			return kit.BeforeToolCallResult{}, nil
		},
		AfterToolCall: func(_ context.Context, _ kit.AfterToolCallContext) (kit.AfterToolCallResult, error) {
			return kit.AfterToolCallResult{Terminate: true}, nil
		},
	}
	ts := kit.Toolset{
		Hooks: kit.Hooks{
			BeforeToolCall: func(_ context.Context, _ kit.BeforeToolCallContext) (kit.BeforeToolCallResult, error) {
				order = append(order, "toolset")
				return kit.BeforeToolCallResult{}, nil
			},
		},
	}
	events := make(chan event.Event, 32)

	a, err := kit.New(kit.Config{
		Provider: provider,
		Events:   events,
		Tools:    []kit.Tool{nopTool{name: "echo"}},
		Hooks:    hooks,
		Toolsets: []kit.Toolset{ts},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	a.WaitForIdle()
	drainUntil(events, untilDone)

	if len(order) != 2 || order[0] != "direct" || order[1] != "toolset" {
		t.Fatalf("hook order = %v, want [direct toolset]", order)
	}
}
