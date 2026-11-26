package compact

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/latebit-io/nib/ai/llm"
)

// run builds one user turn followed by a tool round per body: an
// assistant call and its result. Tools are named from names in order.
func run(names []string, bodies ...string) []llm.Message {
	msgs := []llm.Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "go"}}
	for i, body := range bodies {
		id := string(rune('a' + i))
		msgs = append(msgs,
			llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: id, Function: llm.FunctionCall{Name: names[i%len(names)]}}}},
			llm.Message{Role: "tool", ToolCallID: id, Content: body})
	}
	return msgs
}

func transform(t *testing.T, h func(context.Context, []llm.Message) ([]llm.Message, error), msgs []llm.Message) []llm.Message {
	t.Helper()
	out, err := h(context.Background(), msgs)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestHooks_CutsWhatWasAlreadySent: the results of the latest tool
// round go whole; earlier large ones are cut to a head and a marker.
func TestHooks_CutsWhatWasAlreadySent(t *testing.T) {
	big := strings.Repeat("é", 5000)
	msgs := run([]string{"read_file"}, big, "small", big)
	h := Hooks(nil).TransformContext
	out := transform(t, h, msgs)

	if len(out[3].Content) > capBytes || !strings.Contains(out[3].Content, "[truncated: 10000 original bytes") || !utf8.ValidString(out[3].Content) {
		t.Errorf("the earlier result was not cut cleanly: %d bytes", len(out[3].Content))
	}
	if out[5].Content != "small" || out[7].Content != big {
		t.Error("a small result and the latest round's result must stay whole")
	}
	if msgs[3].Content != big {
		t.Error("the input must not be modified")
	}
	// The same conversation gives the same request: a prompt cache depends on it.
	if again := transform(t, h, msgs); again[3].Content != out[3].Content {
		t.Error("the transform is not deterministic")
	}
	small := run([]string{"read_file"}, "a", "b")
	if same := transform(t, h, small); &same[0] != &small[0] {
		t.Error("with nothing to trim, the input slice must come back")
	}
}

// TestHooks_ReducesOlderOutputOnceTheHistoryIsLong: in one long run
// with a single user message, older results become a line, and later
// rounds leave everything already sent exactly as it was.
func TestHooks_ReducesOlderOutputOnceTheHistoryIsLong(t *testing.T) {
	big := strings.Repeat("line of output\n", 400) // 6000 bytes, 4 KiB once cut
	bodies := make([]string, 40)
	for i := range bodies {
		bodies[i] = big
	}
	msgs := run([]string{"read_file"}, bodies...)
	h := Hooks(nil).TransformContext
	out := transform(t, h, msgs)

	if !strings.HasPrefix(out[3].Content, "[6000 bytes of output removed") {
		t.Fatalf("the oldest result was not reduced: %.60q", out[3].Content)
	}
	if last := out[len(out)-1].Content; last != big {
		t.Error("the latest round's result must stay whole")
	}
	if est := llm.EstimateMessageTokens(out, nil).History; est >= threshold {
		t.Errorf("history is still %d estimated tokens", est)
	}
	// Two more rounds: what was reduced stays reduced, the round that was
	// whole is now only cut, and nothing earlier is rewritten.
	more := append(append([]llm.Message(nil), msgs...), run([]string{"read_file"}, big, big)[2:]...)
	next := transform(t, h, more)
	for i := range out[:len(out)-1] {
		if next[i].Content != out[i].Content {
			t.Fatalf("message %d changed between requests: a prompt cache would miss", i)
		}
	}
	if got := next[len(out)-1].Content; !strings.Contains(got, "[truncated: 6000 original bytes") {
		t.Errorf("the round that was whole must now be cut, not reduced: %.40q", got)
	}
	// A cleared conversation starts over.
	if fresh := transform(t, h, run([]string{"read_file"}, big, "b")); !strings.Contains(fresh[3].Content, "[truncated:") {
		t.Errorf("after a clear, an early result must be cut, not reduced: %.40q", fresh[3].Content)
	}
}

// TestHooks_ExemptTools: a skill's instructions are a tool result and
// must survive whole, however old and however long the history.
func TestHooks_ExemptTools(t *testing.T) {
	big := strings.Repeat("x", 9000)
	bodies := make([]string, 40)
	for i := range bodies {
		bodies[i] = big
	}
	msgs := run([]string{"skill_notes", "read_file"}, bodies...)
	out := transform(t, Hooks(func(tool string) bool { return strings.HasPrefix(tool, "skill_") }).TransformContext, msgs)
	if out[3].Content != big {
		t.Error("the exempt tool's result was touched")
	}
	if out[5].Content == big {
		t.Error("the other tool's old result must still be trimmed")
	}
}
