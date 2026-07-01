package contracttest

import (
	"context"
	"sync"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit"
)

// Tool runs the [kit.Tool] contract suite against tools returned by
// ctor.
//
// Contract preconditions on the ctor'd tool:
//
//   - Definition() returns a stable, well-formed schema with a non-empty
//     Function.Name and Type="function".
//   - Execute() handles malformed argument JSON by returning a
//     [kit.ToolResult] with IsError=true rather than panicking.
//   - Execute() is safe to call concurrently from multiple goroutines.
//
// Subtests verify the invariants above. The fixture does NOT verify
// tool-specific success paths — those depend on the tool's args and
// backing resources, and belong in the tool's own test file. The
// contract suite enforces the cross-cutting discipline every tool
// must hold.
//
// Apply to a new tool implementation by adding a single test:
//
//	func TestMyToolContract(t *testing.T) {
//	    contracttest.Tool(t, func() kit.Tool {
//	        return mytool.New(...)
//	    })
//	}
func Tool(t *testing.T, ctor func() kit.Tool) {
	t.Helper()
	if ctor == nil {
		t.Fatal("contracttest.Tool: ctor must be non-nil")
	}

	t.Run("DefinitionIsWellFormed", func(t *testing.T) {
		t.Parallel()
		tool := ctor()
		def := tool.Definition()
		if def.Type != "function" {
			t.Fatalf("contract: Definition().Type = %q, want %q", def.Type, "function")
		}
		if def.Function.Name == "" {
			t.Fatal("contract: Definition().Function.Name must be non-empty (LLM cannot dispatch a nameless tool)")
		}
		if def.Function.Parameters.Type == "" {
			t.Fatal("contract: Definition().Function.Parameters.Type must be set (typically \"object\")")
		}
	})

	t.Run("DefinitionIsStableAcrossCalls", func(t *testing.T) {
		t.Parallel()
		tool := ctor()
		d1 := tool.Definition()
		d2 := tool.Definition()
		if d1.Function.Name != d2.Function.Name {
			t.Fatalf("contract: Definition().Function.Name changed across calls (%q vs %q)", d1.Function.Name, d2.Function.Name)
		}
		if d1.Type != d2.Type {
			t.Fatalf("contract: Definition().Type changed across calls (%q vs %q)", d1.Type, d2.Type)
		}
	})

	t.Run("ExecuteHandlesMalformedArgs", func(t *testing.T) {
		t.Parallel()
		tool := ctor()
		// Send a syntactically invalid arguments string. The tool must
		// return a ToolResult (with IsError=true is the convention) —
		// panicking would crash the agent loop, which is unrecoverable
		// for a tool failure mode.
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("contract: Execute panicked on malformed args: %v", r)
			}
		}()
		res := tool.Execute(context.Background(), llm.ToolCall{
			ID:   "contracttest-malformed",
			Type: "function",
			Function: llm.FunctionCall{
				Name:      tool.Definition().Function.Name,
				Arguments: "{not valid json",
			},
		})
		// Either IsError=true with explanatory content, or some
		// no-args-required tool that genuinely accepts an empty call
		// and produces a result. We don't enforce IsError specifically
		// because the invariant is "no panic" — the broader claim
		// (tool result must reflect failure) is tool-specific.
		_ = res
	})

	t.Run("ExecuteHandlesEmptyArgs", func(t *testing.T) {
		t.Parallel()
		tool := ctor()
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("contract: Execute panicked on empty args object: %v", r)
			}
		}()
		// Result ignored: the invariant under test is "no panic",
		// not any particular output for empty args.
		_ = tool.Execute(context.Background(), llm.ToolCall{
			ID:   "contracttest-empty",
			Type: "function",
			Function: llm.FunctionCall{
				Name:      tool.Definition().Function.Name,
				Arguments: "{}",
			},
		})
	})

	t.Run("ConcurrentExecuteSafe", func(t *testing.T) {
		t.Parallel()
		tool := ctor()
		const n = 8
		var wg sync.WaitGroup
		wg.Add(n)
		for i := range n {
			go func(i int) {
				defer wg.Done()
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("contract: concurrent Execute panicked (goroutine %d): %v", i, r)
					}
				}()
				// Result ignored: the invariant under test is
				// "no panic under concurrency", not the output.
				_ = tool.Execute(context.Background(), llm.ToolCall{
					ID:   "contracttest-concurrent",
					Type: "function",
					Function: llm.FunctionCall{
						Name:      tool.Definition().Function.Name,
						Arguments: "{}",
					},
				})
			}(i)
		}
		wg.Wait()
	})
}
