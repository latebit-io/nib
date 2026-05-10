package kit_test

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/command"
)

// plainTool implements kit.Tool but not kit.Described. DescribeTool
// must derive metadata from its Definition().
type plainTool struct {
	name string
	desc string
}

func (t plainTool) Definition() llm.ToolDef {
	return llm.ToolDef{Function: llm.FunctionDef{Name: t.name, Description: t.desc}}
}

func (t plainTool) Execute(_ context.Context, _ llm.ToolCall) kit.ToolResult {
	return kit.ToolResult{}
}

// describedTool implements both kit.Tool and kit.Described. DescribeTool
// must return the Described override (with Kind forced to KindTool).
type describedTool struct {
	plainTool
	override kit.Plugin
}

func (t describedTool) Describe() kit.Plugin { return t.override }

// plainCommand implements command.Command without Described override.
type plainCommand struct {
	def command.Definition
}

func (c plainCommand) Definition() command.Definition { return c.def }

// describedCommand implements command.Command + kit.Described.
type describedCommand struct {
	plainCommand
	override kit.Plugin
}

func (c describedCommand) Describe() kit.Plugin { return c.override }

// --- DescribeTool ---

func TestDescribeTool_DerivesFromDefinition(t *testing.T) {
	t.Parallel()
	got := kit.DescribeTool(plainTool{name: "bash", desc: "Run a shell command"})
	want := kit.Plugin{
		Kind:        kit.KindTool,
		Name:        "bash",
		Description: "Run a shell command",
		Source:      "builtin",
	}
	if got != want {
		t.Errorf("DescribeTool = %+v, want %+v", got, want)
	}
}

func TestDescribeTool_TruncatesToFirstSentence(t *testing.T) {
	t.Parallel()
	// Tool definitions are LLM-facing prompts: a leading summary
	// followed by detailed instructions. --plugins listings want
	// only the summary.
	full := "Execute a shell command. Use this to verify edits compile or run tests. Do NOT use for destructive operations."
	got := kit.DescribeTool(plainTool{name: "bash", desc: full})
	want := "Execute a shell command."
	if got.Description != want {
		t.Errorf("Description = %q, want %q", got.Description, want)
	}
}

func TestDescribeTool_NoTrailingPeriodReturnsAll(t *testing.T) {
	t.Parallel()
	got := kit.DescribeTool(plainTool{name: "x", desc: "no terminator"})
	if got.Description != "no terminator" {
		t.Errorf("Description = %q, want %q", got.Description, "no terminator")
	}
}

func TestDescribeTool_PeriodMidWord(t *testing.T) {
	t.Parallel()
	// e.g. acronyms or version numbers — period not followed by
	// whitespace should NOT split.
	got := kit.DescribeTool(plainTool{name: "x", desc: "Use v1.2.3 of the library. Then run tests."})
	want := "Use v1.2.3 of the library."
	if got.Description != want {
		t.Errorf("Description = %q, want %q", got.Description, want)
	}
}

func TestDescribeTool_DescribedOverrideWins(t *testing.T) {
	t.Parallel()
	override := kit.Plugin{
		Kind:        kit.KindCommand, // wrong kind — DescribeTool must force KindTool
		Name:        "fancy",
		Description: "External plug-in tool",
		Version:     "1.2.0",
		Source:      "module:foo/bar",
	}
	got := kit.DescribeTool(describedTool{
		plainTool: plainTool{name: "ignored", desc: "ignored"},
		override:  override,
	})

	if got.Kind != kit.KindTool {
		t.Errorf("Kind = %q, want %q (DescribeTool must force KindTool)", got.Kind, kit.KindTool)
	}
	if got.Name != "fancy" {
		t.Errorf("Name = %q, want %q (Described should win over Definition)", got.Name, "fancy")
	}
	if got.Version != "1.2.0" {
		t.Errorf("Version = %q, want %q", got.Version, "1.2.0")
	}
	if got.Source != "module:foo/bar" {
		t.Errorf("Source = %q, want %q", got.Source, "module:foo/bar")
	}
}

// --- DescribeCommand ---

func TestDescribeCommand_DerivesFromDefinition(t *testing.T) {
	t.Parallel()
	cmd := plainCommand{def: command.Definition{
		Name:        "clear",
		Description: "Reset agent history",
		Source:      command.Source{Kind: command.SourceBuiltin},
	}}
	got := kit.DescribeCommand(cmd)
	want := kit.Plugin{
		Kind:        kit.KindCommand,
		Name:        "clear",
		Description: "Reset agent history",
		Source:      "builtin",
	}
	if got != want {
		t.Errorf("DescribeCommand = %+v, want %+v", got, want)
	}
}

func TestDescribeCommand_RendersSourceKindAndPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		source command.Source
		want   string
	}{
		{"project with path", command.Source{Kind: command.SourceProject, Path: ".project/commands/foo.md"}, "project:.project/commands/foo.md"},
		{"global no path", command.Source{Kind: command.SourceGlobal}, "global"},
		{"mcp with server", command.Source{Kind: command.SourceMCP, Path: "demarkus"}, "mcp:demarkus"},
		{"builtin", command.Source{Kind: command.SourceBuiltin}, "builtin"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := kit.DescribeCommand(plainCommand{def: command.Definition{
				Name:   "x",
				Source: tc.source,
			}})
			if got.Source != tc.want {
				t.Errorf("Source = %q, want %q", got.Source, tc.want)
			}
		})
	}
}

