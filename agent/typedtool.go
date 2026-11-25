package agent

import (
	"context"
	"fmt"

	"github.com/latebit-io/nib/ai/llm"
)

// NewTool returns a [Tool] whose schema comes from the struct A (see
// [llm.SchemaFor]) and whose arguments are validated and decoded before
// run. An error from either reaches the model. Panics if A has no schema.
func NewTool[A any](name, description string, run func(ctx context.Context, args A) (string, error)) Tool {
	params, err := llm.SchemaFor[A]()
	if err != nil {
		panic(fmt.Sprintf("agent: tool %q: %v", name, err))
	}
	return &typedTool[A]{
		def: llm.ToolDef{Type: "function", Function: llm.FunctionDef{
			Name: name, Description: description, Parameters: params,
		}},
		run: run,
	}
}

// typedTool is the [Tool] built by [NewTool]. It holds no mutable state.
type typedTool[A any] struct {
	def llm.ToolDef
	run func(ctx context.Context, args A) (string, error)
}

// Definition returns the schema derived from A.
func (t *typedTool[A]) Definition() llm.ToolDef { return t.def }

// Execute validates and decodes the call's arguments, then calls run.
func (t *typedTool[A]) Execute(ctx context.Context, call llm.ToolCall) ToolResult {
	args, err := llm.DecodeArguments[A](t.def.Function.Parameters, call.Function.Arguments)
	if err != nil {
		return toolError(fmt.Errorf("invalid arguments: %w", err))
	}
	content, err := t.run(ctx, args)
	if err != nil {
		return toolError(err)
	}
	return ToolResult{Content: content}
}
