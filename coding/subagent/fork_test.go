package subagent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/agent"
	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/coding/headless"
	"github.com/latebit-io/nib/kit/skill"
)

func TestAdaptForkSkill_SpawnsBodyAsTask(t *testing.T) {
	t.Parallel()
	var gotTools []agent.Tool
	var gotGoal string
	s := &Spawner{
		provider:  func() llm.Provider { return stubProvider{} },
		baseTools: []agent.Tool{fakeTool{"Read"}, fakeTool{"Write"}},
		run: func(_ context.Context, _ llm.Provider, _ *headless.DiskWorkspace, _ *agent.NewOptions, tools []agent.Tool, goal string, _ func(event.Event)) (headless.Result, error) {
			gotTools, gotGoal = tools, goal
			return headless.Result{Success: true, Summary: "forked-result"}, nil
		},
	}
	sk := skill.Skill{
		Name:        "f",
		Description: "forks",
		Body:        "do the work",
		Context:     "fork",
		Source:      skill.SourceProject,
	}

	tool := AdaptForkSkill(sk, s)
	if tool.Definition().Function.Name != "skill_f" {
		t.Errorf("fork skill tool name = %q, want skill_f", tool.Definition().Function.Name)
	}
	// No arguments — the body is the task.
	if len(tool.Definition().Function.Parameters.Properties) != 0 {
		t.Errorf("fork skill tool should take no arguments")
	}

	out := tool.Execute(context.Background(), llm.ToolCall{})
	if out.IsError || out.Content != "forked-result" {
		t.Errorf("execute = %+v", out)
	}
	// The skill body is the child's goal (persona empty); an unrestricted
	// fork skill inherits the base tools.
	if gotGoal != "do the work" {
		t.Errorf("child goal = %q, want the skill body", gotGoal)
	}
	if want := []string{"Read", "Write"}; !slices.Equal(toolNames(gotTools), want) {
		t.Errorf("child tools = %v, want %v", toolNames(gotTools), want)
	}
}

func TestForkDefinition_MapsProvenance(t *testing.T) {
	t.Parallel()
	d := forkDefinition(skill.Skill{
		Name: "x", Description: "d", Body: "b",
		AllowedTools: []string{"Read"}, DisallowedTools: []string{"Write"},
		Source: skill.SourcePlugin, PluginID: "demo",
	})
	if d.SystemPrompt != "" {
		t.Errorf("persona must be empty (body is the task), got %q", d.SystemPrompt)
	}
	if string(d.Source) != "plugin" || d.PluginID != "demo" {
		t.Errorf("provenance not mapped: %+v", d)
	}
	if !d.Permissions().GrantsTool("Read") || d.Permissions().DeniesTool("Read") {
		t.Errorf("grants not carried")
	}
}

// TestAdaptForkSkill_ChildCanReadBundledFiles: a fork skill that bundles
// files hands its child a reader for them, through the grant filter.
func TestAdaptForkSkill_ChildCanReadBundledFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "references", "guide.md"), []byte("guide"), 0o644); err != nil {
		t.Fatal(err)
	}
	var gotTools []agent.Tool
	var gotGoal string
	s := &Spawner{
		provider:  func() llm.Provider { return stubProvider{} },
		baseTools: []agent.Tool{fakeTool{"Read"}, fakeTool{"Write"}},
		run: func(_ context.Context, _ llm.Provider, _ *headless.DiskWorkspace, _ *agent.NewOptions, tools []agent.Tool, goal string, _ func(event.Event)) (headless.Result, error) {
			gotTools, gotGoal = tools, goal
			return headless.Result{Success: true, Summary: "ok"}, nil
		},
	}
	sk := skill.Skill{
		Name:         "f",
		Description:  "forks",
		Body:         "do the work",
		Context:      "fork",
		AllowedTools: []string{"Read"},
		Dir:          dir,
		Source:       skill.SourceGlobal,
	}
	if out := AdaptForkSkill(sk, s).Execute(context.Background(), llm.ToolCall{}); out.IsError {
		t.Fatalf("execute = %+v", out)
	}
	if !strings.Contains(gotGoal, "<file>references/guide.md</file>") || !strings.Contains(gotGoal, skill.ResourceToolName) {
		t.Errorf("child goal lacks the bundled-file note: %q", gotGoal)
	}
	if want := []string{"Read", skill.ResourceToolName}; !slices.Equal(toolNames(gotTools), want) {
		t.Fatalf("child tools = %v, want %v", toolNames(gotTools), want)
	}
	call := llm.ToolCall{Function: llm.FunctionCall{Arguments: `{"file":"references/guide.md"}`}}
	if res := gotTools[1].Execute(context.Background(), call); res.IsError || res.Content != "guide" {
		t.Errorf("child read = %+v", res)
	}
}
