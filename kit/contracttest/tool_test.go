package contracttest

import (
	"context"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit"
)

// happyTool is a minimal in-process tool that satisfies the [kit.Tool]
// contract. Used as the canonical conforming implementation to verify
// the [Tool] fixture is wired correctly.
type happyTool struct{}

// Definition returns a stable, well-formed schema.
func (happyTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        "happy_test_tool",
			Description: "no-op tool for contracttest verification",
			Parameters: llm.FunctionParams{
				Type:       "object",
				Properties: map[string]llm.FunctionParam{},
				Required:   nil,
			},
		},
	}
}

// Execute returns an empty result regardless of input — never panics.
func (happyTool) Execute(_ context.Context, _ llm.ToolCall) kit.ToolResult {
	return kit.ToolResult{Content: "ok"}
}

// TestTool_HappyImplementationPasses confirms the fixture passes a
// canonical conforming tool.
func TestTool_HappyImplementationPasses(t *testing.T) {
	Tool(t, func() kit.Tool { return happyTool{} })
}

// TestTool_TypedToolPasses: a tool built by [kit.NewTool] holds the
// contract, malformed arguments included.
func TestTool_TypedToolPasses(t *testing.T) {
	type args struct {
		Path string `json:"path"`
	}
	Tool(t, func() kit.Tool {
		return kit.NewTool("typed_test_tool", "typed tool for contracttest verification",
			func(context.Context, args) (string, error) { return "ok", nil })
	})
}
