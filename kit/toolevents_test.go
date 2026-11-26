package kit_test

import (
	"context"
	"testing"

	"github.com/latebit-io/nib/ai/llmtest"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/event"
)

// TestToolEvents_PairByCallID: a tool call is reported before it runs
// and its result after, both carrying the call's ID.
func TestToolEvents_PairByCallID(t *testing.T) {
	t.Parallel()
	provider := llmtest.NewScripted(
		llmtest.ToolCalls(llmtest.Call("call-1", "nop", `{}`)),
		llmtest.Text("done"),
	)
	a, err := kit.New(kit.Config{Provider: provider, Toolset: kit.Toolset{Tools: []kit.Tool{nopTool{name: "nop"}}}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	events := subscribeEvents(t, a)
	if err := a.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	got := drainUntil(events, func(ev event.Event) bool {
		_, waiting := ev.(event.AgentWaiting)
		return waiting || untilDone(ev)
	})
	var order []string
	var call event.AgentToolCall
	var result event.AgentToolResult
	for _, ev := range got {
		switch e := ev.(type) {
		case event.AgentToolCall:
			call = e
			order = append(order, "call")
		case event.AgentToolResult:
			result = e
			order = append(order, "result")
		}
	}
	if len(order) != 2 || order[0] != "call" || order[1] != "result" {
		t.Fatalf("order = %v, want call then result", order)
	}
	if call.CallID != "call-1" || result.CallID != call.CallID || call.Name != "nop" || result.Name != "nop" || result.IsError {
		t.Errorf("call %+v and result %+v must share the call's ID and name", call, result)
	}
}