func TestDescribeCommand_DescribedOverrideWins(t *testing.T) {
	t.Parallel()
	override := kit.Plugin{
		Kind:        kit.KindTool, // wrong kind — DescribeCommand must force KindCommand
		Name:        "custom",
		Description: "External markdown command",
		Source:      "project:.project/commands/custom.md",
	}
	got := kit.DescribeCommand(describedCommand{
		plainCommand: plainCommand{def: command.Definition{Name: "ignored"}},
		override:     override,
	})
	if got.Kind != kit.KindCommand {
		t.Errorf("Kind = %q, want %q", got.Kind, kit.KindCommand)
	}
	if got.Name != "custom" {
		t.Errorf("Name = %q, want %q", got.Name, "custom")
	}
}

// --- RenderPlugins ---

func TestRenderPlugins_GroupsAndOrders(t *testing.T) {
	t.Parallel()
	plugins := []kit.Plugin{
		{Kind: kit.KindCommand, Name: "help", Description: "Show available commands", Source: "builtin"},
		{Kind: kit.KindTool, Name: "search", Description: "Project search", Source: "builtin"},
		{Kind: kit.KindProvider, Name: "openrouter", Description: "google/gemini-2.5-flash"},
		{Kind: kit.KindStore, Name: "demarkus", Description: "Mark Protocol versioned memory"},
		{Kind: kit.KindTool, Name: "bash", Description: "Run a shell command", Source: "builtin"},
		{Kind: kit.KindCommand, Name: "clear", Description: "Reset agent history", Source: "builtin"},
	}

	out := kit.RenderPlugins(plugins)

	// Group order: PROVIDER → STORE → TOOL → COMMAND.
	wantOrder := []string{"PROVIDER", "STORE", "TOOL", "COMMAND"}
	idx := 0
	for _, header := range wantOrder {
		pos := strings.Index(out[idx:], header)
		if pos < 0 {
			t.Fatalf("missing group header %q in output:\n%s", header, out)
		}
		idx += pos + len(header)
	}

	// Within TOOL: bash sorts before search.
	bashIdx := strings.Index(out, "bash")
	searchIdx := strings.Index(out, "search")
	if bashIdx <= 0 || searchIdx <= 0 || bashIdx >= searchIdx {
		t.Errorf("expected bash before search in tool group, got bash=%d search=%d", bashIdx, searchIdx)
	}

	// Within COMMAND: leading slash applied.
	if !strings.Contains(out, "/clear") || !strings.Contains(out, "/help") {
		t.Errorf("expected leading slash on commands; got:\n%s", out)
	}

	// builtin Source not rendered as a [tag].
	if strings.Contains(out, "[builtin]") {
		t.Errorf("builtin Source should be elided; got:\n%s", out)
	}
}

func TestRenderPlugins_NonBuiltinSourceRendered(t *testing.T) {
	t.Parallel()
	plugins := []kit.Plugin{
		{Kind: kit.KindCommand, Name: "custom", Description: "User command", Source: "project:.project/commands/custom.md"},
	}
	out := kit.RenderPlugins(plugins)
	if !strings.Contains(out, "[project:.project/commands/custom.md]") {
		t.Errorf("expected source tag in output; got:\n%s", out)
	}
}

func TestRenderPlugins_VersionAppended(t *testing.T) {
	t.Parallel()
	plugins := []kit.Plugin{
		{Kind: kit.KindTool, Name: "fancy", Description: "External tool", Version: "1.2.0"},
	}
	out := kit.RenderPlugins(plugins)
	if !strings.Contains(out, "fancy@1.2.0") {
		t.Errorf("expected name@version in output; got:\n%s", out)
	}
}

func TestRenderPlugins_EmptyInputProducesEmptyOutput(t *testing.T) {
	t.Parallel()
	if out := kit.RenderPlugins(nil); out != "" {
		t.Errorf("expected empty output for nil plugins; got %q", out)
	}
}

func TestRenderPlugins_EmptyGroupsOmitted(t *testing.T) {
	t.Parallel()
	plugins := []kit.Plugin{
		{Kind: kit.KindTool, Name: "bash", Description: "Run shell"},
	}
	out := kit.RenderPlugins(plugins)
	for _, header := range []string{"PROVIDER", "STORE", "COMMAND"} {
		if strings.Contains(out, header) {
			t.Errorf("expected %q group to be omitted; got:\n%s", header, out)
		}
	}
	if !strings.Contains(out, "TOOL (1)") {
		t.Errorf("expected TOOL group with count; got:\n%s", out)
	}
}

// --- Property: built-in kit tools survive DescribeTool with non-empty fields ---

func TestDescribeTool_BuiltinToolsHaveCompleteMetadata(t *testing.T) {
	t.Parallel()
	// Use a representative built-in fake — full integration with
	// kit/tools/* is exercised in their own packages. The property
	// here is that *any* tool returning a non-empty Definition
	// produces a Plugin with non-empty Name and Description.
	tools := []kit.Tool{
		plainTool{name: "bash", desc: "Run shell"},
		plainTool{name: "search", desc: "Project search"},
		plainTool{name: "memory_fetch", desc: "Fetch a memory document"},
	}
	for _, tl := range tools {
		p := kit.DescribeTool(tl)
		if p.Name == "" {
			t.Errorf("DescribeTool(%q) Name empty", tl.Definition().Function.Name)
		}
		if p.Description == "" {
			t.Errorf("DescribeTool(%q) Description empty", tl.Definition().Function.Name)
		}
		if p.Kind != kit.KindTool {
			t.Errorf("DescribeTool(%q) Kind = %q, want %q", tl.Definition().Function.Name, p.Kind, kit.KindTool)
		}
	}
}
