package agent

import (
	"reflect"
	"strings"
	"testing"

	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/ai/llm"
)

// P8 — Kill switch works. Setting NIB_TOOL_OUTPUT_CAP_DISABLED to
// any non-empty value reverts the cap-entry point to uncapped
// baseline so any regression introduced by the cap can be bypassed
// without a code change.
func TestCapToolOutputsIfEnabled_KillSwitchPreservesBytes(t *testing.T) {
	t.Setenv(brand.EnvKeyToolOutputCapDisabled, "1")

	big := strings.Repeat("x", toolOutputCapBytes*3)
	msgs := stalePayload(big)

	out := capToolOutputsIfEnabled(msgs)
	if out[3].Content != big {
		t.Errorf("kill switch did not disable cap: stale tool result was modified")
	}
	// With the kill switch on, the returned slice must be the same
	// slice (no allocation cost paid for a no-op).
	if &out[0] != &msgs[0] {
		t.Errorf("kill switch path allocated a new slice; expected the input slice")
	}
}

// Default-enabled cap fires when env var is unset. Pins the
// behavioural delta between enabled and disabled so a future
// refactor cannot accidentally make the env var inert.
func TestCapToolOutputsIfEnabled_DefaultEnables(t *testing.T) {
	t.Setenv(brand.EnvKeyToolOutputCapDisabled, "")

	big := strings.Repeat("x", toolOutputCapBytes*3)
	msgs := stalePayload(big)

	out := capToolOutputsIfEnabled(msgs)
	if out[3].Content == big {
		t.Fatalf("default-enabled cap did not fire on stale oversize tool result")
	}
	if !strings.Contains(out[3].Content, "truncated") {
		t.Errorf("capped result missing marker")
	}
}

// Two consecutive invocations on the same slice produce
// byte-identical output. Hook-level idempotency is what protects
// the prompt cache across turns when no new oversize result has
// arrived.
func TestCapToolOutputsIfEnabled_IdempotentAtEntryPoint(t *testing.T) {
	t.Setenv(brand.EnvKeyToolOutputCapDisabled, "")

	big := strings.Repeat("y", toolOutputCapBytes*3)
	msgs := stalePayload(big)

	once := capToolOutputsIfEnabled(msgs)
	twice := capToolOutputsIfEnabled(once)
	if !reflect.DeepEqual(once, twice) {
		t.Errorf("entry point not idempotent — cache stability invariant violated")
	}
}

// stalePayload returns a single-goal agentic conversation with
// three tool results — the typical shape of a pacman-style session
// (one user goal, many assistant↔tool exchanges). With keepRecent=2
// only the oldest tool result (idx 3) sits in the stale window;
// the two newer ones stay verbatim.
func stalePayload(toolContent string) []llm.Message {
	return []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "the goal"},
		{Role: "assistant", Content: "a1"},
		{Role: "tool", ToolCallID: "oldest", Content: toolContent}, // idx 3 — stale
		{Role: "assistant", Content: "a2"},
		{Role: "tool", ToolCallID: "mid", Content: toolContent}, // idx 5 — keep
		{Role: "assistant", Content: "a3"},
		{Role: "tool", ToolCallID: "newest", Content: toolContent}, // idx 7 — keep
	}
}
