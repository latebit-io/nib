package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOpenAIEffort(t *testing.T) {
	cases := []struct {
		in   Effort
		want string
	}{
		{EffortLow, "low"},
		{EffortMedium, "medium"},
		{EffortHigh, "high"},
		{EffortXHigh, "high"}, // nib's higher tiers collapse to the OpenAI ceiling
		{EffortMax, "high"},
		{"", ""},      // unset → omit
		{"bogus", ""}, // unrecognized → omit
	}
	for _, c := range cases {
		if got := openAIEffort(c.in); got != c.want {
			t.Errorf("openAIEffort(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestChatRequest_ReasoningEffortSerialization(t *testing.T) {
	// Set → present.
	data, _ := json.Marshal(chatRequest{Model: "m", ReasoningEffort: "high"})
	if !strings.Contains(string(data), `"reasoning_effort":"high"`) {
		t.Errorf("want reasoning_effort in request; got %s", data)
	}
	// Empty → omitted, so non-reasoning models are unaffected.
	data, _ = json.Marshal(chatRequest{Model: "m"})
	if strings.Contains(string(data), "reasoning_effort") {
		t.Errorf("empty effort must omit reasoning_effort; got %s", data)
	}
	// Same for the caching request shape.
	data, _ = json.Marshal(cachingChatRequest{Model: "m", ReasoningEffort: "low"})
	if !strings.Contains(string(data), `"reasoning_effort":"low"`) {
		t.Errorf("want reasoning_effort in caching request; got %s", data)
	}
	data, _ = json.Marshal(cachingChatRequest{Model: "m"})
	if strings.Contains(string(data), "reasoning_effort") {
		t.Errorf("empty effort must omit reasoning_effort (caching); got %s", data)
	}
}

func TestCodexRequest_ReasoningSerialization(t *testing.T) {
	data, _ := json.Marshal(codexRequest{Model: "m", Reasoning: &codexReasoning{Effort: "medium"}})
	if !strings.Contains(string(data), `"reasoning":{"effort":"medium"}`) {
		t.Errorf("want reasoning object in codex request; got %s", data)
	}
	data, _ = json.Marshal(codexRequest{Model: "m"})
	if strings.Contains(string(data), "reasoning") {
		t.Errorf("nil reasoning must be omitted; got %s", data)
	}
}

// TestOpenAIProviders_StoreEffort locks the constructor wiring: the effort
// passed in is retained for the request builder to read.
func TestOpenAIProviders_StoreEffort(t *testing.T) {
	if c := NewCodexAPI("m", StaticKeyAuth("k"), EffortHigh); c.effort != EffortHigh {
		t.Errorf("CodexAPI.effort = %q, want high", c.effort)
	}
	if a := NewAgentAPI("http://x", "m", StaticKeyAuth("k"), false, EffortLow); a.effort != EffortLow {
		t.Errorf("AgentAPI.effort = %q, want low", a.effort)
	}
}
