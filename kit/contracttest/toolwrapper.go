package contracttest

import (
	"context"
	"reflect"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/dyncontext"
)

// ToolWrapper asserts that wrap forwards every optional interface kit
// defines on a [kit.Tool] — [kit.ShellBinder], [kit.PromptContributor]
// and [kit.Described] — from the wrapped tool through the wrapper. A
// decorator or gate that drops one of these silently disables the
// feature for the tools it wraps, which no unit test of the wrapper's
// own behaviour would notice.
//
// wrap receives a fixture tool implementing all optional interfaces and
// must return the wrapped [kit.Tool]. Add a subtest here whenever kit
// gains a new optional tool interface.
func ToolWrapper(t *testing.T, wrap func(kit.Tool) kit.Tool) {
	t.Helper()
	if wrap == nil {
		t.Fatal("contracttest.ToolWrapper: wrap must be non-nil")
	}

	t.Run("ForwardsPromptContributor", func(t *testing.T) {
		inner := &fullTool{guidelines: []string{"g1", "g2"}}
		got := kit.ToolPromptGuidelines(wrap(inner))
		if !reflect.DeepEqual(got, inner.guidelines) {
			t.Fatalf("contract: wrapper dropped kit.PromptContributor: got %v, want %v", got, inner.guidelines)
		}
	})

	t.Run("ForwardsDescribed", func(t *testing.T) {
		inner := &fullTool{plugin: kit.Plugin{Name: "full_tool", Version: "9.9.9", Source: "contracttest"}}
		got := kit.DescribeTool(wrap(inner))
		if got.Version != inner.plugin.Version || got.Source != inner.plugin.Source {
			t.Fatalf("contract: wrapper dropped kit.Described: got %+v, want version=%q source=%q", got, inner.plugin.Version, inner.plugin.Source)
		}
	})

	t.Run("ForwardsShellBinder", func(t *testing.T) {
		inner := &fullTool{}
		wrapped := wrap(inner)
		rebound := kit.BindToolShell(wrapped, contractRunner{})
		if _, ok := wrapped.(kit.ShellBinder); !ok {
			t.Fatal("contract: wrapper does not implement kit.ShellBinder")
		}
		if inner.bound != nil {
			t.Fatal("contract: BindShell must not mutate the wrapped tool")
		}
		// The rebound tool must still be wrapped and its inner must have
		// been rebound: Execute through it reports the runner it saw.
		res := rebound.Execute(context.Background(), llm.ToolCall{})
		if res.Content != boundMarker {
			t.Fatalf("contract: rebound wrapper did not rebind inner tool (Execute content %q)", res.Content)
		}
	})
}

// boundMarker is what fullTool.Execute returns once BindShell ran.
const boundMarker = "bound"

// fullTool implements every optional kit tool interface so a wrapper's
// forwarding can be observed end to end.
type fullTool struct {
	guidelines []string
	plugin     kit.Plugin
	bound      dyncontext.Runner
}

func (f *fullTool) Definition() llm.ToolDef {
	return llm.ToolDef{Type: "function", Function: llm.FunctionDef{Name: "full_tool", Description: "fixture"}}
}

func (f *fullTool) Execute(context.Context, llm.ToolCall) kit.ToolResult {
	if f.bound != nil {
		return kit.ToolResult{Content: boundMarker}
	}
	return kit.ToolResult{Content: "unbound"}
}

func (f *fullTool) PromptGuidelines() []string { return f.guidelines }

func (f *fullTool) Describe() kit.Plugin { return f.plugin }

func (f *fullTool) BindShell(r dyncontext.Runner) kit.Tool {
	return &fullTool{guidelines: f.guidelines, plugin: f.plugin, bound: r}
}

type contractRunner struct{}

func (contractRunner) Run(context.Context, string) (string, error) { return "", nil }
