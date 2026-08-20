package agent

import (
	"context"
	"testing"

	upagent "github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/contracttest"
)

// resettableTool is a minimal Resettable so the coding-side Reset
// forwarding (not part of kit's contract) can be observed.
type resettableTool struct {
	resets int
}

func (r *resettableTool) Definition() llm.ToolDef {
	return llm.ToolDef{Function: llm.FunctionDef{Name: "bash"}}
}
func (r *resettableTool) Execute(context.Context, llm.ToolCall) upagent.ToolResult {
	return upagent.ToolResult{}
}
func (r *resettableTool) Reset() { r.resets++ }

// codingWrappers lists every coding-side tool wrapper as a wrap func.
func codingWrappers(t *testing.T) map[string]func(kit.Tool) kit.Tool {
	t.Helper()
	grants := mkMatcher(t, "Bash", "")
	return map[string]func(kit.Tool) kit.Tool{
		"lifecycleAwareTool":  func(in kit.Tool) kit.Tool { return lifecycleAwareTool{Tool: in} },
		"commandApprovalGate": func(in kit.Tool) kit.Tool { return commandApprovalGate{inner: in} },
		"bashGrantGate":       func(in kit.Tool) kit.Tool { return bashGrantGate{inner: in, grants: grants} },
	}
}

// TestToolWrappers_KitContract runs kit's wrapper contract (ShellBinder,
// PromptContributor, Described) against every coding-side wrapper.
func TestToolWrappers_KitContract(t *testing.T) {
	for name, wrap := range codingWrappers(t) {
		t.Run(name, func(t *testing.T) { contracttest.ToolWrapper(t, wrap) })
	}
}

// TestToolWrappers_ForwardReset locks the coding-only Resettable
// forwarding: the per-run reset must reach the innermost tool.
func TestToolWrappers_ForwardReset(t *testing.T) {
	for name, wrap := range codingWrappers(t) {
		t.Run(name, func(t *testing.T) {
			inner := &resettableTool{}
			w := wrap(inner)
			r, ok := w.(Resettable)
			if !ok {
				t.Fatalf("%s does not implement Resettable", name)
			}
			r.Reset()
			if inner.resets != 1 {
				t.Fatalf("Reset not forwarded: inner resets = %d, want 1", inner.resets)
			}
		})
	}
}

// TestRegisteredEditFile_IsResettable guards the production path: the
// registered edit_file is lifecycle-wrapped, and the per-run reset in
// resetRunState must still reach EditFileTool.Reset.
func TestRegisteredEditFile_IsResettable(t *testing.T) {
	ag := New(&multiTurnProvider{}, stubWorkspace{}, nil)
	t.Cleanup(ag.Close)
	tool, ok := ag.tools["edit_file"]
	if !ok {
		t.Fatal("edit_file not registered")
	}
	if _, isWrapped := tool.(lifecycleAwareTool); !isWrapped {
		t.Fatalf("edit_file registered as %T, want lifecycleAwareTool", tool)
	}
	if _, ok := tool.(Resettable); !ok {
		t.Fatalf("registered edit_file (%T) lost Resettable through the lifecycle wrapper", tool)
	}
}

// TestTools_IncludesMixedCaseExtraTool: registerTools keys a.tools by
// lower-cased name, so Tools() must look up the same way or mixed-case
// MCP/skill tools silently vanish from introspection.
func TestTools_IncludesMixedCaseExtraTool(t *testing.T) {
	extra := stubTool{name: "MyMcpTool"}
	ag := New(&multiTurnProvider{}, stubWorkspace{}, nil, extra)
	t.Cleanup(ag.Close)
	for _, tool := range ag.Tools() {
		if tool.Definition().Function.Name == "MyMcpTool" {
			return
		}
	}
	t.Fatalf("Tools() dropped mixed-case extra tool; got %d tools", len(ag.Tools()))
}
