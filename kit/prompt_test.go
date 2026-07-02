package kit

import (
	"context"

	"testing"

	"github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/ai/llm"
)

// plainTool implements Tool only — no prompt guidance.
type plainTool struct{}

func (plainTool) Definition() llm.ToolDef {
	return llm.ToolDef{Type: "function", Function: llm.FunctionDef{Name: "plain"}}
}

func (plainTool) Execute(_ context.Context, _ llm.ToolCall) agent.ToolResult {
	return agent.ToolResult{Content: "ok"}
}

// guidedTool additionally implements PromptContributor.
type guidedTool struct{ plainTool }

func (guidedTool) PromptGuidelines() []string {
	return []string{"use `plain` sparingly"}
}

func TestToolPromptGuidelines(t *testing.T) {
	if got := ToolPromptGuidelines(plainTool{}); got != nil {
		t.Errorf("plain tool: want nil guidance, got %v", got)
	}
	got := ToolPromptGuidelines(guidedTool{})
	if len(got) != 1 || got[0] != "use `plain` sparingly" {
		t.Errorf("guided tool: want the contributed bullet, got %v", got)
	}
}
